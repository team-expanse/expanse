package web

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/coder/websocket"
	"google.golang.org/grpc/status"

	pb "github.com/expanse/expanse/proto"
)

// consoleCSP is the page CSP with inline styles allowed: xterm.js adds its own <style> elements.
const consoleCSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'"

// consoleReadLimit bounds one message of keystrokes or paste from the browser.
const consoleReadLimit = 64 << 10

type blockConsoleData struct {
	Page      page
	Namespace string
	Name      string
	Replica   int32
	Replicas  []int32
	Node      string // the node id the replica runs on, "" when it isn't placed
}

func (s *Server) registerConsoleRoutes() {
	s.routes.Handle("GET /blocks/{ns}/{name}/console", s.requireAuth(http.HandlerFunc(s.handleBlockConsole)))
	s.routes.Handle("GET /blocks/{ns}/{name}/console/socket", s.requireAuth(http.HandlerFunc(s.handleBlockConsoleSocket)))
}

// handleBlockConsole is a VM's console page: a terminal that attaches over the console socket.
func (s *Server) handleBlockConsole(w http.ResponseWriter, r *http.Request) {
	if s.blocksUnavailable(w, r) {
		return
	}
	b, err := s.getBlock(r.Context(), r)
	if err != nil {
		s.renderError(w, r, httpStatus(err), errText(err))
		return
	}
	if b.GetSpec().GetType() != "vm/instance" {
		s.renderError(w, r, http.StatusNotFound, "Only VM blocks have a console.")
		return
	}
	replica, _ := strconv.ParseInt(r.URL.Query().Get("replica"), 10, 32)
	ns, name := r.PathValue("ns"), r.PathValue("name")
	full := ns + "/" + name
	data := blockConsoleData{
		Page:      s.newPage(w, r, full+" console", "blocks", crumb{Label: "Blocks", Href: "/blocks"}, crumb{Label: full, Href: "/blocks/" + full}, crumb{Label: "Console"}),
		Namespace: ns, Name: name, Replica: int32(replica), Replicas: logReplicas(b),
	}
	for _, p := range b.GetStatus().GetPlacements() {
		if p.GetReplicaIndex() == data.Replica {
			data.Node = p.GetNodeId()
		}
	}
	w.Header().Set("Content-Security-Policy", consoleCSP)
	s.render(w, http.StatusOK, "block_console.html", data)
}

// handleBlockConsoleSocket relays a WebSocket to the VM's console, wherever it runs.
func (s *Server) handleBlockConsoleSocket(w http.ResponseWriter, r *http.Request) {
	if s.Console == nil {
		http.Error(w, "VM consoles are not available on this node", http.StatusServiceUnavailable)
		return
	}
	replica, _ := strconv.ParseInt(r.URL.Query().Get("replica"), 10, 32)
	// Accept refuses a cross-origin page: the session cookie alone must not open a console.
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(consoleReadLimit)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stream, err := s.Console.Attach(ctx)
	if err == nil {
		err = stream.Send(&pb.ConsoleInput{Namespace: r.PathValue("ns"), Name: r.PathValue("name"), Replica: int32(replica)})
	}
	if err != nil {
		_ = conn.Close(websocket.StatusTryAgainLater, closeReason(err))
		return
	}
	go func() {
		defer cancel()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if err := stream.Send(&pb.ConsoleInput{Data: data}); err != nil {
				return
			}
		}
	}()
	for {
		out, err := stream.Recv()
		if err != nil {
			if ctx.Err() == nil {
				_ = conn.Close(websocket.StatusNormalClosure, closeReason(err))
			}
			return
		}
		if err := conn.Write(ctx, websocket.MessageBinary, out.GetData()); err != nil {
			return
		}
	}
}

// closeReason is err's message cut to what a WebSocket close frame carries; a clean end has none.
func closeReason(err error) string {
	st, ok := status.FromError(err)
	if !ok || st.Message() == "" || st.Code() == 0 {
		return ""
	}
	msg := strings.TrimPrefix(st.Message(), "console: ")
	if len(msg) > 120 {
		msg = strings.ToValidUTF8(msg[:117], "") + "..."
	}
	return msg
}
