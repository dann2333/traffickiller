package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

// 资源列表来自 https://hoyo-files.amarea.cn/ ，实际文件都在米哈游官方 CDN 上。
const apiBase = "https://autopatch.amarea.cn/pkg_version"

// 同时下载解析的清单数上限，避免多个 zstd 解码器同时占用大量内存。
var manifestSem = make(chan struct{}, 2)

var gameIDs = []string{"hk4e", "hkrpg", "nap", "bh3"}

var gameNames = map[string]string{"hk4e": "原神", "hkrpg": "星穹铁道", "nap": "绝区零", "bh3": "崩坏3"}

var gameAlias = map[string]string{
	"ys": "hk4e", "genshin": "hk4e", "原神": "hk4e",
	"sr": "hkrpg", "starrail": "hkrpg", "星铁": "hkrpg", "星穹铁道": "hkrpg",
	"zzz": "nap", "绝区零": "nap",
	"honkai": "bh3", "崩坏3": "bh3",
}

func parseGames(s string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, g := range strings.Split(s, ",") {
		g = strings.ToLower(strings.TrimSpace(g))
		if g == "" {
			continue
		}
		if g == "all" {
			return gameIDs, nil
		}
		if a, ok := gameAlias[g]; ok {
			g = a
		}
		if _, ok := gameNames[g]; !ok {
			return nil, fmt.Errorf("未知游戏 %q，可选: %s", g, strings.Join(gameIDs, ","))
		}
		if !seen[g] {
			seen[g] = true
			out = append(out, g)
		}
	}
	return out, nil
}

// loadGames 并行获取各游戏的资源列表。
func loadGames(ctx context.Context, c *http.Client, games []string, all bool) []*group {
	res := make([]*group, len(games))
	var wg sync.WaitGroup
	for i, id := range games {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g, err := loadGame(ctx, c, id, all)
			if err != nil {
				if ctx.Err() == nil {
					con.logf("%s: 获取资源列表失败: %v", gameNames[id], err)
				}
				return
			}
			res[i] = g
		}()
	}
	wg.Wait()
	var out []*group
	for _, g := range res {
		if g != nil && g.count > 0 {
			out = append(out, g)
		}
	}
	return out
}

func loadGame(ctx context.Context, c *http.Client, id string, all bool) (*group, error) {
	var vers map[string]any
	if err := getJSON(ctx, c, apiBase+"/"+id+"_versions.json", &vers); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(vers))
	for k := range vers {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return verLess(keys[j], keys[i]) }) // 新版本在前

	g := &group{name: gameNames[id]}
	b := newSourceBuilder("整包", "", "", 0)
	seen := map[string]bool{}
	add := func(u string, size int64) {
		if !seen[u] {
			seen[u] = true
			b.add([]byte(u), size)
		}
	}

	if all {
		for _, k := range keys {
			collectURLs(vers[k], add)
		}
		g.add(b.build())
		g.desc = fmt.Sprintf("全部版本 整包 %d 个", g.count)
		return g, nil
	}

	// 最新版本：优先整包，没有整包则用 Sophon 分块（当前正式版 main 分支）。
	for _, k := range keys {
		collectURLs(vers[k], add)
		if len(seen) > 0 {
			g.add(b.build())
			g.desc = fmt.Sprintf("%s 整包 %d 个", k, g.count)
			return g, nil
		}
		if e, _ := vers[k].(map[string]any); e != nil {
			if ch, _ := e["chunk"].(map[string]any); ch != nil && ch["branch"] == "main" {
				return g, loadSophon(ctx, c, id, k, g)
			}
		}
	}
	return nil, errors.New("没有找到可下载的版本")
}

// collectURLs 递归找出所有 {"url": ..., "size": ...} 对象。
func collectURLs(v any, fn func(string, int64)) {
	switch x := v.(type) {
	case map[string]any:
		if u, ok := x["url"].(string); ok && strings.HasPrefix(u, "http") {
			if s, ok := x["size"].(float64); ok {
				fn(u, int64(s))
			}
		}
		for _, e := range x {
			collectURLs(e, fn)
		}
	case []any:
		for _, e := range x {
			collectURLs(e, fn)
		}
	}
}

