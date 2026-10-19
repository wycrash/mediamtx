//go:build windows

package onvif

import (
	"fmt"
	"net"

	"golang.org/x/sys/windows"
)

func setDiscoveryMulticastIF(conn *net.UDPConn, _ *net.Interface, source net.IP) error {
	v4 := source.To4()
	if v4 == nil {
		return fmt.Errorf("multicast interface %s is not IPv4", source)
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var opt [4]byte
	copy(opt[:], v4)
	var sockErr error
	err = raw.Control(func(fd uintptr) {
		sockErr = windows.SetsockoptInet4Addr(
			windows.Handle(fd),
			windows.IPPROTO_IP,
			windows.IP_MULTICAST_IF,
			opt,
		)
	})
	if err != nil {
		return err
	}
	if sockErr != nil {
		return fmt.Errorf("IP_MULTICAST_IF %s: %w", source, sockErr)
	}
	return nil
}
