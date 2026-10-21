package qga

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const ifaces = `{"return":[
 {"name":"lo","hardware-address":"00:00:00:00:00:00","ip-addresses":[{"ip-address-type":"ipv4","ip-address":"127.0.0.1","prefix":8}]},
 {"name":"enp0s2","hardware-address":"52:54:00:AB:CD:EF","ip-addresses":[
  {"ip-address-type":"ipv6","ip-address":"fe80::5054:ff:feab:cdef","prefix":64},
  {"ip-address-type":"ipv6","ip-address":"fd00::5","prefix":64},
  {"ip-address-type":"ipv4","ip-address":"192.168.1.50","prefix":24}]},
 {"name":"docker0","hardware-address":"02:42:ac:11:00:01","ip-addresses":[{"ip-address-type":"ipv4","ip-address":"172.17.0.1","prefix":16}]}]}`

// fakeAgent answers the sync and then one command with reply, after sending stale first.
func fakeAgent(t *testing.T, c net.Conn, stale, reply string) {
	t.Helper()
	defer c.Close()
	r := bufio.NewReader(c)
	if b, err := r.ReadByte(); err != nil || b != 0xff {
		t.Errorf("the client did not flush with 0xff first: %v %x", err, b)
		return
	}
	var sync struct {
		Execute   string
		Arguments struct{ ID int64 }
	}
	line, _ := r.ReadBytes('\n')
	if err := json.Unmarshal(line, &sync); err != nil || sync.Execute != "guest-sync-delimited" {
		t.Errorf("first command = %q, want guest-sync-delimited", line)
		return
	}
	fmt.Fprintf(c, "%s\xff{\"return\": %d}\n", stale, sync.Arguments.ID)
	if line, _ = r.ReadBytes('\n'); !strings.Contains(string(line), "guest-network-get-interfaces") {
		t.Errorf("second command = %q", line)
	}
	fmt.Fprint(c, reply)
}

func serve(t *testing.T, stale, reply string) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "qga.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		if c, err := l.Accept(); err == nil {
			fakeAgent(t, c, stale, reply)
		}
	}()
	return sock
}

func TestAddressesAreTheBlockInterfacesIPv4First(t *testing.T) {
	sock := serve(t, "", ifaces)
	got, err := Addresses(context.Background(), sock, "52:54:00:ab:cd:ef")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"192.168.1.50", "fd00::5"}; !slices.Equal(got, want) {
		t.Errorf("Addresses = %q, want %q", got, want)
	}
}

func TestAddressesSkipStaleReplies(t *testing.T) {
	sock := serve(t, "{\"return\": {}}\n\xff{\"return\": 7}\n", ifaces)
	if got, err := Addresses(context.Background(), sock, "52:54:00:ab:cd:ef"); err != nil || len(got) != 2 {
		t.Errorf("Addresses = %q, %v", got, err)
	}
}

func TestAddressesReportTheAgentsError(t *testing.T) {
	sock := serve(t, "", `{"error":{"class":"CommandDisabled","desc":"disabled"}}`)
	if _, err := Addresses(context.Background(), sock, "52:54:00:ab:cd:ef"); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("err = %v, want the agent's error", err)
	}
}

func TestAddressesGiveUpWhenNoAgentAnswers(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "qga.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		if c, err := l.Accept(); err == nil {
			defer c.Close()
			time.Sleep(2 * time.Second)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := Addresses(ctx, sock, "52:54:00:ab:cd:ef"); err == nil || time.Since(start) > time.Second {
		t.Errorf("err = %v after %v, want a prompt error", err, time.Since(start))
	}
}
