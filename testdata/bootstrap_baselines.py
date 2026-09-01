#!/usr/bin/env python3
"""基线 bootstrap: 对 baselines.json 每条模板用 ov 实测一遍, 把当前可发现的版本
写入 expected_hits。此后 run_baselines.sh 即可以此为回归基准。
用法: python3 testdata/bootstrap_baselines.py [ov 路径, 默认 ./ov]
"""
import json
import re
import subprocess
import sys
import os

os.chdir(os.path.join(os.path.dirname(__file__), ".."))
OV = sys.argv[1] if len(sys.argv) > 1 else "./ov"
VER = r"\d+\.\d+(?:\.\d+)?"
path = "testdata/baselines.json"
entries = json.load(open(path))

for e in entries:
    tpl, anchor = e["template"], e["anchor"]
    run_url = anchor if anchor.startswith("http") else tpl.replace("{v}", anchor)
    urls = []
    for _attempt in range(2):  # 容忍瞬时跑空, 重试一次
        try:
            out = subprocess.run([OV, run_url], capture_output=True, text=True, timeout=240).stdout
        except subprocess.TimeoutExpired:
            print(f"[skip] {e['name']}: 超时", flush=True)
            out = ""
        urls = re.findall(r"https?://\S+", out)
        if urls:
            break
    hits = []
    if e.get("kind") == "single" or "{v}" not in tpl:
        # 单点基线: anchor URL 仍存活即记自身
        if any(u.rstrip(".,;") == anchor for u in urls):
            m = re.search(VER, anchor)
            hits = [m.group(0)] if m else ["literal"]
    else:
        rx = re.compile(
            "^" + re.escape(tpl)
            .replace(r"\{v\}", "(" + VER + ")")
            .replace("{token}", r"[0-9a-f]{16,}")
            .replace("{build}", r"\d{6,}") + "$"
        )
        for u in urls:
            m = rx.match(u.rstrip(".,;"))
            if m:
                hits.append(m.group(1))
        hits = sorted(set(hits), key=lambda v: [int(x) for x in re.findall(r"\d+", v)])
    e["expected_hits"] = hits
    print(f"[{len(hits):>2} hits] {e['name']}", flush=True)

json.dump(entries, open(path, "w"), ensure_ascii=False, indent=1)
print("bootstrap 完成, expected_hits 已回填", flush=True)
