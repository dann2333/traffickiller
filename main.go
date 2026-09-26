// traffickiller: 从米哈游官方 CDN 并发下载游戏资源并直接丢弃，用来消耗流量。
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

var version = "dev"

// 默认 UA 均为社区项目里使用的米哈游启动器 (HoYoPlay) 真实 UA，每个连接随机分配一个：
//
//	HYPContainer/1.10.1.283 (windows 10/11)  Collapse Launcher（模拟启动器资源接口）
//	HYPContainer/1.3.3.182                   gsuid_core、TeyvatGuide、AUTO-MAS
//	HYPContainer/1.1.4.133                   Snap.Hutao、FufuLauncher、sigewinne-toolkit
var defaultUAs = []string{
	"HYPContainer/1.10.1.283 (windows 10)",
	"HYPContainer/1.10.1.283 (windows 11)",
	"HYPContainer/1.3.3.182",
	"HYPContainer/1.1.4.133",
}

// userAgents 是实际使用的 UA 列表（-ua 指定或默认）。
var userAgents = defaultUAs

func randomUA() string { return userAgents[rand.IntN(len(userAgents))] }

// listFlag 支持重复指定的参数，如 -ua A -ua B。
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, " | ") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

var (
	errInterrupted  = errors.New("收到退出信号")
	errDuration     = errors.New("已达到运行时长")
	errTotalReached = errors.New("已达到总流量")
	errNoSource     = errors.New("所有资源均不可用")
)

const usage = `traffickiller %s - 流量消耗器
从米哈游官方 CDN 并发下载最新版原神/星穹铁道/绝区零/崩坏3 资源，数据直接丢弃，不写硬盘。

用法: traffickiller [选项] [URL...]

选项:
  -i, -iface <网卡>    出口网卡名或本机 IP，多个用逗号分隔 (如 eth0 / pppoe-wan,pppoe-wan2)
                       默认走系统路由；多网卡时并发连接轮流分配到各网卡
  -c, -conc <数量>     并发连接数 (默认 32；跑 5Gbps 以上建议 64 或更高)
  -l, -limit <带宽>    总带宽上限，默认不限。100M / 100Mbps = 100 兆比特每秒 (宽带口径)，
                       12MB / 12MB/s = 12 兆字节每秒
  -t, -total <流量>    累计下载达到该流量后退出，如 500G、1.5T、800MB (1024 进制)
  -d, -duration <时长> 运行多久后退出，如 30m、2h、1h30m
  -g, -games <游戏>    资源来源，逗号分隔: hk4e(原神) hkrpg(星铁) nap(绝区零) bh3(崩坏3)
                       (默认全部)
  -all                 使用全部历史版本的整包，而不只是最新版本
  -f, -file <文件>     从文件读取 URL 列表 (每行一个)，替代内置资源；也可直接在参数末尾写 URL
  -6                   绑定网卡时优先使用 IPv6 地址
  -http                CDN 下载改用 HTTP 而非 HTTPS，省去 TLS 解密开销 (适合路由器等弱 CPU 设备)
  -plain               不用全屏界面，只输出单行状态 (输出不是终端时自动如此)
  -nogeo               界面上不查询服务器 IP 属地 (默认通过百度 / ip-api.com 查询)
  -interval <时长>     状态刷新间隔 (默认: 终端 1s，非终端 10s)
  -ua <UA>             自定义 User-Agent，可重复指定多个 (-ua A -ua B)，每个连接随机分配一个；
                       默认使用几个米哈游启动器的 UA (HYPContainer/...)
  -list                只打印资源列表后退出
  -v, -version         显示版本

示例:
  traffickiller                              # 不限速，32 并发，一直跑
  traffickiller -i eth1 -c 16 -l 200M        # 从 eth1 下载，16 并发，限速 200Mbps
  traffickiller -l 50M -t 300G               # 限速 50Mbps，跑满 300GB 后退出
  traffickiller -i pppoe-wan -d 6h -g ys,zzz # 只下载原神和绝区零，跑 6 小时
`

func main() { os.Exit(run()) }

