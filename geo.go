package main

import (
	_ "embed"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/klauspost/compress/zstd"
)

// IP 属地数据来自 ip2region (https://github.com/lionsoul2014/ip2region，Apache-2.0 OR MIT)，
// 完整的 IPv4 / IPv6 数据库用 zstd 压缩后内嵌在程序里，全部在本地查询，不联网。
// 更新数据见 ipdb/update.sh。

//go:embed ipdb/ip2region_v4.xdb.zst
var ipdbV4 []byte

//go:embed ipdb/ip2region_v6.xdb.zst
var ipdbV6 []byte

// geoCache 查询服务器 IP 的属地并缓存。数据库第一次用到时在后台解压
// (IPv4 约占 11MB 内存，IPv6 约 36MB，只有出现 IPv6 服务器时才解压)，解压完成前返回空串。
type geoCache struct {
	v4, v6 geoDB
	mu     sync.Mutex
	m      map[string]string
}

type geoDB struct {
	zst  []byte
	once sync.Once
	x    atomic.Pointer[xdb]
}

func newGeoCache() *geoCache {
	return &geoCache{v4: geoDB{zst: ipdbV4}, v6: geoDB{zst: ipdbV6}, m: map[string]string{}}
}

// load 返回解压好的数据库；还没解压完时返回 nil 并在后台开始解压。
func (d *geoDB) load() *xdb {
	if x := d.x.Load(); x != nil {
		return x
	}
	d.once.Do(func() {
		go func() {
			x, err := openXDB(d.zst)
			if err != nil {
				con.logf("IP 属地数据读取失败: %v", err)
				x = &xdb{} // 之后都查不到
			}
			d.x.Store(x)
		}()
	})
	return nil
}

func openXDB(zst []byte) (*xdb, error) {
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	buf, err := dec.DecodeAll(zst, nil)
	if err != nil {
		return nil, err
	}
	return newXDB(buf)
}

// get 返回 ip 的属地，数据库还在解压时返回空串。
func (g *geoCache) get(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ""
	}
	a = a.Unmap()
	if a.IsLoopback() {
		return "本机"
	} else if a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsUnspecified() {
		return "局域网"
	}
	if g == nil {
		return ""
	}
	db := &g.v6
	if a.Is4() {
		db = &g.v4
	}
	x := db.load()
	if x == nil {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	loc, ok := g.m[ip]
	if !ok {
		if loc = formatRegion(x.search(a.AsSlice())); loc == "" {
			loc = "未知"
		}
		g.m[ip] = loc
	}
	return loc
}

// formatRegion 把 ip2region 的 "国家|省份|城市|运营商|国家代码" 转成界面上显示的属地，
// 如 "中国|广东省|深圳市|电信|CN" -> "广东深圳 电信"，"Japan|Tokyo|Tokyo|WIDE Project|JP" -> "日本 Tokyo WIDE Project"。
func formatRegion(r string) string {
	f := strings.Split(r, "|")
	if len(f) < 5 {
		return ""
	}
	for i, s := range f {
		if s == "0" {
			f[i] = ""
		}
	}
	country, prov, city, isp, cc := f[0], f[1], f[2], f[3], f[4]
	var place string
	switch {
	case country == "":
		return ""
	case country == "Reserved":
		return "保留地址"
	case country == "中国":
		prov, city = shortPlace(prov), shortPlace(city)
		if place = prov; city != prov {
			place += city
		}
		if place == "" {
			place = country
		}
		if s := strings.TrimPrefix(isp, "中国"); s != "" {
			isp = s // "中国电信" -> "电信"
		}
	default:
		if place = countryNames[cc]; place == "" {
			place = country
		}
		if city == "" {
			city = prov
		}
		if city != "" {
			place += " " + city
		}
		isp, _, _ = strings.Cut(isp, ",") // "AT&T Enterprises, LLC" -> "AT&T Enterprises"
	}
	if isp != "" {
		place += " " + isp
	}
	return place
}

// shortPlace 去掉省、市、自治区等后缀，如 "广西壮族自治区" -> "广西"，"北京市" -> "北京"。
func shortPlace(s string) string {
	for _, suf := range []string{"壮族自治区", "回族自治区", "维吾尔自治区", "自治区", "特别行政区", "省", "市"} {
		if t, ok := strings.CutSuffix(s, suf); ok && t != "" {
			return t
		}
	}
	return s
}

