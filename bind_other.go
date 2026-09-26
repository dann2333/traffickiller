//go:build !linux && !darwin

package main

import "syscall"

// bindDevice 在其他系统上不做网卡绑定，仅按源地址绑定。
func bindDevice(string) func(network, address string, c syscall.RawConn) error { return nil }