func run() int {
	var (
		iface, limitStr, totalStr, games, urlFile string
		uas                                       listFlag
		conc                                      int
		dur, interval                             time.Duration
		all, prefer6, plainHTTP, list, showVer    bool
		plain, noGeo                              bool
	)
	fs := flag.CommandLine
	for _, n := range []string{"i", "iface"} {
		fs.StringVar(&iface, n, "", "")
	}
	for _, n := range []string{"c", "conc"} {
		fs.IntVar(&conc, n, 32, "")
	}
	for _, n := range []string{"l", "limit"} {
		fs.StringVar(&limitStr, n, "0", "")
	}
	for _, n := range []string{"t", "total"} {
		fs.StringVar(&totalStr, n, "0", "")
	}
	for _, n := range []string{"d", "duration"} {
		fs.DurationVar(&dur, n, 0, "")
	}
	for _, n := range []string{"g", "games"} {
		fs.StringVar(&games, n, strings.Join(gameIDs, ","), "")
	}
	for _, n := range []string{"f", "file"} {
		fs.StringVar(&urlFile, n, "", "")
	}
	for _, n := range []string{"v", "version"} {
		fs.BoolVar(&showVer, n, false, "")
	}
	fs.BoolVar(&all, "all", false, "")
	fs.BoolVar(&prefer6, "6", false, "")
	fs.BoolVar(&plainHTTP, "http", false, "")
	fs.BoolVar(&list, "list", false, "")
	fs.BoolVar(&plain, "plain", false, "")
	fs.BoolVar(&noGeo, "nogeo", false, "")
	fs.DurationVar(&interval, "interval", 0, "")
	fs.Var(&uas, "ua", "")
	fs.Usage = func() { fmt.Fprintf(os.Stderr, usage, version) }
	flag.Parse()
	if len(uas) > 0 {
		userAgents = uas
	}

	if showVer {
		fmt.Println(version)
		return 0
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "错误: "+format+"\n", a...)
		return 2
	}
	rate, err := parseRate(limitStr)
	if err != nil {
		return fail("%v", err)
	}
	maxTotal, err := parseSize(totalStr)
	if err != nil {
		return fail("%v", err)
	}
	if conc < 1 {
		return fail("并发数必须 >= 1")
	}
	gameList, err := parseGames(games)
	if err != nil {
		return fail("%v", err)
	}
	egs, err := parseEgress(iface, prefer6)
	if err != nil {
		return fail("%v", err)
	}

	ctx, stop := context.WithCancelCause(context.Background())
	defer stop(nil)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		stop(errInterrupted)
		<-sig
		con.stopUI()
		os.Exit(130)
	}()

	// 资源列表
	var groups []*group
	urls := flag.Args()
	if urlFile != "" {
		fromFile, err := loadURLFile(urlFile)
		if err != nil {
			return fail("%v", err)
		}
		urls = append(urls, fromFile...)
	}
	if len(urls) > 0 {
		g := &group{name: "自定义", desc: fmt.Sprintf("%d 个 URL", len(urls))}
		b := newSourceBuilder("URL", "", "", len(urls))
		for _, u := range urls {
			b.add([]byte(u), 0)
		}
		g.add(b.build())
		groups = append(groups, g)
	} else {
		con.logf("正在获取资源列表...")
		groups = loadGames(ctx, egs[0].client(4), gameList, all)
	}
	if ctx.Err() != nil {
		return 130
	}
	if len(groups) == 0 {
		return fail("没有可用的资源")
	}
	debug.FreeOSMemory() // 解析清单产生的临时内存还给系统，常驻内存更低
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(25) // 运行期分配很少，调低 GC 目标换更低的常驻内存
	}
	if list {
		w := bufio.NewWriter(os.Stdout)
		defer w.Flush()
		for _, g := range groups {
			for _, s := range g.sources {
				for _, sp := range s.items {
					w.WriteString(s.url(sp) + "\n")
				}
			}
		}
		return 0
	}

	// 横幅
	var labels []string
	for _, e := range egs {
		labels = append(labels, e.label)
	}
	limitDesc, totalDesc, durDesc := "不限", "不限", "不限"
	if rate > 0 {
		limitDesc = fmtRate(rate) + " (" + fmtBytes(rate) + "/s)"
	}
	if maxTotal > 0 {
		totalDesc = fmtBytes(float64(maxTotal))
	}
	if dur > 0 {
		durDesc = dur.String()
	}
	con.logf("traffickiller %s | 出口: %s | 并发: %d | 限速: %s | 总量: %s | 时长: %s | UA: %d 个随机分配",
		version, strings.Join(labels, ", "), conc, limitDesc, totalDesc, durDesc, len(userAgents))
	for _, g := range groups {
		if sz := g.bytes(); sz > 0 {
			con.logf("  %s: %s, 共 %s", g.name, g.desc, fmtBytes(float64(sz)))
		} else {
			con.logf("  %s: %s", g.name, g.desc)
		}
	}

	proxy := proxyFor(groups, plainHTTP) // 须在下载开始、资源列表被改动之前取

	d := &downloader{
		pool:      &pool{groups: slices.Clone(groups)}, // 界面持有原列表，停用的游戏仍会显示
		lim:       newLimiter(rate),
		plainHTTP: plainHTTP,
		maxTotal:  maxTotal,
		stop:      stop,
	}
	if dur > 0 {
		time.AfterFunc(dur, func() { stop(errDuration) })
	}
	start := time.Now()
	// 每个连接有独立的连接池和固定的 UA，看起来像多个独立的客户端。
	var wg sync.WaitGroup
	conns := make([]*conn, conc)
	uaOff := rand.IntN(len(userAgents))
	for i := range conc {
		c := egs[i%len(egs)].client(1)
		st := newConn(userAgents[(uaOff+i)%len(userAgents)])
		conns[i] = st
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.worker(ctx, c, st)
		}()
	}
	if interval <= 0 {
		interval = 10 * time.Second
		if con.tty {
			interval = time.Second
		}
	}
	if !plain && con.startUI() {
		info := fmt.Sprintf("出口 %s | 限速 %s | 总量 %s | UA %d 个", strings.Join(labels, ", "), limitDesc, totalDesc, len(userAgents))
		if proxy != "" {
			info += " | 经代理 " + proxy + " (服务器列为代理地址)"
		}
		var geo *geoCache
		if !noGeo {
			geo = newGeoCache(egs[0].client(2))
			go geo.run(ctx)
		}
		go newTUI(d, groups, conns, start, dur, info, geo).run(ctx, interval)
	} else {
		go d.report(ctx, start, interval, conc)
	}
	wg.Wait()
	con.stopUI()

	cause := context.Cause(ctx)
	el := time.Since(start)
	n := d.total.Load()
	con.logf("结束: %v | 共下载 %s | 用时 %s | 平均 %s",
		cause, fmtBytes(float64(n)), el.Round(time.Second), fmtRate(float64(n)/el.Seconds()))
	if errors.Is(cause, errNoSource) {
		return 1
	}
	return 0
}

