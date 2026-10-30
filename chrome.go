package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	http2 "github.com/bogdanfinn/fhttp/http2"
	utls "github.com/bogdanfinn/utls"
	"golang.org/x/net/proxy"
)

// doer 是能发请求的客户端。正式跑的是启动器指纹客户端，测试里用普通 fhttp.Client。
type doer interface {
	Do(req *fhttp.Request) (*fhttp.Response, error)
}

// errNeedHTTP1 表示这次 TLS 握手谈成的是 HTTP/1.1，不是 h2。
// 崩坏 3 的清单域名只接受 http/1.1，硬走 HTTP/2 会被对端回一截明文 HTTP。
var errNeedHTTP1 = errors.New("server negotiated http/1.1")

// client 为该出口创建一个和 HoYoPlay 下载组件 sophon.dll 一样的客户端。
// sophon 1.18 的 LastChange 是 refs/branch-heads/5414@{#1496}，即 Cronet / Chromium 109.0.5414。
// ClientHello 用 utls 的 Chrome_109（这一版没有 Kyber、不打乱扩展）；HTTP/2 SETTINGS 与连接窗口
// 按同一版浏览器。不引入 tls-client，避免把用不到的 QUIC 栈链进二进制。
func (e egress) client(idlePerHost int) (doer, error) {
	if idlePerHost < 1 {
		idlePerHost = 1
	}
	idle := 90 * time.Second
	cache := utls.NewLRUClientSessionCache(4)
	h1 := &fhttp.Transport{
		DialContext:         e.dialContext,
		DisableCompression:  true,
		MaxIdleConns:        idlePerHost,
		MaxIdleConnsPerHost: idlePerHost,
		IdleConnTimeout:     idle,
	}
	h1s := &fhttp.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return e.dialTLS(ctx, cache, network, addr, nil, true)
		},
		DisableCompression:  true,
		MaxIdleConns:        idlePerHost,
		MaxIdleConnsPerHost: idlePerHost,
		IdleConnTimeout:     idle,
	}
	h2 := &http2.Transport{
		DialTLS: func(network, addr string, cfg *utls.Config) (net.Conn, error) {
			return e.dialTLS(context.Background(), cache, network, addr, cfg, false)
		},
		DisableCompression: true,
		IdleConnTimeout:    idle,
		ConnectionFlow:     15663105,
		Settings: map[http2.SettingID]uint32{
			http2.SettingHeaderTableSize:      65536,
			http2.SettingEnablePush:           0,
			http2.SettingMaxConcurrentStreams: 1000,
			http2.SettingInitialWindowSize:    6291456,
			http2.SettingMaxHeaderListSize:    262144,
		},
		SettingsOrder: []http2.SettingID{
			http2.SettingHeaderTableSize,
			http2.SettingEnablePush,
			http2.SettingMaxConcurrentStreams,
			http2.SettingInitialWindowSize,
			http2.SettingMaxHeaderListSize,
		},
		PseudoHeaderOrder: []string{":method", ":authority", ":scheme", ":path"},
	}
	return &fhttp.Client{Transport: &schemeRT{h1: h1, h1s: h1s, h2: h2}}, nil
}

// schemeRT 按 URL 方案分流。明文走 HTTP/1.1；HTTPS 先按 ALPN 走 HTTP/2，
// 对端只谈成 http/1.1 时改走 HTTP/1.1，并记住这个主机。
type schemeRT struct {
	h1, h1s *fhttp.Transport
	h2      *http2.Transport
	http1   sync.Map
}

func (s *schemeRT) RoundTrip(req *fhttp.Request) (*fhttp.Response, error) {
	if req.URL == nil || req.URL.Scheme == "http" {
		return s.h1.RoundTrip(req)
	}
	host := req.URL.Hostname()
	if _, ok := s.http1.Load(host); ok {
		return s.h1s.RoundTrip(req)
	}
	resp, err := s.h2.RoundTrip(req)
	if err != nil && errors.Is(err, errNeedHTTP1) {
		s.http1.Store(host, struct{}{})
		return s.h1s.RoundTrip(req)
	}
	return resp, err
}

// dialTLS 在已有的拨号（含代理）之上做 Chrome 109 握手。每个客户端自带一个很小的会话缓存，
// 断线重连可以恢复会话，少一次完整握手。allowH1 为假时，没谈成 h2 就关掉连接并返回 errNeedHTTP1。
func (e egress) dialTLS(ctx context.Context, cache utls.ClientSessionCache, network, addr string, cfg *utls.Config, allowH1 bool) (net.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	raw, err := e.dialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	uCfg := &utls.Config{ServerName: host, OmitEmptyPsk: true, ClientSessionCache: cache}
	if cfg != nil {
		if cfg.ServerName != "" {
			uCfg.ServerName = cfg.ServerName
		}
		uCfg.InsecureSkipVerify = cfg.InsecureSkipVerify
		uCfg.RootCAs = cfg.RootCAs
	}
	uconn := utls.UClient(raw, uCfg, utls.HelloChrome_109, false, false, true)
	if err := uconn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	if !allowH1 && uconn.ConnectionState().NegotiatedProtocol != "h2" {
		uconn.Close()
		return nil, errNeedHTTP1
	}
	return uconn, nil
}

