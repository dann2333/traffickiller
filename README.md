# traffickiller

轻量的命令行流量消耗器：从米哈游官方 CDN 并发下载 **最新版本** 的原神、星穹铁道、绝区零和崩坏3 资源，数据读进内存后 **直接丢弃**，不写硬盘，不解压，不校验。

- 可指定出口网卡（支持多网卡 / 多拨）
- 可控并发数、总带宽上限、总流量、运行时长
- 四个游戏的流量大致均衡；大文件按 32MB 随机分段下载
- 单文件静态二进制，无运行依赖；默认 128 并发，实测约 4Gbps，CPU 占用不到 1 核、内存约 70MB
- 全屏实时界面：总速率、各游戏速率，每个连接正在下载的文件、进度、速率、服务器 IP:端口和 IP 属地，见 [实时界面](#实时界面)
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
  -c, -conc <数量>     并发连接数 (默认 128，约可跑 4Gbps；更高带宽可加到 256；
                       路由器等弱设备建议 16～32)
  -l, -limit <带宽>    总带宽上限，默认不限。100M / 100Mbps = 100 兆比特每秒 (宽带口径)，
                       12MB / 12MB/s = 12 兆字节每秒
  -t, -total <流量>    累计下载达到该流量后退出，如 500G、1.5T、800MB (1024 进制)
  -d, -duration <时长> 运行多久后退出，如 30m、2h、1h30m
  -g, -games <游戏>    资源来源，逗号分隔: hk4e(原神) hkrpg(星铁) nap(绝区零) bh3(崩坏3) (默认全部)
  -all                 使用全部历史版本的整包，而不只是最新版本
  -f, -file <文件>     从文件读取 URL 列表 (每行一个)，替代内置资源；也可直接在参数末尾写 URL
  -6                   绑定网卡时优先使用 IPv6 地址
  -http                CDN 下载改用 HTTP 而非 HTTPS，省去 TLS 解密开销 (适合路由器等弱 CPU 设备)
  -plain               不用全屏界面，只输出单行状态 (输出不是终端时自动如此)
  -nogeo               界面上不显示服务器 IP 属地
  -interval <时长>     状态刷新间隔 (默认: 终端 1s，非终端 10s)
  -ua <UA>             自定义 User-Agent，可重复指定多个 (-ua A -ua B)，每个连接随机分配一个；
                       默认使用几个米哈游启动器的 UA (HYPContainer/...)
  -list                只打印资源列表后退出
  -v, -version         显示版本
```

示例：

```sh
traffickiller                              # 不限速，128 并发，一直跑，Ctrl+C 退出
traffickiller -c 256                       # 带宽更高：加大并发
traffickiller -i eth1 -c 16 -l 200M        # 从 eth1 下载，16 并发，限速 200Mbps
traffickiller -l 50M -t 300G               # 限速 50Mbps，跑满 300GB 后退出
traffickiller -i pppoe-wan -d 6h -g ys,zzz # 只下载原神和绝区零，跑 6 小时
traffickiller -i 以太网 -l 100M             # Windows 下用网卡名称（或直接写本机 IP）
```

## 实时界面

在终端里运行时默认进入全屏界面（Ctrl+C 退出，退出后恢复终端并打印汇总）。下面是 `-c 64 -d 45s` 运行时的截取：

```
traffickiller 20260927-xxxxxxx  运行 00:00:20  剩余 00:00:24  出口 系统默认路由 | 限速 不限 | 总量 不限 | UA 4 个
速率 1.9 Gbps (225.49 MB/s)  平均 1.5 Gbps  累计 3.54 GB  完成 2846 次  连接 27/64
进度 [#############.................]  44.4%

游戏      速率        占比              累计        连接  完成次数  资源
原神      535.2 Mbps  [###.......]  28% 984.40 MB   24    903       7.1.0 分块 168018 个 (5 个清单)
星穹铁道  515.1 Mbps  [###.......]  27% 1007.17 MB  25    950       4.5.0 分块 136590 个 (5 个清单)
绝区零    350.6 Mbps  [##........]  19% 663.41 MB   7     19        3.2.0 整包 24 个
崩坏3     490.7 Mbps  [###.......]  26% 972.14 MB   8     974       9.1.0 分块 30799 个 (2 个清单)

#   游戏      类型        速率        进度              服务器              属地                文件
1   星穹铁道  本体        44.2 Mbps   请求中            153.43.96.66:443    美国加利福尼亚      8eedef843134f793_df7200bd43ba0e0fbced903d724cef19
2   原神      语音 ja-jp  48.8 Mbps     0% 0B/1.36M     138.113.81.215:443  美国亚利桑那凤凰城  591a6d473c306647_37046173ff1f8473704df4b223266c0e
3   原神      语音 ja-jp  57.3 Mbps   请求中            116.169.191.37:443  中国 联通           993c8e8e4bab2d7c_1869a9c43fecb97637005cd9cadbc55d
4   崩坏3     资源包      53.3 Mbps     0% 0B/1.41M     43.146.64.170:443   日本                c08714d6284e8f3e_e2749ec125699949845048707f623ffe
5   原神      本体        23.2 Mbps    44% 551K/1.22M   116.169.191.37:443  中国 联通           987caed490dbc3ec_4d075f878e1c35fa2667ced475ae3ef4
9   绝区零    整包        77.9 Mbps    38% 12.1M/32.0M  116.162.51.191:443  湖南 联通           audio_ko-kr_3.1.0_3.2.0_hdiff_YZVsDCqUparOazas.zip @95.0M
10  星穹铁道  语音 zh-cn  65.0 Mbps   请求中            42.56.88.118:443    辽宁沈阳 联通       356812a545368a03_e39c29b2053ffd18633af918e5d9bf72
... 另有 51 个连接未显示（放大窗口可查看更多）
Ctrl+C 退出
```

- 连接太多一屏放不下时只显示前面的，放大窗口可看到更多；出错的连接会显示错误和重试倒计时，最近几条日志显示在底部。
- 服务器是本连接实际连上的 IP:端口。设置了 `HTTPS_PROXY` 等代理时显示的是代理地址，标题栏会注明。
- IP 属地用内置的 [ip2region](https://github.com/lionsoul2014/ip2region) 离线数据库（IPv4 + IPv6 完整库，zstd 压缩后内嵌）在本地查询，**全程不联网**，也不会像在线查询那样受频率限制。国内到省市 + 运营商，国外到国家（部分到城市）。不想显示可加 `-nogeo`。
- 输出重定向到文件、systemd/nohup 后台运行，或加 `-plain` 时，改为每隔一段时间输出一行：

```
[00:05:12] 速率 99.9 Mbps (11.91 MB/s) | 平均 99.7 Mbps | 累计 3.62 GB / 300.00 GB (1.2%) | 连接 128/128
```

## 工作原理

1. 启动时从 hoyo-files 的接口拉取各游戏版本列表，取 **最新版本**：
   - 有整包的（如绝区零）直接用整包下载地址；
   - 只有 Sophon 分块的（如原神、星铁、崩坏3 新版本）读取该版本 **全部** 分块清单（游戏本体 + 各语音包），保留所有分块。
   - 目前合计约 33 万个文件、465GB 不重复数据（原神 179GB、星铁 140GB、绝区零 116GB、崩坏3 30GB），启动时读取清单约 10～30 秒。
2. 按 `-c` 开启并发，每个连接随机挑游戏、随机挑文件下载，读到的数据直接丢弃：
   - 挑游戏时按"每次请求的平均大小"加权，分块约 1MB 的游戏被挑中得多、整包游戏被挑中得少，结果四个游戏的流量大致相同；
   - 整包动辄几 GB，每次只用 Range 请求随机下载其中 32MB（界面上文件名后的 `@95.0M` 是这一段的起点），连接不会长时间停在一个大文件上。
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

- 原神/星铁/崩坏3 的分块平均约 1MB，每个请求都有一次往返等待，连接数不够就跑不满。实测：64 并发约 1.8Gbps，128 并发（默认）约 4Gbps、内存约 70MB，256 并发约 6Gbps、内存约 115MB。带宽更高就继续加 `-c`，观察速率不再上升即可。
- 每个并发约占 3～4 个文件描述符（与各游戏 CDN 分别保持连接），`-c` 很大时注意 `ulimit -n`。
- 路由器、小主机等弱设备用 `-c 16`～`-c 32`，可省内存和 CPU。
- 不在乎游戏是否均衡、只求单连接效率时，可以用 `-g zzz` 只下载绝区零整包（每次请求 32MB，往返等待占比小）。
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

OpenWrt / 其他：`nohup ./traffickiller -i pppoe-wan -c 16 -http -l 100M > /tmp/tk.log 2>&1 &`

定时任务示例（每天凌晨 1 点跑 5 小时）：`0 1 * * * /usr/local/bin/traffickiller -l 200M -d 5h`

## 构建

```sh
go build -trimpath -ldflags "-s -w" .
```

推送到 `main` 后 GitHub Actions 会自动构建全部平台并更新 `latest` 发布；推送 `v*` 标签会生成对应版本的发布。发布的可执行文件会用 UPX 压缩（UPX 不支持的平台如 macOS、riscv64 保持原样）。

## 数据来源与许可

IP 属地数据来自 [ip2region](https://github.com/lionsoul2014/ip2region)（`ipdb/` 目录下 zstd 压缩内嵌），按 Apache-2.0 或 MIT 许可分发，许可全文见 [`ipdb/LICENSE.md`](ipdb/LICENSE.md)。更新数据库参见 [`ipdb/update.sh`](ipdb/update.sh)。
