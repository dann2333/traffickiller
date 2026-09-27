package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
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
		tk, _ := p.pick()
		if p.report(tk, true) {
			t.Fatalf("group dropped too early at %d", i)
		}
	}
	if g.count != 11 {
		t.Fatalf("count = %d, want 11", g.count)
	}
	tk, _ := p.pick()
	p.report(tk, false) // 成功一次会清零连续失败计数
	for range 9 {
		tk, _ := p.pick()
		if p.report(tk, true) {
			t.Fatal("group dropped although failures were not consecutive")
		}
	}
	tk, _ = p.pick()
	if !p.report(tk, true) {
		t.Fatal("group should be dropped after 10 consecutive failures")
	}
	if _, ok := p.pick(); ok {
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
		if tk, _ := p.pick(); tk.s == small {
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
	src, err := loadManifest(context.Background(), srv.Client(), "test", sm)
	if err != nil {
		t.Fatal(err)
	}
	if len(src.items) != 3 || calls.Load() != 2 {
		t.Fatalf("items = %d, calls = %d", len(src.items), calls.Load())
	}
}

// 分块小的游戏和整包大的游戏，按字节算得到的流量应大致相同。
func TestPoolBalancesBytes(t *testing.T) {
	chunks := &group{name: "chunks"}
	b := newSourceBuilder("", "https://a/", "", 1000)
	for i := range 1000 {
		b.add([]byte(fmt.Sprint(i)), 1<<20)
	}
	chunks.add(b.build())
	big := &group{name: "big"}
	b = newSourceBuilder("", "", "", 4).withSizes()
	for i := range 4 {
		b.add([]byte(fmt.Sprint("https://b/", i)), 8<<30)
	}
	big.add(b.build())
	p := newPool([]*group{chunks, big})

	got := map[*group]int64{}
	for range 200000 {
		tk, _ := p.pick()
		n := tk.size
		if _, seg := tk.segment(); seg > 0 {
			n = seg
		} else if n == 0 {
			n = 1 << 20
		}
		got[tk.g] += n
	}
	if r := float64(got[big]) / float64(got[chunks]); r < 0.85 || r > 1.15 {
		t.Fatalf("bytes big/chunks = %.2f, want ~1", r)
	}
}

func TestSegment(t *testing.T) {
	for range 1000 {
		tk := task{size: 8<<30 + 12345}
		off, n := tk.segment()
		if n != segSize || off%(1<<20) != 0 || off < 0 || off+n > tk.size {
			t.Fatalf("segment = %d+%d of %d", off, n, tk.size)
		}
	}
	for _, size := range []int64{0, 1 << 20, segSize} {
		if off, n := (task{size: size}).segment(); off != 0 || n != 0 {
			t.Fatalf("size %d: segment = %d+%d, want whole file", size, off, n)
		}
	}
}

func TestReportKeepsSizesAligned(t *testing.T) {
	g := &group{name: "x"}
	b := newSourceBuilder("", "", "", 3).withSizes()
	for i, n := range []int64{100, 200, 300} {
		b.add([]byte{byte('a' + i)}, n)
	}
	g.add(b.build())
	p := newPool([]*group{g})
	s := g.sources[0]
	p.report(task{g: g, s: s, sp: s.items[0]}, true)
	for i, sp := range s.items {
		if want := int64(s.names[sp.off]-'a'+1) * 100; s.sizes[i] != want {
			t.Fatalf("item %c size = %d, want %d", s.names[sp.off], s.sizes[i], want)
		}
	}
}

// zeros 是不占内存的定长全零内容，供 http.ServeContent 分段读取。
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

func TestFetchSegment(t *testing.T) {
	const size = 100 << 20
	for _, ranged := range []bool{true, false} {
		var rng string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rng = r.Header.Get("Range")
			if !ranged {
				r.Header.Del("Range") // 模拟不支持分段的服务器，返回整个文件
			}
			http.ServeContent(w, r, "f", time.Time{}, &zeros{size: size})
		}))
		d := &downloader{stop: func(error) {}}
		g := &group{}
		st := newConn("ua")
		off, n := int64(5<<20), int64(segSize)
		if err := d.fetch(context.Background(), srv.Client(), srv.URL, off, n, g, st, make([]byte, 64<<10)); err != nil {
			t.Fatal(err)
		}
		srv.Close()
		if want := fmt.Sprintf("bytes=%d-%d", off, off+n-1); rng != want {
			t.Errorf("Range = %q, want %q", rng, want)
		}
		if got := d.total.Load(); got < n || got > n+64<<10 {
			t.Errorf("ranged=%v: downloaded %d bytes, want %d", ranged, got, n)
		}
		if st.size != n {
			t.Errorf("ranged=%v: size = %d, want %d", ranged, st.size, n)
		}
	}
}
