# traffickiller

轻量的命令行流量消耗器：从米哈游官方 CDN 并发下载 **最新版本** 的原神、星穹铁道、绝区零和崩坏3 资源，数据读进内存后 **直接丢弃**，不写硬盘，不解压，不校验。

- 可指定出口网卡（支持多网卡 / 多拨）
- 可控并发数、总带宽上限、总流量、运行时长
- 单文件静态二进制，无运行依赖；实测 64 并发跑到约 5Gbps，占用约 0.6 个 CPU 核、55MB 内存
- 默认使用多个米哈游启动器 (HoYoPlay) 的真实 UA，每个连接随机分配一个，见 [User-Agent](#user-agent)
- 资源列表来自 [hoyo-files.amarea.cn](https://hoyo-files.amarea.cn/)，失效链接自动剔除

## 下载

到 [Releases](https://github.com/dann2333/traffickiller/releases/latest) 下载对应平台的文件，直接运行即可。

| 平台 | 文件 |
| --- | --- |
| Linux x86_64 | `traffickiller-linux-amd64` |
| Linux ARM64（树莓派 4/5、N1、大部分 ARM 路由器） | `traffickiller-linux-arm64` |
| Linux ARMv7 / v6 / v5 | `traffickiller-linux-armv7` / `-armv6` / `-armv5` |
| Linux MIPS 路由器（OpenWrt，推荐 softfloat） | `traffickiller-linux-mipsle-softfloat` / `-mips-softfloat` |
| Linux 其他 | `386` `mips64` `mips64le` `riscv64` `loong64` |
| Windows | `traffickiller-windows-amd64.exe` / `-386.exe` / `-arm64.exe` |
| macOS | `traffickiller-darwin-arm64`（Apple 芯片） / `-darwin-amd64` |
| FreeBSD | `traffickiller-freebsd-amd64` / `-arm64` |

Linux 一键下载（以 amd64 为例）：

```sh
curl -Lo traffickiller https://github.com/dann2333/traffickiller/releases/latest/download/traffickiller-linux-amd64
chmod +x traffickiller
./traffickiller -l 100M
```

## 用法

```
traffickiller [选项] [URL...]

  -i, -iface <网卡>    出口网卡名或本机 IP，多个用逗号分隔 (如 eth0 / pppoe-wan,pppoe-wan2)
                       默认走系统路由；多网卡时并发连接轮流分配到各网卡
  -c, -conc <数量>     并发连接数 (默认 32；跑 5Gbps 以上建议 64 或更高)
  -l, -limit <带宽>    总带宽上限，默认不限。100M / 100Mbps = 100 兆比特每秒 (宽带口径)，
                       12MB / 12MB/s = 12 兆字节每秒
  -t, -total <流量>    累计下载达到该流量后退出，如 500G、1.5T、800MB (1024 进制)
  -d, -duration <时长> 运行多久后退出，如 30m、2h、1h30m
  -g, -games <游戏>    资源来源，逗号分隔: hk4e(原神) hkrpg(星铁) nap(绝区零) bh3(崩坏3) (默认全部)
  -all                 使用全部历史版本的整包，而不只是最新版本
  -f, -file <文件>     从文件读取 URL 列表 (每行一个)，替代内置资源；也可直接在参数末尾写 URL
  -6                   绑定网卡时优先使用 IPv6 地址
  -http                CDN 下载改用 HTTP 而非 HTTPS，省去 TLS 解密开销 (适合路由器等弱 CPU 设备)
  -interval <时长>     状态刷新间隔 (默认: 终端 1s，非终端 10s)
  -ua <UA>             自定义 User-Agent，可重复指定多个 (-ua A -ua B)，每个连接随机分配一个；
                       默认使用几个米哈游启动器的 UA (HYPContainer/...)
  -list                只打印资源列表后退出
  -v, -version         显示版本
```

示例：

```sh
traffickiller                              # 不限速，32 并发，一直跑，Ctrl+C 退出
traffickiller -c 96                        # 目标数 Gbps：加大并发
traffickiller -i eth1 -c 16 -l 200M        # 从 eth1 下载，16 并发，限速 200Mbps
traffickiller -l 50M -t 300G               # 限速 50Mbps，跑满 300GB 后退出
traffickiller -i pppoe-wan -d 6h -g ys,zzz # 只下载原神和绝区零，跑 6 小时
traffickiller -i 以太网 -l 100M             # Windows 下用网卡名称（或直接写本机 IP）
```

运行时输出：

```
[00:05:12] 速率 99.9 Mbps (11.91 MB/s) | 平均 99.7 Mbps | 累计 3.62 GB / 300.00 GB (1.2%) | 连接 32/32
```

## 工作原理

1. 启动时从 hoyo-files 的接口拉取各游戏版本列表，取 **最新版本**：
   - 有整包的（如绝区零）直接用整包下载地址；
   - 只有 Sophon 分块的（如原神、星铁、崩坏3 新版本）读取该版本 **全部** 分块清单（游戏本体 + 各语音包），保留所有分块。
   - 目前合计约 33 万个文件、465GB 不重复数据（原神 179GB、星铁 140GB、绝区零 116GB、崩坏3 30GB），启动时读取清单约 10～30 秒。
2. 按 `-c` 开启并发，每个连接随机挑游戏、随机挑文件下载，读到的数据直接丢弃。
3. 所有连接共享一个全局令牌桶，总速率严格限制在 `-l` 以内。
4. 强制 HTTP/1.1，每个并发都是独立的 TCP 连接；返回 403/404 的文件自动剔除，同一游戏连续 10 次失效则停用该游戏；网络错误自动退避重试。

统计的是 HTTP 响应体字节数，实际网卡流量会略高（TCP/IP 与 TLS 头部开销约 4%）。

## 指定网卡

- `-i eth0`：使用该网卡的 IPv4 地址作为源地址；Linux 下同时用 `SO_BINDTODEVICE` 强制从该网卡发出（内核 5.7 以下需 root），macOS 用 `IP_BOUND_IF`，其他系统仅按源地址绑定。
- `-i 192.168.1.10`：直接指定源 IP。
- `-i pppoe-wan,pppoe-wan2`：多拨/多线，连接轮流分配到各网卡，`-l` 是所有网卡合计的上限。

## User-Agent

米哈游启动器 (HoYoPlay) 的 UA 格式是 `HYPContainer/<启动器版本>`。默认内置以下几个在社区项目中使用的真实 UA，每个下载连接启动时随机分配一个，并使用独立的连接池（同一条 TCP 连接始终是同一个 UA，看起来像多个独立客户端）：

| UA | 出处 |
| --- | --- |
| `HYPContainer/1.10.1.283 (windows 10)` | [Collapse Launcher](https://github.com/CollapseLauncher/Collapse) 模拟启动器资源接口 |
| `HYPContainer/1.10.1.283 (windows 11)` | 同上（Windows 11 机器） |
| `HYPContainer/1.3.3.182` | [gsuid_core](https://github.com/Genshin-bots/gsuid_core)、[TeyvatGuide](https://github.com/BTMuli/TeyvatGuide)、AUTO-MAS |
| `HYPContainer/1.1.4.133` | Snap.Hutao、FufuLauncher、sigewinne-toolkit |

想换成自己的列表：`traffickiller -ua "UA1" -ua "UA2" -ua "UA3"`。

## 跑满高带宽（数 Gbps）

- 原神/星铁/崩坏3 的分块平均约 1MB，每个请求都有一次往返等待，连接数不够就跑不满：数 Gbps 建议 `-c 64`～`-c 128`，观察速率不再上升即可。
- CPU 吃紧（路由器、小主机）时加 `-http`，省掉 TLS 解密。
- 多线路时 `-i wan1,wan2,...` 把连接分摊到各线路。

## 后台运行

systemd（`/etc/systemd/system/traffickiller.service`）：

```ini
[Unit]
Description=traffickiller
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/traffickiller -i eth0 -l 100M
Restart=always
RestartSec=30

[Install]
WantedBy=multi-user.target
```

```sh
systemctl enable --now traffickiller
journalctl -u traffickiller -f
```

OpenWrt / 其他：`nohup ./traffickiller -i pppoe-wan -l 100M > /tmp/tk.log 2>&1 &`

定时任务示例（每天凌晨 1 点跑 5 小时）：`0 1 * * * /usr/local/bin/traffickiller -l 200M -d 5h`

## 构建

```sh
go build -trimpath -ldflags "-s -w" .
```

推送到 `main` 后 GitHub Actions 会自动构建全部平台并更新 `latest` 发布；推送 `v*` 标签会生成对应版本的发布。
