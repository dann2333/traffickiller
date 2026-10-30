package main

import (
	"fmt"
	"net"
	"strings"
)

// egress 描述一个出口：本机源地址，以及（可选的）要强制绑定的网卡。
type egress struct {
	label string
	ip    net.IP
	dev   string
}

// parseEgress 解析 -i 参数：逗号分隔的网卡名或本机 IP。
func parseEgress(spec string, prefer6 bool) ([]egress, error) {
	if strings.TrimSpace(spec) == "" {
		return []egress{{label: "系统默认路由"}}, nil
	}
	var out []egress
	for _, s := range strings.Split(spec, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if ip := net.ParseIP(s); ip != nil {
			out = append(out, egress{label: s, ip: ip})
			continue
		}
		ifi, err := net.InterfaceByName(s)
		if err != nil {
			return nil, fmt.Errorf("找不到网卡 %q（可用: %s）", s, listIfaces())
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			return nil, fmt.Errorf("读取网卡 %s 地址失败: %v", s, err)
		}
		ip := pickIP(addrs, prefer6)
		if ip == nil {
			return nil, fmt.Errorf("网卡 %s 没有可用的 IP 地址", s)
		}
		out = append(out, egress{label: fmt.Sprintf("%s (%s)", s, ip), ip: ip, dev: s})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-i 参数为空")
	}
	return out, nil
}

// pickIP 选网卡上的地址：默认 IPv4，prefer6 或无 IPv4 时用 IPv6（优先公网地址）。
func pickIP(addrs []net.Addr, prefer6 bool) net.IP {
	var v4, v6 net.IP
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() {
			continue
		}
		if ip4 := ipn.IP.To4(); ip4 != nil {
			if v4 == nil {
				v4 = ip4
			}
		} else if v6 == nil || (v6.IsPrivate() && !ipn.IP.IsPrivate()) {
			v6 = ipn.IP
		}
	}
	if (prefer6 && v6 != nil) || v4 == nil {
		return v6
	}
	return v4
}

func listIfaces() string {
	ifs, err := net.Interfaces()
	if err != nil {
		return "?"
	}
	var names []string
	for _, i := range ifs {
		if i.Flags&net.FlagLoopback == 0 {
			names = append(names, i.Name)
		}
	}
	return strings.Join(names, ", ")
}
