package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/term"
)

// console 在终端上用 \r 原地刷新状态行，日志行不会与状态行混在一起。
// 全屏界面运行时，日志暂存起来显示在界面里，退出界面后再补打出来。
type console struct {
	mu      sync.Mutex
	tty     bool
	last    int
	ui      bool
	logs    []string
	dropped int
}

const keepLogs = 20

var con = newConsole()

func newConsole() *console {
	return &console{tty: term.IsTerminal(int(os.Stderr.Fd()))}
}

func (c *console) status(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.tty {
		fmt.Fprintln(os.Stderr, s)
		return
	}
	w := width(s)
	fmt.Fprint(os.Stderr, "\r"+s+strings.Repeat(" ", max(c.last-w, 0)))
	c.last = w
}

func (c *console) logf(format string, a ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := time.Now().Format("15:04:05 ") + fmt.Sprintf(format, a...)
	if c.ui {
		if len(c.logs) == keepLogs {
			c.logs = c.logs[1:]
			c.dropped++
		}
		c.logs = append(c.logs, s)
		return
	}
	if c.tty && c.last > 0 {
		s = "\r" + s + strings.Repeat(" ", max(c.last-width(s), 0))
		c.last = 0
	}
	fmt.Fprintln(os.Stderr, s)
}

// startUI 切换到全屏界面（终端备用屏幕），不支持时返回 false。
func (c *console) startUI() bool {
	if !c.tty || os.Getenv("TERM") == "dumb" || !enableVT(os.Stderr) {
		return false
	}
	if _, _, err := term.GetSize(int(os.Stderr.Fd())); err != nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ui = true
	// 备用屏幕、隐藏光标、关闭自动换行
	os.Stderr.WriteString("\x1b[?1049h\x1b[?25l\x1b[?7l\x1b[H\x1b[2J")
	return true
}

// stopUI 退出全屏界面，恢复终端，补打界面期间的日志。可重复调用。
func (c *console) stopUI() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ui {
		return
	}
	c.ui = false
	os.Stderr.WriteString("\x1b[?7h\x1b[?25h\x1b[?1049l")
	if c.dropped > 0 {
		fmt.Fprintf(os.Stderr, "(省略更早的 %d 条日志)\n", c.dropped)
	}
	for _, s := range c.logs {
		fmt.Fprintln(os.Stderr, s)
	}
	c.logs, c.dropped = nil, 0
}

func (c *console) draw(frame string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ui {
		os.Stderr.WriteString(frame)
	}
}

func (c *console) recentLogs(n int) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.logs[max(len(c.logs)-n, 0):]...)
}

// tui 是全屏实时界面：总速率、各游戏的速率，以及每个连接正在下载的文件和速率。
type tui struct {
	d      *downloader
	groups []*group
	conns  []*conn
	start  time.Time
	dur    time.Duration
	info   string    // 出口、限速等不变的信息
	geo    *geoCache // nil 表示不查询属地

	w, h         int
	lastT        time.Time
	last         int64
	lastG, lastC []int64
	rate         float64
	rateG, rateC []float64
}

func newTUI(d *downloader, groups []*group, conns []*conn, start time.Time, dur time.Duration, info string, geo *geoCache) *tui {
	return &tui{
		d: d, groups: groups, conns: conns, start: start, dur: dur, info: info, geo: geo,
		lastT: start,
		lastG: make([]int64, len(groups)), rateG: make([]float64, len(groups)),
		lastC: make([]int64, len(conns)), rateC: make([]float64, len(conns)),
	}
}

