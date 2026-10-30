package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/fhttp/httptrace"
)

// span 指向 source.names 里的一段，即一个文件名（或完整 URL）。
type span struct{ off, n uint32 }

// source 是一批共享 URL 前后缀的文件。几十万个文件名紧凑地存放在一个字符串里，
// 每个文件只额外占 8 字节，保留全部分块也只需十几 MB 内存。
type source struct {
	label          string // 界面上显示的类型，如 本体 / 语音 zh-cn / 整包
	prefix, suffix string
	names          string
	items          []span
	bytes          int64
}

func (s *source) url(sp span) string {
	return s.prefix + s.names[sp.off:sp.off+sp.n] + s.suffix
}

// name 返回界面上显示的文件名：分块名，或完整 URL 的最后一段。
func (s *source) name(sp span) string {
	n := s.names[sp.off : sp.off+sp.n]
	if s.prefix == "" {
		if i := strings.IndexAny(n, "?#"); i >= 0 {
			n = n[:i]
		}
		n = n[strings.LastIndexByte(strings.TrimRight(n, "/"), '/')+1:]
	}
	return n
}

// sourceBuilder 逐个追加文件来构建 source。
type sourceBuilder struct {
	src   source
	names strings.Builder
}

func newSourceBuilder(label, prefix, suffix string, sizeHint int) *sourceBuilder {
	b := &sourceBuilder{src: source{label: label, prefix: prefix, suffix: suffix, items: make([]span, 0, sizeHint)}}
	b.names.Grow(sizeHint * 50)
	return b
}

func (b *sourceBuilder) add(name []byte, size int64) {
	b.src.items = append(b.src.items, span{uint32(b.names.Len()), uint32(len(name))})
	b.names.Write(name)
	b.src.bytes += size
}

func (b *sourceBuilder) build() *source {
	b.src.names = b.names.String()
	return &b.src
}

// group 是一个游戏（或自定义 URL 列表）的全部资源。
type group struct {
	name    string
	desc    atomic.Value // string，界面上的说明；刷新时会换
	sources []*source
	count   int // 文件总数
	fails   int // 连续失效 (403/404/410) 次数
	seq     seqCur

	got  atomic.Int64 // 已下载字节
	done atomic.Int64 // 已下载完的文件数
	off  atomic.Bool  // 已停用
}

// seqCur 是该游戏在清单上的位置。多个连接共享同一个游标，各自领走下一个整文件，
// 和启动器按 chunk_concurrent_tasks 并行下载完整分块一样。
type seqCur struct {
	si, ii int
}

func (g *group) setDesc(s string) { g.desc.Store(s) }

func (g *group) getDesc() string {
	v, _ := g.desc.Load().(string)
	return v
}

func (g *group) add(s *source) {
	if len(s.items) > 0 {
		g.sources = append(g.sources, s)
		g.count += len(s.items)
	}
}

func (g *group) bytes() (n int64) {
	for _, s := range g.sources {
		n += s.bytes
	}
	return n
}

// task 是一次下载：清单里的一个文件，整份获取，不切 Range。
type task struct {
	g  *group
	s  *source
	sp span
}

// pool 按清单顺序发任务。每个游戏有自己的游标；连接优先只领自己绑定的游戏，
// 走完一遍后从头再来（持续消耗流量）。失效的文件会被剔除，游标跟着修正。
type pool struct {
	mu     sync.Mutex
	groups []*group // 当前还能领任务的
	stable []*group // 启动时的游戏对象，刷新时按名字找回，界面和连接都拿着这些指针
	retry  []task
}

func newPool(groups []*group) *pool {
	return &pool{groups: slices.Clone(groups), stable: slices.Clone(groups)}
}

// adopt 用新拉到的清单换掉同名游戏的文件。游戏对象不变，所以界面和已绑定的连接不用改。
// 清单没有变化时不动游标，返回 false。已经停用的游戏在清单更新后会重新启用。
func (p *pool) adopt(fresh *group) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	var g *group
	for _, x := range p.stable {
		if x.name == fresh.name {
			g = x
			break
		}
	}
	if g == nil || sameList(g, fresh) {
		return false
	}
	g.sources = fresh.sources
	g.count = fresh.count
	g.seq = seqCur{}
	g.fails = 0
	g.setDesc(fresh.getDesc())
	g.off.Store(false)
	if slices.Index(p.groups, g) < 0 {
		p.groups = append(p.groups, g)
	}
	return true
}

func sameList(a, b *group) bool {
	if a.getDesc() != b.getDesc() || a.count != b.count || len(a.sources) != len(b.sources) {
		return false
	}
	for i := range a.sources {
		as, bs := a.sources[i], b.sources[i]
		if as.label != bs.label || as.prefix != bs.prefix || as.suffix != bs.suffix || as.names != bs.names {
			return false
		}
	}
	return true
}