// countryNames 把国家代码翻译成中文 (ip2region 里国外的国家名是英文)。
var countryNames = func() map[string]string {
	m := map[string]string{}
	for _, f := range strings.Fields(`
		CN中国 HK香港 MO澳门 TW台湾 US美国 JP日本 KR韩国 KP朝鲜 SG新加坡 MY马来西亚 TH泰国 VN越南 PH菲律宾
		ID印度尼西亚 IN印度 PK巴基斯坦 BD孟加拉国 LK斯里兰卡 NP尼泊尔 BT不丹 MV马尔代夫 MM缅甸 KH柬埔寨 LA老挝
		BN文莱 TL东帝汶 MN蒙古 KZ哈萨克斯坦 UZ乌兹别克斯坦 KG吉尔吉斯斯坦 TJ塔吉克斯坦 TM土库曼斯坦 AF阿富汗
		IR伊朗 IQ伊拉克 SY叙利亚 LB黎巴嫩 JO约旦 IL以色列 PS巴勒斯坦 SA沙特阿拉伯 AE阿联酋 QA卡塔尔 BH巴林
		KW科威特 OM阿曼 YE也门 TR土耳其 CY塞浦路斯 GE格鲁吉亚 AM亚美尼亚 AZ阿塞拜疆
		GB英国 IE爱尔兰 FR法国 DE德国 NL荷兰 BE比利时 LU卢森堡 CH瑞士 AT奥地利 LI列支敦士登 MC摩纳哥
		IT意大利 SM圣马力诺 VA梵蒂冈 MT马耳他 ES西班牙 PT葡萄牙 AD安道尔 GI直布罗陀 GR希腊 AL阿尔巴尼亚
		MK北马其顿 RS塞尔维亚 ME黑山 BA波黑 HR克罗地亚 SI斯洛文尼亚 XK科索沃 BG保加利亚 RO罗马尼亚 MD摩尔多瓦
		HU匈牙利 SK斯洛伐克 CZ捷克 PL波兰 UA乌克兰 BY白俄罗斯 RU俄罗斯 LT立陶宛 LV拉脱维亚 EE爱沙尼亚
		FI芬兰 SE瑞典 NO挪威 DK丹麦 IS冰岛 FO法罗群岛 GL格陵兰 AX奥兰群岛 SJ斯瓦尔巴和扬马延
		IM马恩岛 JE泽西岛 GG根西岛
		CA加拿大 MX墨西哥 GT危地马拉 BZ伯利兹 SV萨尔瓦多 HN洪都拉斯 NI尼加拉瓜 CR哥斯达黎加 PA巴拿马
		CU古巴 JM牙买加 HT海地 DO多米尼加 PR波多黎各 BS巴哈马 BB巴巴多斯 TT特立尼达和多巴哥 GD格林纳达
		LC圣卢西亚 VC圣文森特和格林纳丁斯 DM多米尼克 AG安提瓜和巴布达 KN圣基茨和尼维斯 BM百慕大 KY开曼群岛
		VG英属维尔京群岛 VI美属维尔京群岛 AI安圭拉 MS蒙特塞拉特 TC特克斯和凯科斯群岛 AW阿鲁巴 CW库拉索
		SX荷属圣马丁 BQ荷兰加勒比区 MF法属圣马丁 BL圣巴泰勒米 GP瓜德罗普 MQ马提尼克 PM圣皮埃尔和密克隆
		BR巴西 AR阿根廷 CL智利 UY乌拉圭 PY巴拉圭 BO玻利维亚 PE秘鲁 EC厄瓜多尔 CO哥伦比亚 VE委内瑞拉
		GY圭亚那 SR苏里南 GF法属圭亚那 FK福克兰群岛 GS南乔治亚和南桑威奇群岛
		AU澳大利亚 NZ新西兰 PG巴布亚新几内亚 FJ斐济 SB所罗门群岛 VU瓦努阿图 NC新喀里多尼亚 PF法属波利尼西亚
		WS萨摩亚 AS美属萨摩亚 TO汤加 TV图瓦卢 KI基里巴斯 NR瑙鲁 MH马绍尔群岛 FM密克罗尼西亚 PW帕劳
		GU关岛 MP北马里亚纳群岛 CK库克群岛 NU纽埃 TK托克劳 WF瓦利斯和富图纳 NF诺福克岛 PN皮特凯恩群岛
		CX圣诞岛 CC科科斯群岛 UM美国本土外小岛屿 AQ南极洲 TF法属南部领地 IO英属印度洋领地
		EG埃及 LY利比亚 TN突尼斯 DZ阿尔及利亚 MA摩洛哥 EH西撒哈拉 SD苏丹 SS南苏丹 ET埃塞俄比亚 ER厄立特里亚
		DJ吉布提 SO索马里 KE肯尼亚 UG乌干达 RW卢旺达 BI布隆迪 TZ坦桑尼亚 MZ莫桑比克 MW马拉维 ZM赞比亚
		ZW津巴布韦 BW博茨瓦纳 NA纳米比亚 ZA南非 LS莱索托 SZ斯威士兰 MG马达加斯加 MU毛里求斯 SC塞舌尔
		KM科摩罗 RE留尼汪 YT马约特 AO安哥拉 CD刚果金 CG刚果布 GA加蓬 GQ赤道几内亚 CM喀麦隆 CF中非
		TD乍得 NE尼日尔 NG尼日利亚 BJ贝宁 TG多哥 GH加纳 CI科特迪瓦 BF布基纳法索 ML马里 SN塞内加尔
		GM冈比亚 GW几内亚比绍 GN几内亚 SL塞拉利昂 LR利比里亚 MR毛里塔尼亚 CV佛得角 ST圣多美和普林西比
		SH圣赫勒拿
	`) {
		m[f[:2]] = f[2:]
	}
	return m
}()
