package main

import (
	"net"
	"syscall"
)

// bindDevice 用 IP_BOUND_IF / IPV6_BOUND_IF 强制从指定网卡发出。
func bindDevice(dev string) func(network, address string, c syscall.RawConn) error {
	ifi, err := net.InterfaceByName(dev)
	if err != nil {
		return nil
	}
	return func(network, _ string, c syscall.RawConn) error {
		var err error
		if cerr := c.Control(func(fd uintptr) {
			if network == "tcp6" {
				err = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_BOUND_IF, ifi.Index)
			} else {
				err = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_BOUND_IF, ifi.Index)
			}
		}); cerr != nil {
			return cerr
		}
		return err
	}
}
