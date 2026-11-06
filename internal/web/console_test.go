package web

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/expanse/expanse/internal/web/schemaform"
	pb "github.com/expanse/expanse/proto"
)

// echoConsole greets each session with a prompt and echoes its input back.
type echoConsole struct {
	pb.UnimplementedConsoleServiceServer
	mu    sync.Mutex
	first *pb.ConsoleInput
	fail  error
}

func (e *echoConsole) Attach(stream pb.ConsoleService_AttachServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.first = first
	e.mu.Unlock()
	if e.fail != nil {
		return e.fail
	}
	if err := stream.Send(&pb.ConsoleOutput{Data: []byte("login: ")}); err != nil {
		return err
	}
	for {
		in, err := stream.Recv()
		if err != nil {
			return nil //nolint:nilerr // the client hung up
		}
		if err := stream.Send(&pb.ConsoleOutput{Data: in.GetData()}); err != nil {
			return err
		}
	}
}

func consoleClientFor(t *testing.T, e *echoConsole) pb.ConsoleServiceClient {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	pb.RegisterConsoleServiceServer(srv, e)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return pb.NewConsoleServiceClient(conn)
}

// consoleServer runs the UI with a VM block vm1 and a web block, consoles served by e.
func consoleServer(t *testing.T, e *echoConsole) (c *http.Client, base, csrf string) {
	t.Helper()
	console := consoleClientFor(t, e)
	srv, pw, st := newTestServerWith(t, func(s *Server) { s.Console = console })
	seedNode(t, st, "n1", "kvm")
	c, csrf = loggedInClient(t, srv, pw)
	resp, body := postForm(t, c, srv.URL+"/deploy/vm/instance", csrf, url.Values{
		"name": {"vm1"}, "cfg/image" + schemaform.PresetSuffix: {"Debian 13 (log in as debian)"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("deploy vm1: %d\n%s", resp.StatusCode, alerts(body))
	}
	if resp, body := postForm(t, c, srv.URL+"/blocks", csrf, url.Values{"manifest": {echoManifest}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("deploy web: %d\n%s", resp.StatusCode, alerts(body))
	}
	return c, srv.URL, csrf
}

func TestVMPagesHaveAConsoleTabAndOtherBlocksDont(t *testing.T) {
	c, base, _ := consoleServer(t, &echoConsole{})
	for _, path := range []string{"/blocks/default/vm1", "/blocks/default/vm1/logs", "/blocks/default/vm1/edit"} {
		if _, body := get(t, c, base+path); !strings.Contains(body, `href="/blocks/default/vm1/console"`) {
			t.Errorf("%s has no Console tab", path)
		}
	}
	if _, body := get(t, c, base+"/blocks/default/web"); strings.Contains(body, "/console") {
		t.Error("a web block's page offers a console")
	}
}

func TestConsolePageLoadsTheTerminal(t *testing.T) {
	c, base, _ := consoleServer(t, &echoConsole{})
	resp, err := c.Get(base + "/blocks/default/vm1/console")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	body := string(b)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for _, want := range []string{`data-socket="/blocks/default/vm1/console/socket?replica=0"`, `src="/static/xterm/xterm.js"`, `src="/static/console.js"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %s", want)
		}
	}
	// xterm.js adds <style> elements; scripts stay restricted to the embedded files.
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "style-src 'self' 'unsafe-inline'") || !strings.Contains(csp, "script-src 'self';") {
		t.Errorf("CSP = %q", csp)
	}
	if code, _ := get(t, c, base+"/blocks/default/web/console"); code != http.StatusNotFound {
		t.Errorf("a web block's console page: status %d, want 404", code)
	}
	for _, asset := range []string{"/static/xterm/xterm.js", "/static/xterm/addon-fit.js", "/static/xterm/xterm.css", "/static/console.js"} {
		if code, _ := get(t, c, base+asset); code != http.StatusOK {
			t.Errorf("%s: status %d", asset, code)
		}
	}
}

func dialConsole(t *testing.T, c *http.Client, base string, header http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	u := "ws" + strings.TrimPrefix(base, "http") + "/blocks/default/vm1/console/socket?replica=0"
	return websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: c, HTTPHeader: header})
}

func readText(t *testing.T, conn *websocket.Conn, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got := ""
	for !strings.Contains(got, want) {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read %q so far, want %q: %v", got, want, err)
		}
		got += string(data)
	}
}

func TestConsoleSocketRelaysToTheVM(t *testing.T) {
	e := &echoConsole{}
	c, base, _ := consoleServer(t, e)
	conn, _, err := dialConsole(t, c, base, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()
	readText(t, conn, "login: ")
	if err := conn.Write(context.Background(), websocket.MessageBinary, []byte("root\r")); err != nil {
		t.Fatal(err)
	}
	readText(t, conn, "root\r")
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.first.GetNamespace() != "default" || e.first.GetName() != "vm1" || e.first.GetReplica() != 0 {
		t.Errorf("attached %v, want default/vm1 replica 0", e.first)
	}
}

func TestConsoleSocketPassesOnWhyItClosed(t *testing.T) {
	c, base, _ := consoleServer(t, &echoConsole{fail: status.Error(codes.Unavailable, "console: the VM's console is not up on n1")})
	conn, _, err := dialConsole(t, c, base, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()
	_, _, err = conn.Read(context.Background())
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Reason != "the VM's console is not up on n1" {
		t.Fatalf("read error %v, want a close with the reason", err)
	}
}

// A page on another site can't open a console with the admin's cookie.
func TestConsoleSocketRefusesAnotherOrigin(t *testing.T) {
	c, base, _ := consoleServer(t, &echoConsole{})
	_, resp, err := dialConsole(t, c, base, http.Header{"Origin": {"https://evil.example"}})
	if err == nil {
		t.Fatal("a cross-origin console socket was accepted")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("response %v, want 403", resp)
	}
}

func TestConsoleSocketNeedsASession(t *testing.T) {
	_, base, _ := consoleServer(t, &echoConsole{})
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if _, _, err := dialConsole(t, noRedirect, base, nil); err == nil {
		t.Fatal("a console socket opened without signing in")
	}
}

func TestSigningOutClosesAnOpenConsole(t *testing.T) {
	c, base, csrf := consoleServer(t, &echoConsole{})
	conn, _, err := dialConsole(t, c, base, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()
	readText(t, conn, "login: ")
	req, _ := http.NewRequest(http.MethodPost, base+"/logout", nil)
	req.Header.Set(csrfHeader, csrf)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			if ctx.Err() != nil {
				t.Fatal("the console stayed open after signing out")
			}
			return
		}
	}
}