// verLess 比较 "7.1.0" 这类版本号。
func verLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			return x < y
		}
	}
	return a < b
}

type sophonDL struct {
	Encryption  int    `json:"encryption"`
	Compression int    `json:"compression"`
	URLPrefix   string `json:"url_prefix"`
	URLSuffix   string `json:"url_suffix"`
}

type sophonManifest struct {
	Field    string `json:"matching_field"`
	Manifest struct {
		ID string `json:"id"`
	} `json:"manifest"`
	ChunkDL    sophonDL `json:"chunk_download"`
	ManifestDL sophonDL `json:"manifest_download"`
	Stats      struct {
		ChunkCount string `json:"chunk_count"`
	} `json:"stats"`
}

type sophonBuild struct {
	Data struct {
		Manifests []sophonManifest `json:"manifests"`
	} `json:"data"`
}

// loadSophon 读取该版本全部 Sophon 清单（游戏本体 + 各语音包），保留所有分块。
func loadSophon(ctx context.Context, c *http.Client, id, tag string, g *group) error {
	var b sophonBuild
	if err := getJSON(ctx, c, fmt.Sprintf("%s/chunk/%s_%s.json", apiBase, id, tag), &b); err != nil {
		return err
	}
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
		seen     = map[string]bool{}
	)
	for _, m := range b.Data.Manifests {
		if m.ManifestDL.Encryption != 0 || seen[m.Manifest.ID] {
			continue
		}
		seen[m.Manifest.ID] = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			src, err := loadManifest(ctx, c, gameNames[id], m)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("清单 %s: %w", m.Field, err)
				}
				return
			}
			g.add(src)
		}()
	}
	wg.Wait()
	if g.count == 0 {
		if firstErr == nil {
			firstErr = errors.New("没有可用的分块清单")
		}
		return firstErr
	}
	if firstErr != nil && ctx.Err() == nil {
		con.logf("%s: 部分清单读取失败，已跳过: %v", g.name, firstErr)
	}
	g.desc = fmt.Sprintf("%s 分块 %d 个 (%d 个清单)", tag, g.count, len(g.sources))
	return nil
}

