package main

import (
	"fmt"
	"strconv"
	"strings"
)

// splitNum 把 "12.5MB/s" 拆成 "12.5" 和 "MB/s"。
func splitNum(s string) (string, string) {
	s = strings.TrimSpace(s)
	i := strings.IndexFunc(s, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimSpace(s[i:])
}

func prefixPow(p string) (int, bool) {
	switch strings.ToLower(p) {
	case "":
		return 0, true
	case "k":
		return 1, true
	case "m":
		return 2, true
	case "g":
		return 3, true
	case "t":
		return 4, true
	case "p":
		return 5, true
	}
	return 0, false
}

func pow(base float64, n int) float64 {
	v := 1.0
	for range n {
		v *= base
	}
	return v
}

// parseRate 解析带宽，返回字节/秒。
// 裸前缀或带 b/bps/bit 后缀按比特计 (1000 进制)：100M、100Mbps、1G；
// 带大写 B 按字节计 (1024 进制)：12MB、12MB/s、1.5GB。
func parseRate(s string) (float64, error) {
	num, unit := splitNum(s)
	v, err := strconv.ParseFloat(num, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("无法解析带宽 %q", s)
	}
	u := strings.TrimSuffix(unit, "/s")
	u = strings.TrimSuffix(u, "ps")
	u = strings.TrimSuffix(u, "it")
	isBytes := strings.HasSuffix(u, "B")
	u = strings.TrimRight(u, "bB")
	u = strings.TrimSuffix(u, "i")
	n, ok := prefixPow(u)
	if !ok {
		return 0, fmt.Errorf("无法解析带宽单位 %q", s)
	}
	if isBytes {
		return v * pow(1024, n), nil
	}
	return v * pow(1000, n) / 8, nil
}

// parseSize 解析流量大小，返回字节数 (1024 进制)：500G、1.5T、800MB。
func parseSize(s string) (int64, error) {
	num, unit := splitNum(s)
	v, err := strconv.ParseFloat(num, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("无法解析流量 %q", s)
	}
	u := strings.TrimRight(unit, "bB")
	u = strings.TrimSuffix(u, "i")
	n, ok := prefixPow(u)
	if !ok {
		return 0, fmt.Errorf("无法解析流量单位 %q", s)
	}
	return int64(v * pow(1024, n)), nil
}

// fmtBytes 按 1024 进制格式化字节数。
func fmtBytes(n float64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	i := 0
	for n >= 1024 && i < len(units)-1 {
		n /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f B", n)
	}
	return fmt.Sprintf("%.2f %s", n, units[i])
}

// fmtRate 把字节/秒格式化为比特率 (1000 进制)。
func fmtRate(bytesPerSec float64) string {
	v := bytesPerSec * 8
	units := []string{"bps", "Kbps", "Mbps", "Gbps", "Tbps"}
	i := 0
	for v >= 1000 && i < len(units)-1 {
		v /= 1000
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}
