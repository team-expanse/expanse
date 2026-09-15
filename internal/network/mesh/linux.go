//go:build linux

package mesh

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"

	"github.com/expanse/expanse/internal/errors"
	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// LinuxController is the real Controller over netlink + wgctrl.
// Incremental only: SetPeer/RemovePeer configure single peers (no
// ReplacePeers), and the private key is set via Config so wgctrl updates
// just the device attribute — never a full peer-set replace (§4.1).
type LinuxController struct {
	iface string
	port  int
	priv  wgtypes.Key
	wg    *wgctrl.Client
}

// NewLinuxController opens wgctrl bound to iface/port with the node's
// private key.
func NewLinuxController(iface string, port int, priv wgtypes.Key) (*LinuxController, error) {
	wg, err := wgctrl.New()
	if err != nil {
		return nil, errors.New(errors.KindUnavailable, "mesh.wgctrl", err.Error())
	}
	return &LinuxController{iface: iface, port: port, priv: priv, wg: wg}, nil
}

// Close releases the wgctrl client.
func (c *LinuxController) Close() error { return c.wg.Close() }

// EnsureDevice creates the WireGuard interface if missing and sets its
// MTU (and listen port / private key attributes).
func (c *LinuxController) EnsureDevice(mtu int) error {
	link, err := netlink.LinkByName(c.iface)
	if err != nil {
		l := &netlink.Wireguard{
			LinkAttrs: netlink.LinkAttrs{Name: c.iface, MTU: mtu},
		}
		if err := netlink.LinkAdd(l); err != nil {
			return errors.New(errors.KindUnavailable, "mesh.linkAdd",
				fmt.Sprintf("%s: %v", c.iface, err))
		}
		link, err = netlink.LinkByName(c.iface)
		if err != nil {
			return errors.New(errors.KindUnavailable, "mesh.linkGet", err.Error())
		}
	}
	if link.Attrs().OperState != netlink.OperUp {
		if err := netlink.LinkSetUp(link); err != nil {
			return errors.New(errors.KindUnavailable, "mesh.linkUp", err.Error())
		}
	}
	if link.Attrs().MTU != mtu {
		if err := netlink.LinkSetMTU(link, mtu); err != nil {
			return errors.New(errors.KindUnavailable, "mesh.setMTU", err.Error())
		}
	}
	return c.configure(&wgtypes.Config{
		PrivateKey:   &c.priv,
		ListenPort:   &c.port,
		ReplacePeers: false,
	})
}

// EnsureAddr idempotently assigns the overlay address to exp0.
func (c *LinuxController) EnsureAddr(addr netip.Addr) error {
	link, err := netlink.LinkByName(c.iface)
	if err != nil {
		return errors.New(errors.KindUnavailable, "mesh.linkGet", err.Error())
	}
	want := netlink.Addr{
		IPNet: &net.IPNet{
			IP:   addr.AsSlice(),
			Mask: net.CIDRMask(addr.BitLen(), addr.BitLen()),
		},
	}
	existing, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return errors.New(errors.KindUnavailable, "mesh.addrList", err.Error())
	}
	for _, a := range existing {
		if a.IP.Equal(want.IP) {
			return nil
		}
	}
	if err := netlink.AddrAdd(link, &want); err != nil {
		return errors.New(errors.KindUnavailable, "mesh.addrAdd", err.Error())
	}
	return nil
}

// Peers snapshots the device's peers into PeerState values.
func (c *LinuxController) Peers() ([]PeerState, error) {
	d, err := c.wg.Device(c.iface)
	if err != nil {
		return nil, errors.New(errors.KindUnavailable, "mesh.device", err.Error())
	}
	out := make([]PeerState, 0, len(d.Peers))
	for _, p := range d.Peers {
		st := PeerState{
			PublicKey: p.PublicKey.String(),
		}
		if p.Endpoint != nil {
			st.Endpoint = p.Endpoint.String()
		}
		for _, a := range p.AllowedIPs {
			ones, _ := a.Mask.Size()
			ip, _ := netip.AddrFromSlice(a.IP)
			st.AllowedIPs = append(st.AllowedIPs, netip.PrefixFrom(ip.Unmap(), ones))
		}
		out = append(out, st)
	}
	return out, nil
}