func (e egress) netDialer() *net.Dialer {
	d := &net.Dialer{KeepAlive: 30 * time.Second, Timeout: 10 * time.Second}
	if e.ip != nil {
		d.LocalAddr = &net.TCPAddr{IP: e.ip}
	}
	if e.dev != "" {
		d.Control = bindDevice(e.dev)
	}
	return d
}

// dialContext 自己拨号，连接超时单独计算，不会被「整段请求不限时」盖住。
// 设了代理时从同一出口去连代理，再 CONNECT / SOCKS 到目标。
func (e egress) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if e.ip != nil {
		if e.ip.To4() != nil {
			network = "tcp4"
		} else {
			network = "tcp6"
		}
	}
	d := e.netDialer()
	raw := envProxy()
	if raw == "" {
		return d.DialContext(ctx, network, addr)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Host == "" {
		u, err = url.Parse("http://" + raw)
		if err != nil {
			return nil, err
		}
	}
	switch strings.ToLower(u.Scheme) {
	case "socks5", "socks5h", "socks4", "socks4a":
		var auth *proxy.Auth
		if u.User != nil {
			pw, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: pw}
		}
		sd, err := proxy.SOCKS5("tcp", u.Host, auth, d)
		if err != nil {
			return nil, err
		}
		if cd, ok := sd.(proxy.ContextDialer); ok {
			return cd.DialContext(ctx, network, addr)
		}
		return sd.Dial(network, addr)
	default:
		return dialHTTPConnect(ctx, d, network, u, addr)
	}
}

func dialHTTPConnect(ctx context.Context, d *net.Dialer, network string, proxyURL *url.URL, addr string) (net.Conn, error) {
	host := proxyURL.Host
	if host == "" {
		return nil, fmt.Errorf("代理地址为空")
	}
	if !strings.Contains(host, ":") {
		host += ":80"
	}
	c, err := d.DialContext(ctx, network, host)
	if err != nil {
		return nil, err
	}
	req := "CONNECT " + addr + " HTTP/1.1\r\nHost: " + addr + "\r\n"
	if proxyURL.User != nil {
		pw, _ := proxyURL.User.Password()
		tok := base64.StdEncoding.EncodeToString([]byte(proxyURL.User.Username() + ":" + pw))
		req += "Proxy-Authorization: Basic " + tok + "\r\n"
	}
	req += "\r\n"
	if _, err := io.WriteString(c, req); err != nil {
		c.Close()
		return nil, err
	}
	br := bufio.NewReader(c)
	resp, err := fhttp.ReadResponse(br, &fhttp.Request{Method: fhttp.MethodConnect})
	if err != nil {
		c.Close()
		return nil, err
	}
	if resp.Body != nil {
		resp.Body.Close()
	}
	if resp.StatusCode != fhttp.StatusOK {
		c.Close()
		return nil, fmt.Errorf("代理 CONNECT 失败: %s", resp.Status)
	}
	if br.Buffered() > 0 {
		return &prefixConn{Conn: c, r: br}, nil
	}
	return c, nil
}

type prefixConn struct {
	net.Conn
	r *bufio.Reader
}

func (p *prefixConn) Read(b []byte) (int, error) { return p.r.Read(b) }

func envProxy() string {
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy", "HTTP_PROXY", "http_proxy"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// launcherHeaders 按 sophon.dll 拉 CDN 文件时的头来组，不是 CEF 页面，也不是桌面 Chrome。
// download_item.cc 里写死的 Accept-Encoding 是 "gzip,deflate,br"（逗号后没有空格）。
// 没有 sec-ch-ua、sec-fetch-*、priority，也没有 x-rpc-*（那是云配请求，不是下文件）。
func launcherHeaders(ua, rng string) fhttp.Header {
	h := fhttp.Header{}
	h.Set("accept-encoding", "gzip,deflate,br")
	h.Set("user-agent", ua)
	order := []string{
		"accept-encoding",
		"user-agent",
	}
	if rng != "" {
		h.Set("range", rng)
		order = append(order, "range")
	}
	h[fhttp.HeaderOrderKey] = order
	h[fhttp.PHeaderOrderKey] = []string{":method", ":authority", ":scheme", ":path"}
	return h
}
