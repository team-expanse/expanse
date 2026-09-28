package console

import "net"

// LocalAddrs lists global unicast addresses on up, non-loopback interfaces, IPv4 first.
func LocalAddrs() []Addr {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var v4, v6 []Addr
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagLoopback != 0 || ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || !ipn.IP.IsGlobalUnicast() {
				continue
			}
			if ipn.IP.To4() != nil {
				v4 = append(v4, Addr{Iface: ifc.Name, IP: ipn.IP.String()})
			} else {
				v6 = append(v6, Addr{Iface: ifc.Name, IP: ipn.IP.String()})
			}
		}
	}
	return append(v4, v6...)
}

// LocalIPs is LocalAddrs as bare addresses, for the installer's done screen.
func LocalIPs() []string {
	var ips []string
	for _, a := range LocalAddrs() {
		ips = append(ips, a.IP)
	}
	return ips
}
