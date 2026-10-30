package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
)

func TestParseRate(t *testing.T) {
	cases := map[string]float64{
		"0":       0,
		"100M":    100e6 / 8,
		"100Mbps": 100e6 / 8,
		"100mbit": 100e6 / 8,
		"1G":      1e9 / 8,
		"500k":    500e3 / 8,
		"12MB":    12 << 20,
		"12MB/s":  12 << 20,
		"1.5GB":   1.5 * (1 << 30),
		"2MiB/s":  2 << 20,
	}
	for in, want := range cases {
		got, err := parseRate(in)
		if err != nil || got != want {
			t.Errorf("parseRate(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "abc", "10X", "-5M"} {
		if _, err := parseRate(in); err == nil {
			t.Errorf("parseRate(%q) should fail", in)
		}
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string]int64{
		"0":     0,
		"500G":  500 << 30,
		"500GB": 500 << 30,
		"1.5T":  3 << 39,
		"800MB": 800 << 20,
		"1GiB":  1 << 30,
		"1024":  1024,
	}
	for in, want := range cases {
		got, err := parseSize(in)
		if err != nil || got != want {
			t.Errorf("parseSize(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
}

func TestVerLess(t *testing.T) {
	for _, c := range [][2]string{{"4.10.0", "4.9.0"}, {"7.1.0", "7.0.0"}, {"1.0.1", "1.0.0"}, {"10.0.0", "9.9.9"}} {
		if !verLess(c[1], c[0]) || verLess(c[0], c[1]) {
			t.Errorf("expected %s < %s", c[1], c[0])
		}
	}
}

// pb 拼接 protobuf 字段，用于构造测试清单。
func pbStr(field int, b []byte) []byte {
	out := binary.AppendUvarint(nil, uint64(field<<3|2))
	out = binary.AppendUvarint(out, uint64(len(b)))
	return append(out, b...)
}

func pbVarint(field int, v uint64) []byte {
	return binary.AppendUvarint(binary.AppendUvarint(nil, uint64(field<<3)), v)
}

func TestParseManifest(t *testing.T) {
	chunk := func(name string, size uint64) []byte {
		return bytes.Join([][]byte{pbStr(1, []byte(name)), pbStr(2, []byte("md5")), pbVarint(3, 7), pbVarint(4, size), pbVarint(5, size+1)}, nil)
	}
	asset := func(name string, chunks ...[]byte) []byte {
		b := pbStr(1, []byte(name))
		for _, c := range chunks {
			b = append(b, pbStr(2, c)...)
		}
		return append(b, pbVarint(4, 123)...)
	}
	var m []byte
	m = append(m, pbStr(1, asset("a.pck", chunk("c1", 100), chunk("c2", 200)))...)
	m = append(m, pbVarint(9, 1)...) // 未知字段应被跳过
	m = append(m, pbStr(1, asset("b.pck", chunk("c3", 300)))...)

	got := map[string]int64{}
	if err := parseManifest(bytes.NewReader(m), func(name []byte, size int64) { got[string(name)] = size }); err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"c1": 100, "c2": 200, "c3": 300}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("chunk %s size = %d, want %d", k, got[k], v)
		}
	}
	if err := parseManifest(bytes.NewReader(m[:len(m)-3]), func([]byte, int64) {}); err == nil {
		t.Error("truncated manifest should fail")
	}
}

func TestSourceBuilder(t *testing.T) {
	b := newSourceBuilder("", "https://cdn/x/", "?s", 2)
	b.add([]byte("aa"), 10)
	b.add([]byte("bbb"), 20)
	src := b.build()
	if got := src.url(src.items[1]); got != "https://cdn/x/bbb?s" {
		t.Fatalf("url = %q", got)
	}
	if src.bytes != 30 || len(src.items) != 2 {
		t.Fatalf("bytes = %d, items = %d", src.bytes, len(src.items))
	}
}

func TestPoolDropsDeadSource(t *testing.T) {
	g := &group{name: "x"}
	for range 2 {
		b := newSourceBuilder("", "", "", 0)
		for i := range 10 {
			b.add([]byte{byte('a' + i)}, 1)
		}
		g.add(b.build())
	}
	p := newPool([]*group{g})
	for i := range 9 {
		tk, _ := p.pick(nil)
		if p.report(tk, true) {
			t.Fatalf("group dropped too early at %d", i)
		}
	}
	if g.count != 11 {
		t.Fatalf("count = %d, want 11", g.count)
	}
	tk, _ := p.pick(nil)
	p.report(tk, false) // 成功一次会清零连续失败计数
	for range 9 {
		tk, _ := p.pick(nil)
		if p.report(tk, true) {
			t.Fatal("group dropped although failures were not consecutive")
		}
	}
	tk, _ = p.pick(nil)
	if !p.report(tk, true) {
		t.Fatal("group should be dropped after 10 consecutive failures")
	}
	if _, ok := p.pick(nil); ok {
		t.Fatal("pool should be empty")
	}
}

func TestPoolEmptiesSource(t *testing.T) {
	g := &group{name: "x"}
	for _, n := range []int{1, 3} {
		b := newSourceBuilder("", "", "", 0)
		for i := range n {
			b.add([]byte{byte('a' + i)}, 1)
		}
		g.add(b.build())
	}
	p := newPool([]*group{g})
	small := g.sources[0]
	p.report(task{g: g, s: small, sp: small.items[0]}, true)
	if len(g.sources) != 1 || g.count != 3 {
		t.Fatalf("sources = %d, count = %d", len(g.sources), g.count)
	}
	for range 100 {
		if tk, _ := p.pick(nil); tk.s == small {
			t.Fatal("picked from emptied source")
		}
	}
}

func TestSourceName(t *testing.T) {
	b := newSourceBuilder("", "", "", 2)
	b.add([]byte("https://cdn.example.com/a/b/game_1.0.zip.001?x=1"), 1)
	b.add([]byte("https://cdn.example.com/dir/"), 1)
	src := b.build()
	if got := src.name(src.items[0]); got != "game_1.0.zip.001" {
		t.Errorf("name = %q", got)
	}
	if got := src.name(src.items[1]); got != "dir/" {
		t.Errorf("name = %q", got)
	}
	c := newSourceBuilder("本体", "https://cdn/chunks/", "", 1)
	c.add([]byte("4af6307d_a1c9"), 1)
	if cs := c.build(); cs.name(cs.items[0]) != "4af6307d_a1c9" {
		t.Errorf("chunk name = %q", cs.name(cs.items[0]))
	}
}

func TestFitClip(t *testing.T) {
	cases := []struct {
		in   string
		w    int
		want string
	}{
		{"abc", 5, "abc  "},
		{"abcde", 5, "ab.. "},
		{"原神", 6, "原神  "},
		{"星穹铁道", 8, "星穹..  "},
		{"x", 1, " "},
	}
	for _, c := range cases {
		if got := fit(c.in, c.w); got != c.want || width(got) != c.w {
			t.Errorf("fit(%q, %d) = %q, want %q", c.in, c.w, got, c.want)
		}
	}
	if got := clip(paint(cRed, "原神abc"), 5); got != cRed+"原神a"+cReset {
		t.Errorf("clip = %q", got)
	}
}

func TestFmtShort(t *testing.T) {
	for n, want := range map[int64]string{0: "0B", 1000: "1000B", 1536: "1.50K", 500 << 10: "500K", 12 << 20: "12.0M", 7 << 30: "7.00G"} {
		if got := fmtShort(n); got != want {
			t.Errorf("fmtShort(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestGeo(t *testing.T) {
	for in, want := range map[string]string{
		"中国|辽宁省|本溪市|联通|CN":                                              "辽宁本溪 联通",
		"中国|广西|北海市|中国电信|CN":                                             "广西北海 电信",
		"中国|上海市|上海市|电信|CN":                                              "上海 电信",
		"中国|香港特别行政区|0|电讯盈科|CN":                                          "香港 电讯盈科",
		"中国|0|0|0|CN":                                                   "中国",
		"Japan|Tokyo|Tokyo|WIDE Project|JP":                             "日本 Tokyo WIDE Project",
		"United States|California|Los Angeles|AT&T Enterprises, LLC|US": "美国 Los Angeles AT&T Enterprises",
		"Australia|Queensland|0|0|AU":                                   "澳大利亚 Queensland",
		"Atlantis|0|0|0|XX":                                             "Atlantis",
		"Reserved|Reserved|Reserved|0|0":                                "保留地址",
		"":                                                              "",
	} {
		if got := formatRegion(in); got != want {
			t.Errorf("formatRegion(%q) = %q, want %q", in, got, want)
		}
	}
	var g *geoCache
	if g.get("127.0.0.1") != "本机" || g.get("192.168.1.1") != "局域网" || g.get("fe80::1%eth0") != "局域网" || g.get("1.1.1.1") != "" {
		t.Error("geo.get special addresses")
	}

	// 内嵌的完整数据库
	g = newGeoCache()
	for ip, want := range map[string]string{
		"1.0.1.1":     "福建福州 电信",
		"2001:200::1": "日本 Tokyo WIDE Project",
	} {
		deadline := time.Now().Add(10 * time.Second)
		got := g.get(ip)
		for got == "" && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
			got = g.get(ip)
		}
		if got != want {
			t.Errorf("geo.get(%q) = %q, want %q", ip, got, want)
		}
	}
}

// 第一次请求发一半数据后卡住，应在 stallTimeout 后放弃并重试成功。
func TestManifestStallRetry(t *testing.T) {
	defer func(d time.Duration) { stallTimeout = d }(stallTimeout)
	stallTimeout = 300 * time.Millisecond
	var m []byte
	for _, n := range []string{"c1", "c2", "c3"} {
		m = append(m, pbStr(1, append(pbStr(1, []byte("f")), pbStr(2, append(pbStr(1, []byte(n)), pbVarint(4, 10)...))...))...)
	}
	var calls atomic.Int32
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "gzip,deflate,br" {
			return // 忽略打到这个端口上的无关探测
		}
		if calls.Add(1) == 1 {
			w.Write(m[:len(m)/2])
			w.(http.Flusher).Flush()
			<-done
			return
		}
		w.Write(m)
	}))
	defer srv.Close()
	defer close(done) // 先放开卡住的请求，srv.Close 才能返回

	var sm sophonManifest
	sm.Manifest.ID = "x"
	sm.ManifestDL.URLPrefix = srv.URL
	sm.ChunkDL.URLPrefix = "https://cdn"
	src, err := loadManifest(context.Background(), &fhttp.Client{Transport: &fhttp.Transport{DisableCompression: true}}, "test", sm)
	if err != nil {
		t.Fatal(err)
	}
	if len(src.items) != 3 || calls.Load() != 2 {
		t.Fatalf("items = %d, calls = %d", len(src.items), calls.Load())
	}
}

// 同一游戏按清单顺序领整份文件，走完再回头。大文件也不切段。
func TestPoolSequential(t *testing.T) {
	g := &group{name: "g"}
	b := newSourceBuilder("整包", "", "", 2)
	b.add([]byte("https://cdn/a.zip"), 80<<20)
	b.add([]byte("https://cdn/b.zip"), 10<<20)
	g.add(b.build())
	c := newSourceBuilder("本体", "https://cdn/", "", 2)
	c.add([]byte("c0"), 1<<20)
	c.add([]byte("c1"), 1<<20)
	g.add(c.build())
	p := newPool([]*group{g})

	var got []string
	for range 4 {
		tk, ok := p.pick(g)
		if !ok {
			t.Fatal("pick")
		}
		got = append(got, tk.s.name(tk.sp))
	}
	want := []string{"a.zip", "b.zip", "c0", "c1"}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("step %d = %s, want %s", i, got[i], w)
		}
	}
	tk, ok := p.pick(g)
	if !ok || tk.s.name(tk.sp) != "a.zip" {
		t.Fatalf("wrap = %s ok=%v, want a.zip", tk.s.name(tk.sp), ok)
	}

	// 两条连接绑定不同游戏时，各自按自己的清单往下走，不会交叉跳文件。
	g1, g2 := &group{name: "g1"}, &group{name: "g2"}
	b1 := newSourceBuilder("", "", "", 2)
	b1.add([]byte("https://a/1"), 1)
	b1.add([]byte("https://a/2"), 1)
	g1.add(b1.build())
	b2 := newSourceBuilder("", "", "", 2)
	b2.add([]byte("https://b/1"), 1)
	b2.add([]byte("https://b/2"), 1)
	g2.add(b2.build())
	p = newPool([]*group{g1, g2})
	for _, w := range []string{"https://a/1", "https://a/2", "https://a/1"} {
		tk, _ := p.pick(g1)
		if tk.s.url(tk.sp) != w {
			t.Fatalf("g1 pick = %s, want %s", tk.s.url(tk.sp), w)
		}
	}
	tk, _ = p.pick(g2)
	if tk.s.url(tk.sp) != "https://b/1" {
		t.Fatalf("g2 pick = %s", tk.s.url(tk.sp))
	}
}

