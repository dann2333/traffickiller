package main

// ip2region xdb 格式的内存查询，改写自 ip2region 的 Go binding
// (https://github.com/lionsoul2014/ip2region，Copyright 2022 The Ip2Region Authors，Apache-2.0 OR MIT)。
// 整个数据库放在内存里，只保留按内存查询需要的部分。

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	xdbHeaderLen  = 256
	xdbVectorCols = 256
	xdbVectorSize = 8 // 每格: 起始、结束段索引的偏移，各 4 字节
)

type xdb struct {
	buf   []byte
	bytes int // IP 长度: IPv4 为 4，IPv6 为 16
}

func newXDB(buf []byte) (*xdb, error) {
	if len(buf) < xdbHeaderLen+256*xdbVectorCols*xdbVectorSize {
		return nil, errors.New("xdb 数据不完整")
	}
	x := &xdb{buf: buf, bytes: 4}
	switch v := binary.LittleEndian.Uint16(buf); v {
	case 2: // 旧版结构，只有 IPv4
	case 3:
		switch ipv := binary.LittleEndian.Uint16(buf[16:]); ipv {
		case 4:
		case 6:
			x.bytes = 16
		default:
			return nil, fmt.Errorf("xdb 的 IP 版本 %d 无效", ipv)
		}
	default:
		return nil, fmt.Errorf("不支持的 xdb 结构版本 %d", v)
	}
	return x, nil
}

// search 返回 ip 所在网段的地区信息，如 "中国|广东省|深圳市|电信|CN"，查不到返回空串。
// ip 的长度必须和数据库一致。
func (x *xdb) search(ip []byte) string {
	if len(ip) != x.bytes {
		return ""
	}
	b, n := x.buf, x.bytes
	v := xdbHeaderLen + (int(ip[0])*xdbVectorCols+int(ip[1]))*xdbVectorSize
	sPtr, ePtr := int(binary.LittleEndian.Uint32(b[v:])), int(binary.LittleEndian.Uint32(b[v+4:]))
	if sPtr == 0 || ePtr == 0 {
		return ""
	}
	// 段索引: 起始 IP、结束 IP、地区长度 (2 字节)、地区偏移 (4 字节)
	size := 2*n + 6
	for l, h := 0, (ePtr-sPtr)/size; l <= h; {
		m := (l + h) >> 1
		p := sPtr + m*size
		if p+size > len(b) {
			return ""
		}
		seg := b[p : p+size]
		if x.cmp(ip, seg[:n]) < 0 {
			h = m - 1
		} else if x.cmp(ip, seg[n:2*n]) > 0 {
			l = m + 1
		} else {
			dLen, dPtr := int(binary.LittleEndian.Uint16(seg[2*n:])), int(binary.LittleEndian.Uint32(seg[2*n+2:]))
			if dPtr+dLen > len(b) {
				return ""
			}
			return string(b[dPtr : dPtr+dLen])
		}
	}
	return ""
}

func (x *xdb) cmp(ip, seg []byte) int {
	if x.bytes == 16 {
		return bytes.Compare(ip, seg)
	}
	// IPv4 的段索引按小端序存储
	for i := range 4 {
		if a, b := ip[i], seg[3-i]; a != b {
			if a < b {
				return -1
			}
			return 1
		}
	}
	return 0
}