func loadManifest(ctx context.Context, c *http.Client, game string, m sophonManifest) (*source, error) {
	select {
	case manifestSem <- struct{}{}:
		defer func() { <-manifestSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	hint, _ := strconv.Atoi(m.Stats.ChunkCount)
	hint = min(max(hint, 0), 1<<20) // 仅用于预分配，防止异常数据
	murl := m.ManifestDL.URLPrefix + "/" + m.Manifest.ID + m.ManifestDL.URLSuffix
	var src *source
	err := retry(ctx, func() error {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		ctx, stall := context.WithCancelCause(ctx)
		defer stall(nil)
		resp, err := get(ctx, c, murl)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		// 服务器偶尔连接不断却不再发数据，一段时间没有数据就放弃这次、重新下载
		t := time.AfterFunc(stallTimeout, func() { stall(errStalled) })
		defer t.Stop()
		var r io.Reader = idleReader{resp.Body, t}
		if m.ManifestDL.Compression == 1 {
			zr, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
			if err != nil {
				return err
			}
			defer zr.Close()
			r = zr
		}
		b := newSourceBuilder(fieldLabel(m.Field), m.ChunkDL.URLPrefix+"/", m.ChunkDL.URLSuffix, hint)
		if err := parseManifest(r, b.add); err != nil {
			if context.Cause(ctx) == errStalled {
				con.logf("%s: 清单 %s %v，重新下载", game, m.Field, errStalled)
				return errStalled
			}
			return err
		}
		src = b.build()
		return nil
	})
	return src, err
}

// idleReader 每读到数据就重置计时器，计时器到期说明连接卡住了。
type idleReader struct {
	r io.Reader
	t *time.Timer
}

func (ir idleReader) Read(p []byte) (int, error) {
	n, err := ir.r.Read(p)
	if n > 0 {
		ir.t.Reset(stallTimeout)
	}
	return n, err
}

// fieldLabel 把清单的 matching_field 转成界面上显示的类型。
func fieldLabel(f string) string {
	switch f {
	case "game":
		return "本体"
	case "asb":
		return "资源包"
	}
	if len(f) == 5 && f[2] == '-' { // zh-cn、en-us 等语音包
		return "语音 " + f
	}
	return f
}

var errBadManifest = errors.New("清单格式错误")

// parseManifest 流式解析 Sophon 清单 (protobuf)：
// Manifest{ repeated Asset = 1 }，Asset{ name = 1; repeated Chunk = 2 }，
// Chunk{ name = 1; md5 = 2; offset = 3; size = 4; ... }。
func parseManifest(r io.Reader, fn func(name []byte, size int64)) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var buf []byte
	for {
		key, err := binary.ReadUvarint(br)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		var n uint64
		switch key & 7 {
		case 0:
			_, err = binary.ReadUvarint(br)
			if err != nil {
				return err
			}
			continue
		case 1:
			n = 8
		case 5:
			n = 4
		case 2:
			if n, err = binary.ReadUvarint(br); err != nil {
				return err
			}
			if n > 64<<20 {
				return errBadManifest
			}
		default:
			return errBadManifest
		}
		if uint64(cap(buf)) < n {
			buf = make([]byte, n)
		}
		buf = buf[:n]
		if _, err := io.ReadFull(br, buf); err != nil {
			return err
		}
		if key != 1<<3|2 {
			continue
		}
		err = pbFields(buf, func(f int, _ uint64, chunk []byte) error {
			if f != 2 || chunk == nil {
				return nil
			}
			var name []byte
			var size int64
			if err := pbFields(chunk, func(f int, v uint64, b []byte) error {
				switch f {
				case 1:
					name = b
				case 4:
					size = int64(v)
				}
				return nil
			}); err != nil {
				return err
			}
			if len(name) > 0 {
				fn(name, size)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
}

// pbFields 遍历一条 protobuf 消息的字段；varint 字段给出 v，长度字段给出 b。
func pbFields(msg []byte, fn func(field int, v uint64, b []byte) error) error {
	for len(msg) > 0 {
		key, n := binary.Uvarint(msg)
		if n <= 0 {
			return errBadManifest
		}
		msg = msg[n:]
		var v uint64
		var b []byte
		switch key & 7 {
		case 0:
			if v, n = binary.Uvarint(msg); n <= 0 {
				return errBadManifest
			}
			msg = msg[n:]
		case 1, 5:
			w := 8
			if key&7 == 5 {
				w = 4
			}
			if len(msg) < w {
				return errBadManifest
			}
			msg = msg[w:]
			continue
		case 2:
			l, n := binary.Uvarint(msg)
			if n <= 0 || l > uint64(len(msg)-n) {
				return errBadManifest
			}
			b, msg = msg[n:n+int(l)], msg[n+int(l):]
		default:
			return errBadManifest
		}
		if err := fn(int(key>>3), v, b); err != nil {
			return err
		}
	}
	return nil
}

// loadURLFile 读取 URL 列表文件，每行一个，# 开头为注释。
func loadURLFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out, nil
}

func get(ctx context.Context, c *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", randomUA())
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

func getJSON(ctx context.Context, c *http.Client, url string, v any) error {
	return retry(ctx, func() error {
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		resp, err := get(ctx, c, url)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		return json.NewDecoder(resp.Body).Decode(v)
	})
}

// retry 最多尝试 3 次，间隔 2s、4s。
func retry(ctx context.Context, fn func() error) error {
	var err error
	for i := range 3 {
		if i > 0 && !sleep(ctx, time.Duration(1<<i)*time.Second) {
			return ctx.Err()
		}
		if err = fn(); err == nil || ctx.Err() != nil {
			return err
		}
	}
	return err
}
