// The LAN address range blocks (and this interface) get their VIPs from: a cluster setting
// that overrides the nodes' --external-vip-pool.
package web

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/expanse/expanse/internal/network/vip"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
)

func (s *Server) registerNetworkRoutes() {
	s.routes.Handle("POST /cluster/network", s.requireAuth(http.HandlerFunc(s.handleNetworkSave)))
}

// networkView is the Settings → Network card.
type networkView struct {
	Setting  string // the cluster setting, "" if unset
	Fallback string // this node's --external-vip-pool
	Input    string // the text box: the setting, or rejected input
	Error    string
}

// InUse is the range VIPs come from; "" means cluster-internal addresses only.
func (n networkView) InUse() string {
	return vip.EffectivePool(vip.Settings{ExternalPool: n.Setting}, n.Fallback)
}

func (s *Server) networkView(ctx context.Context) networkView {
	set, _ := vip.LoadSettings(store.WithStale(ctx), s.store)
	return networkView{Setting: set.ExternalPool, Fallback: s.VIPPoolFallback, Input: set.ExternalPool}
}

// lanPool is the range in effect; "" means blocks are reachable inside the cluster only.
func (s *Server) lanPool(ctx context.Context) string { return s.networkView(ctx).InUse() }

// nodeAddrs lists the nodes' LAN addresses from their Raft and API addresses.
func (s *Server) nodeAddrs(ctx context.Context) []netip.Addr {
	entries, err := s.store.List(store.WithStale(ctx), "/nodes/")
	if err != nil {
		return nil
	}
	var out []netip.Addr
	for _, e := range entries {
		if strings.Contains(strings.TrimPrefix(string(e.Key), "/nodes/"), "/") {
			continue
		}
		var rec struct {
			RaftAddr string `json:"raft_addr"`
			APIAddr  string `json:"api_addr"`
		}
		if json.Unmarshal(e.Value, &rec) != nil {
			continue
		}
		for _, hp := range []string{rec.RaftAddr, rec.APIAddr} {
			host, _, err := net.SplitHostPort(hp)
			if a, perr := netip.ParseAddr(host); err == nil && perr == nil && !a.IsUnspecified() {
				out = append(out, a.Unmap())
			}
		}
	}
	return out
}

func (s *Server) handleNetworkSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	pool := strings.TrimSpace(r.PostForm.Get("externalVIPPool"))
	fail := func(msg string) {
		d := s.clusterSettingsData(w, r)
		d.Network.Input, d.Network.Error = pool, msg
		s.render(w, http.StatusBadRequest, "cluster_settings.html", d)
	}
	if pool != "" {
		if err := vip.CheckPool(pool, s.nodeAddrs(r.Context())); err != nil {
			fail(sentence(errText(err)))
			return
		}
	}
	if err := vip.SaveSettings(r.Context(), s.store, vip.Settings{ExternalPool: pool}); err != nil {
		fail(errText(err))
		return
	}
	msg := "LAN address range saved. Block addresses move into it within a few seconds."
	if pool == "" {
		msg = "LAN address range cleared."
	}
	s.done(w, r, "success", msg, "/cluster/settings#network")
}

// sentence capitalizes an error message for display.
func sentence(msg string) string {
	if msg == "" {
		return msg
	}
	r, n := utf8.DecodeRuneInString(msg)
	return string(unicode.ToUpper(r)) + msg[n:] + "."
}

// hasVIP reports whether b asks for its own address.
func hasVIP(b *pb.Block) bool {
	for _, p := range b.GetSpec().GetNetwork().GetPorts() {
		if p.GetExpose() == pb.Expose_EXPOSE_VIP {
			return true
		}
	}
	return false
}
