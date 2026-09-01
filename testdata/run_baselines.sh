#!/usr/bin/env bash
# 探测基线校验: 对 testdata/baselines.json 的每个模板跑 ov, 校验期望版本是否全部命中。
# 单跑有缺失时补跑两次取并集再判(容忍 CDN 瞬时非 200 抖动)。
# 用法: testdata/run_baselines.sh [ov 二进制路径, 默认 ./ov]
set -u
cd "$(dirname "$0")/.."
OV="${1:-./ov}"
BASE="testdata/baselines.json"

strip() { grep -oE '[0-9]+\.[0-9]+(\.[0-9]+)?' | sort -u; }

n=$(python3 -c "import json;print(len(json.load(open('$BASE'))))")
fail=0
for i in $(seq 0 $((n - 1))); do
  name=$(python3 -c "import json;print(json.load(open('$BASE'))[$i]['name'])")
  tpl=$(python3 -c "import json;print(json.load(open('$BASE'))[$i]['template'])")
  anchor=$(python3 -c "import json;print(json.load(open('$BASE'))[$i]['anchor'])")
  expected=$(python3 -c "import json;print(' '.join(json.load(open('$BASE'))[$i]['expected_hits']))")
  av=$(python3 -c "import json;print(json.load(open('$BASE'))[$i].get('anchor_version',''))")
  substitute() { printf '%s' "$1" | sed "s/{v}/$2/g"; }  # bash 3.2 的 ${tpl//{v}/..} 会损坏, 用 sed 替换
  if [ -n "$av" ]; then
    url=$(substitute "$tpl" "$av")
  elif [[ "$anchor" == http* ]]; then
    url="$anchor"
  else
    # anchor 是纯版本号时拼进模板(如 cursor 系列记录的版本锚点)
    url=$(substitute "$tpl" "$anchor")
  fi
  echo "== baseline: $name (anchor $anchor)"

  raw=$("$OV" "$url" 2>/dev/null)
  got=$(echo "$raw" | strip)
  miss=""
  check() { # $1=期望值: URL 则整行匹配, 版本则按版本号匹配
    case "$1" in
      http*) echo "$raw" | grep -qxF "$1" ;;
      *) echo "$got" | grep -qx "$1" ;;
    esac
  }
  for v in $expected; do check "$v" || miss="$miss $v"; done
  if [ -n "$miss" ]; then
    # 瞬时缺失补跑两次取并集, 间隔冷却规避 CDN 突发限流。
    for _ in 1 2; do
      sleep 10
      raw=$( { echo "$raw"; "$OV" "$url" 2>/dev/null; } )
      got=$(echo "$raw" | strip)
    done
    miss=""
    for v in $expected; do check "$v" || miss="$miss $v"; done
  fi

  if [ -z "$expected" ]; then
    echo "SKIP: 无期望版本(锚点当前不可达, 仅记录模板)"
    continue
  fi
  if [ -n "$miss" ]; then
    echo "FAIL: 缺失:$miss"
    fail=$((fail + 1))
  else
    echo "PASS: 期望 $(echo "$expected" | wc -w | tr -d ' ') 个版本全部命中"
  fi
done
echo "== 基线校验: $((n - fail))/$n 通过"
exit "$fail"
