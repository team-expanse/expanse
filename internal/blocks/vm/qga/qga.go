// Package qga asks a VM's QEMU guest agent, over its virtio-serial socket, for the guest's addresses.
package qga

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/netip"
	"strings"
	"time"
)

// QEMUArgs give the guest a virtio-serial port named for the guest agent, served on sock.
func QEMUArgs(sock string) []string {
	return []string{
		"-device", "virtio-serial",
		"-chardev", "socket,id=qga0,path=" + sock + ",server=on,wait=off",
		"-device", "virtserialport,chardev=qga0,name=org.qemu.guest_agent.0",
	}
}

// timeout bounds one exchange when ctx has no earlier deadline; a guest without an agent never answers.
const timeout = 5 * time.Second

// Addresses are the guest's global addresses on the interface with hardware address mac, IPv4 first.
func Addresses(ctx context.Context, sock, mac string) ([]string, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = c.SetDeadline(deadline)
	r := bufio.NewReader(c)
	if err := sync(c, r, rand.Int64N(1<<31)); err != nil {
		return nil, fmt.Errorf("guest agent sync: %w", err)
	}
	var ifaces []iface
	if err := call(c, r, "guest-network-get-interfaces", &ifaces); err != nil {
		return nil, err
	}
	return pick(ifaces, mac), nil
}

// sync flushes the agent's parser and skips replies left from earlier clients.
func sync(w io.Writer, r *bufio.Reader, id int64) error {
	if _, err := fmt.Fprintf(w, "\xff{\"execute\":\"guest-sync-delimited\",\"arguments\":{\"id\":%d}}\n", id); err != nil {
		return err
	}
	for {
		if _, err := r.ReadBytes(0xff); err != nil {
			return err
		}
		reply, err := r.ReadBytes('}')
		if err != nil {
			return err
		}
		var v struct{ Return int64 }
		if json.Unmarshal(reply, &v) == nil && v.Return == id {
			return nil
		}
	}
}

func call(w io.Writer, r io.Reader, cmd string, out any) error {
	if _, err := fmt.Fprintf(w, "{\"execute\":%q}\n", cmd); err != nil {
		return err
	}
	var reply struct {
		Return json.RawMessage
		Error  *struct{ Class, Desc string }
	}
	if err := json.NewDecoder(r).Decode(&reply); err != nil {
		return fmt.Errorf("guest agent %s: %w", cmd, err)
	}
	if reply.Error != nil {
		return fmt.Errorf("guest agent %s: %s: %s", cmd, reply.Error.Class, reply.Error.Desc)
	}
	if reply.Return == nil {
		return errors.New("guest agent " + cmd + ": empty reply")
	}
	return json.Unmarshal(reply.Return, out)
}

type iface struct {
	HWAddr string `json:"hardware-address"`
	IPs    []struct {
		Addr string `json:"ip-address"`
	} `json:"ip-addresses"`
}

func pick(ifaces []iface, mac string) []string {
	var v4, v6 []string
	for _, f := range ifaces {
		if !strings.EqualFold(f.HWAddr, mac) {
			continue
		}
		for _, ip := range f.IPs {
			a, err := netip.ParseAddr(ip.Addr)
			switch {
			case err != nil || a.IsLinkLocalUnicast() || a.IsLoopback():
			case a.Is4():
				v4 = append(v4, a.String())
			default:
				v6 = append(v6, a.String())
			}
		}
	}
	return append(v4, v6...)
}
