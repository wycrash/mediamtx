package onvif

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/mickeyzzc/onvif-go/v2/discovery"
	"github.com/mickeyzzc/onvif-go/v2/wsdiscovery"
)

const discoveryMulticast = "239.255.255.250:3702"

var errDiscoveryInterface = errors.New("network interface not found")

// probeDiscover sends one WS-Discovery probe on the same socket the scan reads.
// The log line is the socket's local address and every datagram that came back.
func (c *LibraryCamera) probeDiscover(ctx context.Context, timeout time.Duration, ifaceName string) ([]Found, error) {
	iface, err := resolveDiscoveryInterface(ifaceName)
	if err != nil {
		c.logDisc("discovery interface %q: %v", ifaceName, err)
		return nil, err
	}

	group, err := net.ResolveUDPAddr("udp4", discoveryMulticast)
	if err != nil {
		return nil, err
	}
	source, err := discoverySourceIP(ifaceName, iface)
	if err != nil {
		c.logDisc("discovery source %q: %v", ifaceName, err)
		return nil, err
	}
	if iface == nil {
		iface, err = resolveDiscoveryInterface(source.String())
		if err != nil {
			c.logDisc("discovery interface for %s: %v", source, err)
			return nil, err
		}
	}

	// Bind the probe source, not 239.255.255.250:3702. Cameras answer with
	// unicast to that source. On Windows a socket on port 3702 shares the
	// port with the system WS-Discovery service, which keeps the replies.
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: source, Port: 0})
	if err != nil {
		c.logDisc("discovery listen failed os=%s iface=%q source=%s: %v; up: %s",
			runtime.GOOS, ifaceName, source, err, upInterfaceSummary())
		return nil, fmt.Errorf("listen on %s: %w", source, err)
	}
	defer conn.Close()
	if err = setDiscoveryMulticastIF(conn, iface, source); err != nil {
		c.logDisc("discovery multicast interface %s: %v", source, err)
		return nil, fmt.Errorf("multicast interface %s: %w", source, err)
	}

	local := conn.LocalAddr().String()
	c.logDisc("discovery os=%s iface=%q using=%s source=%s local=%s wait=%s",
		runtime.GOOS, ifaceName, discoveryInterfaceLine(iface), source, local, timeout)
	if c != nil {
		c.ifaceOnce.Do(func() {
			c.logDisc("discovery interfaces: %s", upInterfaceSummary())
		})
	}

	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if err = conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}

	probe := wsdiscovery.BuildProbe(strconv.FormatInt(time.Now().UnixNano(), 10))
	if _, err = conn.WriteToUDP(probe, group); err != nil {
		c.logDisc("discovery probe send failed local=%s: %v", local, err)
		return nil, fmt.Errorf("send probe from %s: %w", local, err)
	}
	c.logDisc("discovery probe sent from %s to %s (%d bytes)", local, discoveryMulticast, len(probe))

	seen := map[string]struct{}{}
	var found []Found
	packets := 0
	buf := make([]byte, 8192)
	for {
		if err = ctx.Err(); err != nil {
			c.logDisc("discovery cancelled packets=%d devices=%d local=%s", packets, len(found), local)
			return found, err
		}
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				c.logDisc("discovery done packets=%d devices=%d local=%s", packets, len(found), local)
				c.logEmpty(local, packets, source)
				return found, nil
			}
			c.logDisc("discovery read failed after %d packets local=%s: %v", packets, local, err)
			return found, fmt.Errorf("read discovery reply on %s: %w", local, err)
		}
		packets++
		remote := ""
		if from != nil {
			remote = from.String()
		}
		item, ok, parseErr := foundFromDatagram(buf[:n])
		if parseErr != nil || !ok {
			c.logDisc("discovery udp from %s %d bytes skipped: %s", remote, n, datagramNote(buf[:n], parseErr, ok))
			continue
		}
		if _, dup := seen[item.Address]; dup {
			c.logDisc("discovery udp from %s duplicate %s", remote, item.Address)
			continue
		}
		seen[item.Address] = struct{}{}
		found = append(found, item)
		c.logDisc("discovery udp from %s %d bytes device=%s name=%q type=%s",
			remote, n, item.Address, item.Name, item.Type)
	}
}

func (c *LibraryCamera) logEmpty(local string, packets int, source net.IP) {
	if c == nil || packets != 0 {
		return
	}
	c.windowsHint.Do(func() {
		c.logDisc("discovery read 0 datagrams on %s (probe source %s)", local, source)
	})
}

func (c *LibraryCamera) logDisc(format string, args ...any) {
	if c == nil || c.Log == nil {
		return
	}
	c.Log.Log(logger.Info, "[onvif] "+format, args...)
}

