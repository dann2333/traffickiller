package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// geoCache 在后台查询服务器 IP 的属地并缓存，不影响下载。
// 优先用百度 IP 查询（中文省市 + 运营商，仅 IPv4），查不到再用 ip-api.com。
// 只发送服务器 IP，每个 IP 只查一次。
type geoCache struct {
	c  *http.Client
	mu sync.Mutex
	m  map[string]*geoEntry
	q  chan string
}

type geoEntry struct {
	loc   string
	busy  bool
	retry time.Time // 查询失败后到这个时间再重试
}

func newGeoCache(c *http.Client) *geoCache {
	return &geoCache{c: c, m: map[string]*geoEntry{}, q: make(chan string, 256)}
}

// get 返回 ip 的属地；还没查到时返回空串并在后台排队查询。
func (g *geoCache) get(ip string) string {
	if a := net.ParseIP(ip); a == nil {
		return ""
	} else if a.IsLoopback() {
		return "本机"
	} else if a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsUnspecified() {
		return "局域网"
	}
	if g == nil {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.m[ip]
	if e == nil {
		e = &geoEntry{}
		g.m[ip] = e
	}
	if e.loc == "" && !e.busy && time.Now().After(e.retry) {
		select {
		case g.q <- ip:
			e.busy = true
		default:
		}
	}
	return e.loc
}

func (g *geoCache) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ip := <-g.q:
			loc := g.query(ctx, ip)
			g.mu.Lock()
			e := g.m[ip]
			e.busy, e.loc = false, loc
			if loc == "" {
				e.retry = time.Now().Add(time.Minute)
			}
			g.mu.Unlock()
		}
	}
}

func (g *geoCache) query(ctx context.Context, ip string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if net.ParseIP(ip).To4() != nil {
		var r struct {
			Data []struct {
				Location string `json:"location"`
			} `json:"data"`
		}
		if g.getJSON(ctx, "https://opendata.baidu.com/api.php?resource_id=6006&oe=utf8&query="+ip, &r) == nil &&
			len(r.Data) > 0 && r.Data[0].Location != "" {
			return shortPlace(r.Data[0].Location) // "辽宁省本溪市 联通" -> "辽宁本溪 联通"
		}
	}
	var r struct {
		Status, Country, RegionName, City, ISP string
	}
	u := "http://ip-api.com/json/" + url.PathEscape(ip) + "?lang=zh-CN&fields=status,country,regionName,city,isp"
	if g.getJSON(ctx, u, &r) != nil || r.Status != "success" {
		return ""
	}
	loc := strings.TrimSpace(r.Country + " " + r.City)
	if r.Country == "中国" {
		region, city := shortPlace(r.RegionName), shortPlace(r.City)
		loc = region
		if !strings.HasPrefix(city, region) {
			loc += city
		}
	}
	return strings.TrimSpace(loc + " " + shortISP(r.ISP))
}

// shortPlace 去掉省、市、自治区等后缀，如 "广西壮族自治区北海市" -> "广西北海"。
var shortPlace = strings.NewReplacer(
	"壮族自治区", "", "回族自治区", "", "维吾尔自治区", "", "自治区", "", "特别行政区", "", "省", "", "市", "",
).Replace

func (g *geoCache) getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := g.c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// shortISP 把国内运营商的英文名简化为 电信/联通/移动。
func shortISP(isp string) string {
	s := strings.ToLower(isp)
	switch {
	case strings.Contains(s, "telecom") || strings.Contains(s, "chinanet"):
		return "电信"
	case strings.Contains(s, "unicom") || strings.Contains(s, "cnc") || strings.Contains(s, "china169"):
		return "联通"
	case strings.Contains(s, "mobile") || strings.Contains(s, "cmnet"):
		return "移动"
	case strings.Contains(s, "cernet") || strings.Contains(s, "education"):
		return "教育网"
	}
	return isp
}