// SetPeer adds or updates exactly one peer (AllowedIPs replace that
// peer's set only).
func (c *LinuxController) SetPeer(spec PeerSpec) error {
	if spec.PublicKey == "" {
		return errors.New(errors.KindInvalid, "mesh.setPeer", "empty public key")
	}
	pub, err := wgtypes.ParseKey(spec.PublicKey)
	if err != nil {
		return errors.New(errors.KindInvalid, "mesh.setPeer", err.Error())
	}
	var eps *net.UDPAddr
	if spec.Endpoint != "" {
		host, portStr, splitErr := net.SplitHostPort(spec.Endpoint)
		if splitErr != nil {
			return errors.New(errors.KindInvalid, "mesh.setPeer", splitErr.Error())
		}
		port, portErr := strconv.Atoi(portStr)
		if portErr != nil {
			return errors.New(errors.KindInvalid, "mesh.setPeer", portErr.Error())
		}
		ips, lookupErr := net.LookupIP(host)
		if lookupErr != nil || len(ips) == 0 {
			return errors.New(errors.KindUnavailable, "mesh.lookup", lookupErr.Error())
		}
		eps = &net.UDPAddr{IP: ips[0], Port: port}
	}
	pc := wgtypes.PeerConfig{
		PublicKey:                   pub,
		Endpoint:                    eps,
		AllowedIPs:                  toIPNets(spec.AllowedIPs),
		PersistentKeepaliveInterval: &spec.Keepalive,
	}
	if pc.PersistentKeepaliveInterval != nil && spec.Keepalive == 0 {
		pc.PersistentKeepaliveInterval = nil
	}
	if err := c.configure(&wgtypes.Config{Peers: []wgtypes.PeerConfig{pc}, ReplacePeers: false}); err != nil {
		return err
	}
	// wgctrl handles crypto-key routing only; the kernel still needs
	// routes for the peer's prefixes (wg-quick's job, done here).
	return c.ensureRoutes(spec.AllowedIPs)
}

// ensureRoutes installs a route per prefix via exp0. RouteReplace is
// idempotent and updates an existing route's link in place — never a
// full table flush.
func (c *LinuxController) ensureRoutes(ps []netip.Prefix) error {
	link, err := netlink.LinkByName(c.iface)
	if err != nil {
		return errors.New(errors.KindUnavailable, "mesh.linkGet", err.Error())
	}
	for _, p := range ps {
		dst := toIPNet(p)
		route := netlink.Route{
			LinkIndex: link.Attrs().Index,
			Dst:       dst,
			Scope:     netlink.SCOPE_LINK,
		}
		if err := netlink.RouteReplace(&route); err != nil {
			return errors.New(errors.KindUnavailable, "mesh.routeReplace",
				p.String()+": "+err.Error())
		}
	}
	return nil
}

// RemovePeer deletes exactly one peer and its routes.
func (c *LinuxController) RemovePeer(publicKey string) error {
	pub, err := wgtypes.ParseKey(publicKey)
	if err != nil {
		return errors.New(errors.KindInvalid, "mesh.removePeer", err.Error())
	}
	// Capture the peer's prefixes first so the routes can go too.
	var doomed []netip.Prefix
	for _, p := range mustPeers(c) {
		if p.PublicKey == publicKey {
			doomed = p.AllowedIPs
		}
	}
	if err := c.configure(&wgtypes.Config{
		Peers:        []wgtypes.PeerConfig{{PublicKey: pub, Remove: true}},
		ReplacePeers: false,
	}); err != nil {
		return err
	}
	link, linkErr := netlink.LinkByName(c.iface)
	if linkErr != nil {
		return nil // interface gone; routes went with it
	}
	for _, p := range doomed {
		_ = netlink.RouteDel(&netlink.Route{
			LinkIndex: link.Attrs().Index,
			Dst:       toIPNet(p),
		})
	}
	return nil
}

// mustPeers is Peers() ignoring errors (used for prefix bookkeeping
// before a removal).
func mustPeers(c *LinuxController) []PeerState {
	ps, err := c.Peers()
	if err != nil {
		return nil
	}
	return ps
}

func (c *LinuxController) configure(cfg *wgtypes.Config) error {
	if err := c.wg.ConfigureDevice(c.iface, *cfg); err != nil {
		return errors.New(errors.KindUnavailable, "mesh.configure", err.Error())
	}
	return nil
}

func toIPNets(ps []netip.Prefix) []net.IPNet {
	out := make([]net.IPNet, 0, len(ps))
	for _, p := range ps {
		out = append(out, *toIPNet(p))
	}
	return out
}

func toIPNet(p netip.Prefix) *net.IPNet {
	ip := p.Addr()
	if ip.Is4In6() {
		ip = netip.AddrFrom4(ip.As4())
	}
	return &net.IPNet{
		IP:   ip.AsSlice(),
		Mask: net.CIDRMask(p.Bits(), ip.BitLen()),
	}
}

// detectPhysicalMTU picks the MTU of the interface carrying the default
// route (the node's LAN path); ignoring the mesh interface itself.
func detectPhysicalMTU(meshIface string) (int, error) {
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return 0, err
	}
	for _, r := range routes {
		if r.Dst == nil && r.LinkIndex != 0 { // default route
			link, linkErr := netlink.LinkByIndex(r.LinkIndex)
			if linkErr != nil {
				continue
			}
			if link.Attrs().Name == meshIface {
				continue
			}
			if link.Attrs().MTU > 0 {
				return link.Attrs().MTU, nil
			}
		}
	}
	return 0, fmt.Errorf("no default-route interface MTU found")
}
