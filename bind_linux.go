package main

import (
	"sync"
	"syscall"
)

var bindWarn sync.Once

// bindDevice 用 SO_BINDTODEVICE 强制从指定网卡发出。无权限时退化为仅按源地址绑定。
func bindDevice(dev string) func(network, address string, c syscall.RawConn) error {
	return func(_, _ string, c syscall.RawConn) error {
		var err error
		if cerr := c.Control(func(fd uintptr) {
			err = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, dev)
		}); cerr != nil {
			return cerr
		}
		if err != nil {
			bindWarn.Do(func() {
				con.logf("警告: 无法绑定网卡 %s (%v)，仅按源地址绑定；用 root 运行可强制走该网卡", dev, err)
			})
		}
		return nil
	}
}
