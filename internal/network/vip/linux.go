package vip

import (
	stderrors "errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/expanse/expanse/internal/errors"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// LinuxSeams builds production Seams: netlink for address management
// and raw AF_PACKET sockets for gratuitous ARP.
//
// iface is the physical interface to announce on; "auto" resolves to
// the interface owning the default route (§4.2 externalInterface).
func LinuxSeams(iface string) (Seams, error) {
	link, err := resolveIface(iface)
	if err != nil {
		return Seams{}, err
	}
	return Seams{
		AddAddr:  func(p netip.Prefix) error { return addAddr(link, p) },
		DelAddr:  func(p netip.Prefix) error { return delAddr(link, p) },
		Announce: func(p netip.Prefix, n int) error { return announce(link, p, n) },
	}, nil
}

// ResolveIface maps an interface name, "auto" meaning the default
// route's device. Returns the link and its name.
func ResolveIface(iface string) (netlink.Link, string, error) {
	link, err := resolveIface(iface)
	if err != nil {
		return nil, "", err
	}
	return link, link.Attrs().Name, nil
}

func resolveIface(iface string) (netlink.Link, error) {
	if iface != "" && iface != "auto" {
		link, err := netlink.LinkByName(iface)
		if err != nil {
			return nil, errors.Wrap(err, errors.KindNotFound, "vip", "interface "+iface)
		}
		return link, nil
	}
	// auto: the default route's interface.
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return nil, errors.Wrap(err, errors.KindUnavailable, "vip", "list routes")
	}
	for _, r := range routes {
		if r.Dst == nil && r.LinkIndex != 0 {
			link, err := netlink.LinkByIndex(r.LinkIndex)
			if err != nil {
				continue
			}
			return link, nil
		}
	}
	return nil, errors.New(errors.KindNotFound, "vip", "no default route interface for auto detection")
}

func addrIPNet(p netip.Prefix) *net.IPNet {
	a := p.Addr().Unmap().AsSlice()
	return &net.IPNet{IP: a, Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen())}
}

func addAddr(link netlink.Link, p netip.Prefix) error {
	a := &netlink.Addr{IPNet: addrIPNet(p)}
	if err := netlink.AddrAdd(link, a); err != nil && !stderrors.Is(err, unix.EEXIST) {
		return err
	}
	return nil
}

func delAddr(link netlink.Link, p netip.Prefix) error {
	a := &netlink.Addr{IPNet: addrIPNet(p)}
	err := netlink.AddrDel(link, a)
	if err != nil && !stderrors.Is(err, unix.ENOENT) {
		// Address already gone is fine — that is the goal state.
		return err
	}
	return nil
}

// announce sends n gratuitous ARP replies (IPv4): broadcast Ethernet
// frames announcing "VIP is at my MAC". Switches and hosts update
// their tables and steer traffic here. IPv6 uses unsolicited Neighbor
// Advertisements instead; no IPv6 VIP pool exists yet, so v6 is an
// explicit error rather than a silent no-op.
func announce(link netlink.Link, p netip.Prefix, n int) error {
	if p.Addr().Is6() {
		return errors.New(errors.KindInvalid, "vip", "IPv6 unsolicited NA not supported yet")
	}
	mac := link.Attrs().HardwareAddr
	if len(mac) != 6 {
		return errors.New(errors.KindInvalid, "vip", "interface has no 6-byte MAC")
	}
	vip := p.Addr().Unmap().As4()
	frame := garpReply(mac, vip)
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htonS(unix.ETH_P_ARP)))
	if err != nil {
		return errors.Wrap(err, errors.KindUnavailable, "vip", "raw socket (need CAP_NET_RAW)")
	}
	defer unix.Close(fd)
	// Bind to the interface: send only on the announcement link.
	ll := &unix.SockaddrLinklayer{Protocol: htonS(unix.ETH_P_ARP), Ifindex: link.Attrs().Index}
	if err := unix.Bind(fd, ll); err != nil {
		return errors.Wrap(err, errors.KindUnavailable, "vip", "bind raw socket")
	}
	for i := 0; i < n; i++ {
		if err := unix.Sendto(fd, frame, 0, ll); err != nil {
			return errors.Wrap(err, errors.KindUnavailable, "vip", fmt.Sprintf("send gARP %d/%d", i+1, n))
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

func htonS(v uint16) uint16 {
	return v<<8 | v>>8 // host→network byte order
}

// garpReply builds a broadcast gratuitous-ARP reply frame (60 bytes):
// sender/target protocol IPs both = VIP, sender MAC = ours. The
// *reply* opcode is what legacy hosts that ignore requests still
// honor, and the broadcast targets every host on the link.
func garpReply(srcMAC []byte, vip [4]byte) []byte {
	f := make([]byte, 14+28)
	copy(f[0:6], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}) // eth dst: broadcast
	copy(f[6:12], srcMAC)                                    // eth src
	f[12], f[13] = 0x08, 0x06                                // ethertype: ARP
	arp := f[14:]
	arp[0], arp[1] = 0x00, 0x01 // hardware: ethernet
	arp[2], arp[3] = 0x08, 0x00 // protocol: IPv4
	arp[4] = 6                  // hw size
	arp[5] = 4                  // proto size
	arp[6], arp[7] = 0x00, 0x02 // operation: reply
	copy(arp[8:14], srcMAC)     // sender MAC
	copy(arp[14:18], vip[:])    // sender IP = VIP
	copy(arp[18:24], srcMAC)    // target MAC = ours (gratuitous)
	copy(arp[24:28], vip[:])    // target IP = VIP
	return f
}