// pick 领取下一次下载。prefer 非空时优先领这个游戏的下一段（连接和游戏绑定，
// 同一条连接尽量打同一个 CDN）；这个游戏没了再改领别的。
func (p *pool) pick(prefer *group) (task, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t, ok := p.takeRetry(prefer); ok {
		return t, true
	}
	if prefer != nil && !prefer.off.Load() {
		if t, ok := p.next(prefer); ok {
			return t, true
		}
	}
	if t, ok := p.takeRetry(nil); ok {
		return t, true
	}
	for _, g := range p.groups {
		if t, ok := p.next(g); ok {
			return t, true
		}
	}
	return task{}, false
}

// requeue 把没下完的同一个文件放回队首，下次仍从这里续，而不是跳去别的文件。
func (p *pool) requeue(t task) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.alive(t) || len(p.retry) >= 4096 {
		return
	}
	p.retry = append(p.retry, t)
}

func (p *pool) takeRetry(prefer *group) (task, bool) {
	for i := 0; i < len(p.retry); {
		t := p.retry[i]
		if !p.alive(t) {
			p.retry = slices.Delete(p.retry, i, i+1)
			continue
		}
		if prefer != nil && t.g != prefer {
			i++
			continue
		}
		p.retry = slices.Delete(p.retry, i, i+1)
		return t, true
	}
	return task{}, false
}

func (p *pool) alive(t task) bool {
	if t.g == nil || t.s == nil || t.g.off.Load() || slices.Index(p.groups, t.g) < 0 {
		return false
	}
	if slices.Index(t.g.sources, t.s) < 0 {
		return false
	}
	return slices.Index(t.s.items, t.sp) >= 0
}

// next 沿清单往前领下一个文件，整份下载。清单走完就回到开头。
func (p *pool) next(g *group) (task, bool) {
	if g.count == 0 || len(g.sources) == 0 {
		return task{}, false
	}
	c := &g.seq
	for range g.count + len(g.sources) + 1 {
		if c.si >= len(g.sources) {
			c.si, c.ii = 0, 0
		}
		s := g.sources[c.si]
		if c.ii >= len(s.items) {
			c.si++
			c.ii = 0
			continue
		}
		sp := s.items[c.ii]
		c.ii++
		return task{g: g, s: s, sp: sp}, true
	}
	return task{}, false
}

// report 记录下载结果；gone 表示该文件已失效。返回整个来源是否被停用。
func (p *pool) report(t task, gone bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	g, s := t.g, t.s
	si := slices.Index(g.sources, s)
	if si < 0 {
		return false // 清单已经刷新，这是上一份列表里的文件
	}
	if !gone {
		g.fails = 0
		return false
	}
	g.fails++
	if i := slices.Index(s.items, t.sp); i >= 0 {
		s.items = slices.Delete(s.items, i, i+1)
		g.count--
		if si >= 0 {
			g.noteRemoved(si, i)
		}
		if len(s.items) == 0 && si >= 0 {
			g.sources = slices.Delete(g.sources, si, si+1)
			g.noteSourceRemoved(si)
		}
	}
	if g.count > 0 && g.fails < 10 {
		return false
	}
	if i := slices.Index(p.groups, g); i >= 0 {
		p.groups = slices.Delete(p.groups, i, i+1)
		g.off.Store(true)
		return true
	}
	return false
}

// noteRemoved 在清单里删掉第 i 个文件后修正游标，避免跳过或重复错位。
func (g *group) noteRemoved(si, i int) {
	c := &g.seq
	if c.si != si {
		return
	}
	if c.ii > i {
		c.ii--
	}
}

func (g *group) noteSourceRemoved(si int) {
	c := &g.seq
	if c.si > si {
		c.si--
	} else if c.si == si {
		c.ii = 0
	}
}

// limiter 是全局令牌桶（虚拟时钟实现），所有并发共享总带宽。
type limiter struct {
	mu        sync.Mutex
	nsPerByte float64
	next      time.Time
}

func newLimiter(bytesPerSec float64) *limiter {
	if bytesPerSec <= 0 {
		return nil
	}
	return &limiter{nsPerByte: 1e9 / bytesPerSec}
}

// wait 预留 n 字节的额度，必要时等待。
func (l *limiter) wait(ctx context.Context, n int) error {
	if l == nil {
		return nil
	}
	now := time.Now()
	l.mu.Lock()
	if floor := now.Add(-100 * time.Millisecond); l.next.Before(floor) {
		l.next = floor // 允许最多 100ms 的突发，用来吸收 sleep 误差
	}
	l.next = l.next.Add(time.Duration(float64(n) * l.nsPerByte))
	d := l.next.Sub(now)
	l.mu.Unlock()
	if d <= 0 {
		return nil
	}
	if !sleep(ctx, d) {
		return ctx.Err()
	}
	return nil
}

