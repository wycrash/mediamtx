//go:build !windows && !linux && !darwin && !freebsd

package onvif

import (
	"fmt"
	"net"

	"golang.org/x/net/ipv4"
)

func setDiscoveryMulticastIF(conn *net.UDPConn, iface *net.Interface, source net.IP) error {
	if source.To4() == nil {
		return fmt.Errorf("multicast interface %s is not IPv4", source)
	}
	if iface == nil {
		return nil
	}
	return ipv4.NewPacketConn(conn).SetMulticastInterface(iface)
}
