//go:build linux || darwin || freebsd

package onvif

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

func setDiscoveryMulticastIF(conn *net.UDPConn, iface *net.Interface, source net.IP) error {
	v4 := source.To4()
	if v4 == nil {
		return fmt.Errorf("multicast interface %s is not IPv4", source)
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var mreq unix.IPMreqn
	copy(mreq.Address[:], v4)
	if iface != nil {
		mreq.Ifindex = int32(iface.Index)
	}
	var sockErr error
	err = raw.Control(func(fd uintptr) {
		sockErr = unix.SetsockoptIPMreqn(int(fd), unix.IPPROTO_IP, unix.IP_MULTICAST_IF, &mreq)
	})
	if err != nil {
		return err
	}
	if sockErr != nil {
		return fmt.Errorf("IP_MULTICAST_IF %s: %w", source, sockErr)
	}
	return nil
}