// refund 退还预留了但没用上的额度。
func (l *limiter) refund(n int) {
	if l == nil || n <= 0 {
		return
	}
	l.mu.Lock()
	l.next = l.next.Add(-time.Duration(float64(n) * l.nsPerByte))
	l.mu.Unlock()
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

type statusError int

func (e statusError) Error() string { return fmt.Sprintf("HTTP %d", int(e)) }

var errStalled = errors.New("30 秒未收到数据")

var stallTimeout = 30 * time.Second // 测试里会调小

type downloader struct {
	pool      *pool
	lim       *limiter
	plainHTTP bool
	maxTotal  int64
	stop      context.CancelCauseFunc

	total  atomic.Int64
	done   atomic.Int64 // 已下载完的文件数
	active atomic.Int32
}

type connState int

const (
	stIdle     connState = iota
	stRequest            // 已发出请求，等待响应
	stDownload           // 正在接收数据
	stBackoff            // 出错后等待重试
)

// conn 是一个下载连接的实时状态，供界面展示。
type conn struct {
	ua    string
	trace *httptrace.ClientTrace

	n   atomic.Int64 // 该连接累计下载字节
	cur atomic.Int64 // 当前文件已下载字节

	mu    sync.Mutex
	state connState
	g     *group
	kind  string    // 资源类型
	file  string    // 文件名
	size  int64     // 当前文件大小，未知为 -1
	addr  string    // 服务器 IP:端口（经代理时是代理的地址）
	until time.Time // 重试等待截止时间
	err   string    // 最近一次错误
}

func newConn(ua string) *conn {
	st := &conn{ua: ua}
	st.trace = &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) {
		addr := i.Conn.RemoteAddr().String()
		st.set(func(st *conn) { st.addr = addr })
	}}
	return st
}

func (st *conn) set(f func(st *conn)) {
	st.mu.Lock()
	f(st)
	st.mu.Unlock()
}

func (d *downloader) worker(ctx context.Context, c doer, st *conn, prefer *group) {
	buf := make([]byte, 64<<10)
	backoff := time.Second
	for ctx.Err() == nil {
		t, ok := d.pool.pick(prefer)
		if !ok {
			d.stop(errNoSource)
			return
		}
		g := t.g
		u := t.s.url(t.sp)
		if d.plainHTTP && strings.HasPrefix(u, "https://") {
			u = "http://" + u[len("https://"):]
		}
		file := t.s.name(t.sp)
		st.cur.Store(0)
		st.set(func(st *conn) {
			st.state, st.g, st.kind, st.file, st.size, st.err = stRequest, g, t.s.label, file, -1, ""
		})
		err := d.fetch(ctx, c, u, g, st, buf)
		if ctx.Err() != nil {
			return
		}
		var se statusError
		gone := errors.As(err, &se) && (se == 403 || se == 404 || se == 410)
		if err != nil && !gone {
			d.pool.requeue(t)
			e := shortErr(err).Error()
			con.logf("%s 下载出错: %s", host(u), e)
			st.set(func(st *conn) { st.state, st.until, st.err = stBackoff, time.Now().Add(backoff), e })
			if !sleep(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		if d.pool.report(t, gone) {
			con.logf("%s: 资源连续失效 (%v)，已停用该来源", g.name, err)
		}
		if err == nil {
			d.done.Add(1)
			g.done.Add(1)
		}
		backoff = time.Second
	}
}

// fetch 整份下载一个文件，数据读进缓冲区后直接丢弃。不发 Range：启动器只有断点续传才带，这里不落盘。
func (d *downloader) fetch(ctx context.Context, c doer, u string, g *group, st *conn, buf []byte) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	req, err := fhttp.NewRequestWithContext(httptrace.WithClientTrace(ctx, st.trace), fhttp.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header = launcherHeaders(st.ua, "")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != fhttp.StatusOK && resp.StatusCode != fhttp.StatusPartialContent {
		return statusError(resp.StatusCode)
	}
	size := resp.ContentLength
	st.set(func(st *conn) { st.state, st.size = stDownload, size })

	d.active.Add(1)
	defer d.active.Add(-1)
	watchdog := time.AfterFunc(stallTimeout, func() { cancel(errStalled) })
	defer watchdog.Stop()
	for {
		// 先预留额度再读，读完退还多余部分，总速率不会超出上限。
		if d.lim != nil {
			watchdog.Stop() // 限速等待不算“卡住”
			if d.lim.wait(ctx, len(buf)) != nil {
				return context.Cause(ctx)
			}
			watchdog.Reset(stallTimeout)
		}
		k, err := resp.Body.Read(buf)
		d.lim.refund(len(buf) - k)
		if k > 0 {
			watchdog.Reset(stallTimeout)
			st.n.Add(int64(k))
			st.cur.Add(int64(k))
			g.got.Add(int64(k))
			if v := d.total.Add(int64(k)); d.maxTotal > 0 && v >= d.maxTotal {
				d.stop(errTotalReached)
				return nil
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			return err
		}
	}
}

func host(u string) string {
	if p, err := url.Parse(u); err == nil {
		return p.Host
	}
	return u
}

func shortErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
