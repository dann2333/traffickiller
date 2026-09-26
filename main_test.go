package main

import (
	"bytes"
	"encoding/binary"
	"testing"
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

func TestPoolDropsDeadSource(t *testing.T) {
	parts := &urlParts{}
	g := &group{name: "x"}
	for i := range 20 {
		g.items = append(g.items, item{parts: parts, name: string(rune('a' + i))})
	}
	p := &pool{groups: []*group{g}}
	for i := range 9 {
		_, it, _ := p.pick()
		if p.report(g, it, true) {
			t.Fatalf("group dropped too early at %d", i)
		}
	}
	if len(g.items) != 11 {
		t.Fatalf("items = %d, want 11", len(g.items))
	}
	_, it, _ := p.pick()
	if !p.report(g, it, true) {
		t.Fatal("group should be dropped after 10 consecutive failures")
	}
	if _, _, ok := p.pick(); ok {
		t.Fatal("pool should be empty")
	}
}