func (u *tui) run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		u.frame(time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// sample 按上次刷新以来的增量计算各项速率。
func (u *tui) sample(now time.Time) {
	dt := now.Sub(u.lastT).Seconds()
	if dt <= 0 {
		return
	}
	u.lastT = now
	rate := func(cur int64, last *int64) float64 {
		r := float64(cur-*last) / dt
		*last = cur
		return r
	}
	u.rate = rate(u.d.total.Load(), &u.last)
	for i, g := range u.groups {
		u.rateG[i] = rate(g.got.Load(), &u.lastG[i])
	}
	for i, c := range u.conns {
		u.rateC[i] = rate(c.n.Load(), &u.lastC[i])
	}
}

func (u *tui) frame(now time.Time) {
	w, h, err := term.GetSize(int(os.Stderr.Fd()))
	if err != nil || w < 20 || h < 5 {
		w, h = max(w, 80), max(h, 24)
	}
	var b strings.Builder
	if w != u.w || h != u.h {
		b.WriteString("\x1b[2J") // 窗口大小变了，整屏重画
		u.w, u.h = w, h
	}
	u.sample(now)
	b.WriteString("\x1b[H")
	for i, l := range u.render(now) {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(clip(l, w-1)) // 留一列，避免写到最后一列触发换行
		b.WriteString("\x1b[K")
	}
	b.WriteString("\x1b[J")
	con.draw(b.String())
}

const (
	cReset  = "\x1b[0m"
	cBold   = "\x1b[1m"
	cGray   = "\x1b[90m"
	cRed    = "\x1b[31m"
	cGreen  = "\x1b[32m"
	cYellow = "\x1b[33m"
	cCyan   = "\x1b[36m"
)

func paint(color, s string) string { return color + s + cReset }

type connSnap struct {
	state     connState
	g         *group
	kind      string
	file      string
	size, cur int64
	addr      string
	until     time.Time
	err       string
}

func (u *tui) render(now time.Time) []string {
	d := u.d
	el := now.Sub(u.start)
	total := d.total.Load()
	avg := float64(total) / max(el.Seconds(), 1e-3)

	// 标题与总体状态
	head := paint(cBold, "traffickiller "+version) + "  运行 " + fmtClock(el)
	if u.dur > 0 {
		head += "  剩余 " + fmtClock(max(u.dur-el, 0))
	}
	lines := []string{
		head + "  " + paint(cGray, u.info),
		fmt.Sprintf("速率 %s  平均 %s  累计 %s  完成 %d 次  连接 %d/%d",
			paint(cBold+cGreen, fmtRate(u.rate)+" ("+fmtBytes(u.rate)+"/s)"),
			fmtRate(avg), paint(cBold, fmtBytes(float64(total))), d.done.Load(), d.active.Load(), len(u.conns)),
	}
	if d.maxTotal > 0 {
		p := float64(total) / float64(d.maxTotal)
		s := fmt.Sprintf("进度 %s %5.1f%%  %s / %s", bar(p, 30), p*100, fmtBytes(float64(total)), fmtBytes(float64(d.maxTotal)))
		if avg > 0 && p < 1 {
			s += "  预计还需 " + fmtClock(time.Duration(float64(d.maxTotal-total)/avg*float64(time.Second)))
		}
		lines = append(lines, s)
	} else if u.dur > 0 {
		p := min(el.Seconds()/u.dur.Seconds(), 1)
		lines = append(lines, fmt.Sprintf("进度 %s %5.1f%%", bar(p, 30), p*100))
	}

	snaps := make([]connSnap, len(u.conns))
	busy := map[*group]int{}
	for i, c := range u.conns {
		c.mu.Lock()
		snaps[i] = connSnap{c.state, c.g, c.kind, c.file, c.size, c.cur.Load(), c.addr, c.until, c.err}
		c.mu.Unlock()
		if s := snaps[i]; s.g != nil && (s.state == stRequest || s.state == stDownload) {
			busy[s.g]++
		}
	}

	// 各游戏
	lines = append(lines, "", paint(cBold, fit("游戏", 10)+fit("速率", 12)+fit("占比", 18)+fit("累计", 12)+fit("连接", 6)+fit("完成次数", 10)+"资源"))
	for i, g := range u.groups {
		name := paint(cCyan, fit(g.name, 10))
		if g.off.Load() {
			name = paint(cRed, fit(g.name+"(停用)", 10))
		}
		share := 0.0
		if u.rate > 0 {
			share = u.rateG[i] / u.rate
		}
		lines = append(lines, name+
			paint(cGreen, fit(fmtRate(u.rateG[i]), 12))+
			fit(fmt.Sprintf("%s %3.0f%%", bar(share, 10), share*100), 18)+
			fit(fmtBytes(float64(g.got.Load())), 12)+
			fit(fmt.Sprint(busy[g]), 6)+
			fit(fmt.Sprint(g.done.Load()), 10)+
			paint(cGray, g.desc))
	}

	// 日志和连接列表按剩余高度分配，高度不够时先不显示日志
	logs := con.recentLogs(4)
	avail := func() int {
		n := u.h - len(lines) - 3 // 空行、表头、底部提示
		if len(logs) > 0 {
			n -= len(logs) + 1
		}
		return n
	}
	rows := avail()
	if len(logs) > 0 && rows < min(len(snaps), 3) {
		logs = nil
		rows = avail()
	}
	if rows < len(snaps) {
		rows = max(rows-1, 1) // 给 "另有 N 个" 留一行
	} else {
		rows = len(snaps)
	}

	// 服务器和属地两列按内容定宽（IPv6 地址较长）
	servers, locs := make([]string, rows), make([]string, rows)
	sw, lw := 8, 6
	for i, s := range snaps[:rows] {
		if host, _, err := net.SplitHostPort(s.addr); err == nil {
			servers[i], locs[i] = s.addr, u.geo.get(host)
			if locs[i] == "" && u.geo != nil {
				locs[i] = "查询中"
			}
		}
		sw, lw = max(sw, width(servers[i])+2), max(lw, width(locs[i])+2)
	}
	sw, lw = min(sw, 42), min(lw, 28)

	lines = append(lines, "", paint(cBold, fit("#", 4)+fit("游戏", 10)+fit("类型", 12)+fit("速率", 12)+
		fit("进度", 18)+fit("服务器", sw)+fit("属地", lw)+"文件"))
	for i, s := range snaps[:rows] {
		var game, kind, prog, file string
		rate := paint(cGreen, fit(fmtRate(u.rateC[i]), 12))
		if u.rateC[i] == 0 {
			rate = paint(cGray, fit("-", 12))
		}
		if s.g != nil {
			game, kind, file = s.g.name, s.kind, s.file
		}
		loc := fit(locs[i], lw)
		if locs[i] == "查询中" {
			loc = paint(cGray, loc)
		}
		switch s.state {
		case stIdle:
			prog = paint(cGray, fit("空闲", 18))
		case stRequest:
			prog = paint(cYellow, fit("请求中", 18))
		case stDownload:
			if s.size > 0 {
				prog = fit(fmt.Sprintf("%3.0f%% %s/%s", float64(s.cur)*100/float64(s.size), fmtShort(s.cur), fmtShort(s.size)), 18)
			} else {
				prog = fit(fmtShort(s.cur), 18)
			}
		case stBackoff:
			prog = paint(cRed, fit(fmt.Sprintf("出错 %ds后重试", int(max(s.until.Sub(now), 0).Seconds()+0.99)), 18))
			file = paint(cRed, s.err)
		}
		lines = append(lines, fit(fmt.Sprint(i+1), 4)+paint(cCyan, fit(game, 10))+fit(kind, 12)+rate+prog+
			fit(servers[i], sw)+loc+file)
	}
	if n := len(snaps) - rows; n > 0 {
		lines = append(lines, paint(cGray, fmt.Sprintf("... 另有 %d 个连接未显示（放大窗口可查看更多）", n)))
	}
	if len(logs) > 0 {
		lines = append(lines, "")
		for _, l := range logs {
			lines = append(lines, paint(cYellow, l))
		}
	}
	lines = append(lines, paint(cGray, "Ctrl+C 退出"))
	if len(lines) > u.h {
		lines = lines[:u.h]
	}
	return lines
}

// fmtShort 紧凑地格式化字节数，如 481K、1.07M、7.45G。
func fmtShort(n int64) string {
	v, i := float64(n), 0
	for v >= 1024 && i < 5 {
		v /= 1024
		i++
	}
	unit := "BKMGTP"[i : i+1]
	switch {
	case i == 0 || v >= 100:
		return fmt.Sprintf("%.0f%s", v, unit)
	case v >= 10:
		return fmt.Sprintf("%.1f%s", v, unit)
	}
	return fmt.Sprintf("%.2f%s", v, unit)
}

// bar 画 ASCII 进度条，如 [#####.....]。
func bar(p float64, n int) string {
	k := int(min(max(p, 0), 1)*float64(n) + 0.5)
	return "[" + strings.Repeat("#", k) + strings.Repeat(".", n-k) + "]"
}

func fmtClock(d time.Duration) string {
	s := int(d.Seconds())
	return fmt.Sprintf("%02d:%02d:%02d", s/3600, s/60%60, s%60)
}

// fit 把 s 截断/补齐到恰好 w 列宽，末尾至少留一个空格作为列间隔。
func fit(s string, w int) string {
	if sw := width(s); sw < w {
		return s + strings.Repeat(" ", w-sw)
	}
	if w < 3 {
		return strings.Repeat(" ", max(w, 0))
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		rw := runeWidth(r)
		if n+rw > w-3 {
			break
		}
		b.WriteRune(r)
		n += rw
	}
	b.WriteString("..")
	return b.String() + strings.Repeat(" ", w-n-2)
}

// clip 把一行截断到 w 列宽，跳过其中的颜色转义序列。
func clip(s string, w int) string {
	var b strings.Builder
	n := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := strings.IndexByte(s[i:], 'm')
			if j < 0 {
				break
			}
			b.WriteString(s[i : i+j+1])
			i += j + 1
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if n += runeWidth(r); n > w {
			break
		}
		b.WriteString(s[i : i+size])
		i += size
	}
	b.WriteString(cReset)
	return b.String()
}

func runeWidth(r rune) int {
	if r >= 0x2E80 {
		return 2
	}
	return 1
}

// width 估算终端显示宽度（中文占两格）。
func width(s string) int {
	n := 0
	for _, r := range s {
		n += runeWidth(r)
	}
	return n
}