// proxyFor 返回下载会经过的代理（来自 HTTPS_PROXY 等环境变量），没有则为空。
func proxyFor(groups []*group, plainHTTP bool) string {
	s := groups[0].sources[0]
	u := s.url(s.items[0])
	if plainHTTP && strings.HasPrefix(u, "https://") {
		u = "http://" + u[len("https://"):]
	}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return ""
	}
	if p, err := http.ProxyFromEnvironment(req); err == nil && p != nil {
		return p.Host
	}
	return ""
}

// report 周期性输出速率和累计流量。
func (d *downloader) report(ctx context.Context, start time.Time, every time.Duration, conc int) {
	t := time.NewTicker(every)
	defer t.Stop()
	last, lastT := int64(0), start
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			cur := d.total.Load()
			rate := float64(cur-last) / now.Sub(lastT).Seconds()
			avg := float64(cur) / now.Sub(start).Seconds()
			last, lastT = cur, now
			el := now.Sub(start).Round(time.Second)
			s := fmt.Sprintf("[%02d:%02d:%02d] 速率 %s (%s/s) | 平均 %s | 累计 %s",
				int(el.Hours()), int(el.Minutes())%60, int(el.Seconds())%60,
				fmtRate(rate), fmtBytes(rate), fmtRate(avg), fmtBytes(float64(cur)))
			if d.maxTotal > 0 {
				s += fmt.Sprintf(" / %s (%.1f%%)", fmtBytes(float64(d.maxTotal)), float64(cur)*100/float64(d.maxTotal))
			}
			s += fmt.Sprintf(" | 连接 %d/%d", d.active.Load(), conc)
			con.status(s)
		}
	}
}