// 网络错误把同一个文件放回队列，下次仍是这个文件，而不是跳到下一个。
func TestPoolRequeue(t *testing.T) {
	g := &group{name: "g"}
	b := newSourceBuilder("", "", "", 2)
	b.add([]byte("https://cdn/1"), 1)
	b.add([]byte("https://cdn/2"), 1)
	g.add(b.build())
	p := newPool([]*group{g})
	first, _ := p.pick(g)
	p.requeue(first)
	got, _ := p.pick(g)
	if got.s.url(got.sp) != "https://cdn/1" {
		t.Fatalf("retry = %s, want https://cdn/1", got.s.url(got.sp))
	}
	got, _ = p.pick(g)
	if got.s.url(got.sp) != "https://cdn/2" {
		t.Fatalf("next = %s, want https://cdn/2", got.s.url(got.sp))
	}
}

// zeros 是不占内存的定长全零内容，供 http.ServeContent 读取。
type zeros struct{ pos, size int64 }

func (z *zeros) Read(p []byte) (int, error) {
	if z.pos >= z.size {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), z.size-z.pos))
	clear(p[:n])
	z.pos += int64(n)
	return n, nil
}

func (z *zeros) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		z.pos = off
	case io.SeekCurrent:
		z.pos += off
	case io.SeekEnd:
		z.pos = z.size + off
	}
	return z.pos, nil
}

func TestFetchWholeFile(t *testing.T) {
	const size = 2 << 20
	var rng string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng = r.Header.Get("Range")
		http.ServeContent(w, r, "f", time.Time{}, &zeros{size: size})
	}))
	defer srv.Close()
	d := &downloader{stop: func(error) {}}
	g := &group{}
	st := newConn("ua")
	if err := d.fetch(context.Background(), &fhttp.Client{Transport: &fhttp.Transport{DisableCompression: true}}, srv.URL, g, st, make([]byte, 64<<10)); err != nil {
		t.Fatal(err)
	}
	if rng != "" {
		t.Errorf("Range = %q, want none", rng)
	}
	if got := d.total.Load(); got != size {
		t.Errorf("downloaded %d bytes, want %d", got, size)
	}
	if st.size != size {
		t.Errorf("size = %d, want %d", st.size, size)
	}
}
