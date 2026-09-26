package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
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
	prefix, suffix string
	names          string
	items          []span
	bytes          int64
}

func (s *source) url(sp span) string {
	return s.prefix + s.names[sp.off:sp.off+sp.n] + s.suffix
}

// sourceBuilder 逐个追加文件来构建 source。
type sourceBuilder struct {
	src   source
	names strings.Builder
}

func newSourceBuilder(prefix, suffix string, sizeHint int) *sourceBuilder {
	b := &sourceBuilder{src: source{prefix: prefix, suffix: suffix, items: make([]span, 0, sizeHint)}}
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
	desc    string
	sources []*source
	count   int // 文件总数
	fails   int // 连续失效 (403/404/410) 次数
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

// pool 随机分配资源：先随机选游戏，再随机选文件。失效的文件/来源会被剔除。
type pool struct {
	mu     sync.Mutex
	groups []*group
}

func (p *pool) pick() (*group, *source, span, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.groups) == 0 {
		return nil, nil, span{}, false
	}
	g := p.groups[rand.IntN(len(p.groups))]
	i := rand.IntN(g.count)
	for _, s := range g.sources {
		if i < len(s.items) {
			return g, s, s.items[i], true
		}
		i -= len(s.items)
	}
	panic("pool: count out of sync")
}

// report 记录下载结果；gone 表示该文件已失效。返回整个来源是否被停用。
func (p *pool) report(g *group, s *source, sp span, gone bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !gone {
		g.fails = 0
		return false
	}
	g.fails++
	if i := slices.Index(s.items, sp); i >= 0 {
		s.items[i] = s.items[len(s.items)-1]
		s.items = s.items[:len(s.items)-1]
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

const stallTimeout = 30 * time.Second

type downloader struct {
	pool      *pool
	lim       *limiter
	plainHTTP bool
	maxTotal  int64
	stop      context.CancelCauseFunc

	total  atomic.Int64
	active atomic.Int32
}

func (d *downloader) worker(ctx context.Context, c *http.Client, ua string) {
	buf := make([]byte, 64<<10)
	backoff := time.Second
	for ctx.Err() == nil {
		g, s, sp, ok := d.pool.pick()
		if !ok {
			d.stop(errNoSource)
			return
		}
		u := s.url(sp)
		if d.plainHTTP && strings.HasPrefix(u, "https://") {
			u = "http://" + u[len("https://"):]
		}
		err := d.fetch(ctx, c, u, ua, buf)
		if ctx.Err() != nil {
			return
		}
		var se statusError
		gone := errors.As(err, &se) && (se == 403 || se == 404 || se == 410)
		if d.pool.report(g, s, sp, gone) {
			con.logf("%s: 资源连续失效 (%v)，已停用该来源", g.name, err)
		}
		if err == nil || gone {
			backoff = time.Second
			continue
		}
		con.logf("%s 下载出错: %v", host(u), shortErr(err))
		if !sleep(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// fetch 下载一个文件，数据读进缓冲区后直接丢弃。
func (d *downloader) fetch(ctx context.Context, c *http.Client, u, ua string, buf []byte) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", ua)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return statusError(resp.StatusCode)
	}

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
		n, err := resp.Body.Read(buf)
		d.lim.refund(len(buf) - n)
		if n > 0 {
			watchdog.Reset(stallTimeout)
			if v := d.total.Add(int64(n)); d.maxTotal > 0 && v >= d.maxTotal {
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
