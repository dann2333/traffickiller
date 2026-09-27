package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
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
	sizes          []int64 // 各文件大小，只有整包这类直链来源保存（用于分段下载）
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
	src       source
	names     strings.Builder
	keepSizes bool
}

func newSourceBuilder(label, prefix, suffix string, sizeHint int) *sourceBuilder {
	b := &sourceBuilder{src: source{label: label, prefix: prefix, suffix: suffix, items: make([]span, 0, sizeHint)}}
	b.names.Grow(sizeHint * 50)
	return b
}

// withSizes 让来源记住每个文件的大小，大文件就能随机分段下载。
func (b *sourceBuilder) withSizes() *sourceBuilder {
	b.keepSizes = true
	return b
}

func (b *sourceBuilder) add(name []byte, size int64) {
	b.src.items = append(b.src.items, span{uint32(b.names.Len()), uint32(len(name))})
	b.names.Write(name)
	b.src.bytes += size
	if b.keepSizes {
		b.src.sizes = append(b.src.sizes, size)
	}
}

func (b *sourceBuilder) build() *source {
	b.src.names = b.names.String()
	return &b.src
}

// group 是一个游戏（或自定义 URL 列表）的全部资源。
type group struct {
	name    string
	desc    string
	sources []*source
	count   int     // 文件总数
	fails   int     // 连续失效 (403/404/410) 次数
	weight  float64 // 被选中的权重，见 newPool

	got  atomic.Int64 // 已下载字节
	done atomic.Int64 // 已下载完的文件数
	off  atomic.Bool  // 已停用
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

// segSize 是大文件每次请求下载的长度。整包动辄几 GB，整个下完要几十分钟，
// 连接会长时间停在同一个游戏上；随机取一段下载，各游戏才能均衡。
const segSize = 32 << 20

// task 是一次下载：一个文件，或大文件中的一段。
type task struct {
	g    *group
	s    *source
	sp   span
	size int64 // 文件大小，未知为 0
}

// segment 为大文件随机选一段（按 1MB 对齐，便于 CDN 缓存），返回起点和长度；
// 小文件或大小未知时返回 0, 0，表示下载整个文件。
func (t task) segment() (off, n int64) {
	if t.size <= segSize {
		return 0, 0
	}
	return rand.Int64N((t.size-segSize)>>20+1) << 20, segSize
}

// pool 随机分配资源：先按权重选游戏，再随机选文件。失效的文件/来源会被剔除。
type pool struct {
	mu     sync.Mutex
	groups []*group
}

// newPool 按每次请求的平均字节数给游戏加权（权重与之成反比），
// 这样分块很小的游戏和整包很大的游戏得到的流量大致相同。
func newPool(groups []*group) *pool {
	for _, g := range groups {
		var sum int64
		for _, s := range g.sources {
			if s.sizes == nil {
				sum += s.bytes
				continue
			}
			for _, n := range s.sizes {
				sum += min(n, segSize)
			}
		}
		avg := float64(sum) / float64(max(g.count, 1))
		if avg <= 0 {
			avg = 1 << 20 // 大小未知（自定义 URL）
		}
		g.weight = 1 / avg
	}
	return &pool{groups: slices.Clone(groups)}
}

func (p *pool) pick() (task, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.groups) == 0 {
		return task{}, false
	}
	var sum float64
	for _, g := range p.groups {
		sum += g.weight
	}
	g := p.groups[len(p.groups)-1]
	r := rand.Float64() * sum
	for _, x := range p.groups {
		if r < x.weight {
			g = x
			break
		}
		r -= x.weight
	}
	i := rand.IntN(g.count)
	for _, s := range g.sources {
		if i < len(s.items) {
			t := task{g: g, s: s, sp: s.items[i]}
			if s.sizes != nil {
				t.size = s.sizes[i]
			}
			return t, true
		}
		i -= len(s.items)
	}
	panic("pool: count out of sync")
}

// report 记录下载结果；gone 表示该文件已失效。返回整个来源是否被停用。
func (p *pool) report(t task, gone bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	g, s := t.g, t.s
	if !gone {
		g.fails = 0
		return false
	}
	g.fails++
	if i := slices.Index(s.items, t.sp); i >= 0 {
		last := len(s.items) - 1
		s.items[i] = s.items[last]
		s.items = s.items[:last]
		if s.sizes != nil {
			s.sizes[i] = s.sizes[last]
			s.sizes = s.sizes[:last]
		}
		g.count--
		if len(s.items) == 0 {
			g.sources = slices.DeleteFunc(g.sources, func(x *source) bool { return x == s })
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

func (d *downloader) worker(ctx context.Context, c *http.Client, st *conn) {
	buf := make([]byte, 64<<10)
	backoff := time.Second
	for ctx.Err() == nil {
		t, ok := d.pool.pick()
		if !ok {
			d.stop(errNoSource)
			return
		}
		g := t.g
		u := t.s.url(t.sp)
		if d.plainHTTP && strings.HasPrefix(u, "https://") {
			u = "http://" + u[len("https://"):]
		}
		off, n := t.segment()
		file := t.s.name(t.sp)
		if n > 0 {
			file += " @" + fmtShort(off)
		}
		st.cur.Store(0)
		st.set(func(st *conn) {
			st.state, st.g, st.kind, st.file, st.size, st.err = stRequest, g, t.s.label, file, -1, ""
		})
		err := d.fetch(ctx, c, u, off, n, g, st, buf)
		if ctx.Err() != nil {
			return
		}
		var se statusError
		gone := errors.As(err, &se) && (se == 403 || se == 404 || se == 410)
		if d.pool.report(t, gone) {
			con.logf("%s: 资源连续失效 (%v)，已停用该来源", g.name, err)
		}
		if err == nil {
			d.done.Add(1)
			g.done.Add(1)
		}
		if err == nil || gone {
			backoff = time.Second
			continue
		}
		e := shortErr(err).Error()
		con.logf("%s 下载出错: %s", host(u), e)
		st.set(func(st *conn) { st.state, st.until, st.err = stBackoff, time.Now().Add(backoff), e })
		if !sleep(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// fetch 下载一个文件（n > 0 时只下载从 off 开始的 n 字节），数据读进缓冲区后直接丢弃。
func (d *downloader) fetch(ctx context.Context, c *http.Client, u string, off, n int64, g *group, st *conn, buf []byte) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, st.trace), http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", st.ua)
	if n > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+n-1))
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return statusError(resp.StatusCode)
	}
	size := resp.ContentLength
	if n > 0 && resp.StatusCode == http.StatusOK {
		size = n // 服务器不支持分段、返回了整个文件：读够 n 字节就断开
	}
	st.set(func(st *conn) { st.state, st.size = stDownload, size })

	d.active.Add(1)
	defer d.active.Add(-1)
	watchdog := time.AfterFunc(stallTimeout, func() { cancel(errStalled) })
	defer watchdog.Stop()
	var got int64
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
			if got += int64(k); n > 0 && got >= n {
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