func foundFromDatagram(data []byte) (Found, bool, error) {
	matches, err := wsdiscovery.ParseProbeMatches(data)
	if err != nil {
		return Found{}, false, err
	}
	if len(matches) == 0 {
		return Found{}, false, nil
	}
	xaddrs := strings.Fields(strings.TrimSpace(matches[0].XAddrs))
	if len(xaddrs) == 0 {
		return Found{}, false, nil
	}
	types := strings.Fields(strings.TrimSpace(matches[0].Types))
	scopes := strings.Fields(strings.TrimSpace(matches[0].Scopes))
	info := discovery.ParseScopes(scopes)
	return Found{
		Address:  xaddrs[0],
		Name:     info.Name,
		Hardware: info.Hardware,
		XAddrs:   append([]string(nil), xaddrs...),
		Type:     deviceType(types),
	}, true, nil
}

func datagramNote(data []byte, err error, ok bool) string {
	switch {
	case err != nil:
		return err.Error() + " payload=" + clipPayload(data)
	case !ok:
		return "no xaddrs payload=" + clipPayload(data)
	default:
		return clipPayload(data)
	}
}

func clipPayload(data []byte) string {
	const max = 160
	s := strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return ' '
		}
		return r
	}, string(data))
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

func discoverySourceIP(spec string, iface *net.Interface) (net.IP, error) {
	if ip := net.ParseIP(strings.TrimSpace(spec)); ip != nil {
		ip4 := ip.To4()
		if ip4 == nil {
			return nil, fmt.Errorf("discovery source %q is not IPv4", spec)
		}
		return append(net.IP(nil), ip4...), nil
	}
	if iface != nil {
		ips := ipv4Addrs(iface)
		if len(ips) == 0 {
			return nil, fmt.Errorf("interface %s has no IPv4 address", iface.Name)
		}
		return ips[0], nil
	}
	conn, err := net.Dial("udp4", discoveryMulticast)
	if err != nil {
		return nil, fmt.Errorf("default discovery source: %w", err)
	}
	defer conn.Close()
	udp, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || udp.IP.To4() == nil {
		return nil, errors.New("default discovery source is not IPv4")
	}
	return append(net.IP(nil), udp.IP.To4()...), nil
}

func resolveDiscoveryInterface(spec string) (*net.Interface, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	if iface, err := net.InterfaceByName(spec); err == nil {
		return iface, nil
	}
	ip := net.ParseIP(spec)
	if ip == nil {
		return nil, fmt.Errorf("%w: %q; up: %s", errDiscoveryInterface, spec, upInterfaceSummary())
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list network interfaces: %w", err)
	}
	for i := range ifaces {
		addrs, addrErr := ifaces[i].Addrs()
		if addrErr != nil {
			continue
		}
		for _, addr := range addrs {
			var got net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				got = v.IP
			case *net.IPAddr:
				got = v.IP
			}
			if got != nil && got.Equal(ip) {
				return &ifaces[i], nil
			}
		}
	}
	return nil, fmt.Errorf("%w: %q; up: %s", errDiscoveryInterface, spec, upInterfaceSummary())
}

func discoveryInterfaceLine(iface *net.Interface) string {
	if iface == nil {
		return "default"
	}
	ips := ipv4List(iface)
	if len(ips) == 0 {
		ips = []string{"-"}
	}
	return fmt.Sprintf("%s#%d %s ipv4=%s", iface.Name, iface.Index, iface.Flags.String(), strings.Join(ips, ","))
}

func upInterfaceSummary() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return err.Error()
	}
	parts := make([]string, 0, len(ifaces))
	for i := range ifaces {
		if ifaces[i].Flags&net.FlagUp == 0 {
			continue
		}
		ips := ipv4List(&ifaces[i])
		if len(ips) == 0 {
			ips = []string{"-"}
		}
		parts = append(parts, fmt.Sprintf("%s#%d %s [%s]",
			ifaces[i].Name, ifaces[i].Index, ifaces[i].Flags.String(), strings.Join(ips, ",")))
	}
	if len(parts) == 0 {
		return "(none up)"
	}
	return strings.Join(parts, "; ")
}

func ipv4Addrs(iface *net.Interface) []net.IP {
	if iface == nil {
		return nil
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil
	}
	var ips []net.IP
	for _, addr := range addrs {
		var ip net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		v4 := ip.To4()
		if v4 == nil {
			continue
		}
		ips = append(ips, append(net.IP(nil), v4...))
	}
	return ips
}

func ipv4List(iface *net.Interface) []string {
	ips := ipv4Addrs(iface)
	out := make([]string, len(ips))
	for i, ip := range ips {
		out[i] = ip.String()
	}
	return out
}
