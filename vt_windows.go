package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// enableVT 打开 Windows 控制台的 ANSI 转义序列支持（Windows 10 及以上）。
func enableVT(f *os.File) bool {
	h := windows.Handle(f.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) != nil {
		return false
	}
	return mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 ||
		windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) == nil
}
