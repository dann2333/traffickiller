#!/bin/sh
# 更新内嵌的 ip2region 离线 IP 属地数据库。
# 从 ip2region 仓库拉取最新的完整 IPv4 / IPv6 xdb，用 zstd 压缩后放到本目录，
# 程序会通过 //go:embed 打包进去（见 geo.go）。
#
# 需要: git、zstd。用法: 在本目录 (ipdb/) 下运行 ./update.sh
set -eu

repo=https://github.com/lionsoul2014/ip2region.git
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# 只取数据目录，省去下载整个仓库
git clone --depth 1 --filter=blob:none --sparse "$repo" "$tmp/ip2region"
git -C "$tmp/ip2region" sparse-checkout set data

for v in v4 v6; do
	src="$tmp/ip2region/data/ip2region_$v.xdb"
	zstd --ultra -22 -q -f "$src" -o "ip2region_$v.xdb.zst"
done

cp "$tmp/ip2region/LICENSE.md" LICENSE.md
echo "已更新："
ls -l ip2region_v4.xdb.zst ip2region_v6.xdb.zst
