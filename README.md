# ov — 下载地址版本号探测工具

[![Go Version](https://img.shields.io/badge/Go-1.22-blue)](https://golang.org)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

给定一个包含版本号的下载地址，自动识别版本号（标准版本 / 日期 / 纯数字 / 字母混合），
枚举全部版本组合**并发探测**，输出所有可下载的完整 URL。
支持从单一平台 URL 发现其他平台的下载地址（`-platform` 高级模式）。

## 目录

- [编译](#编译)
- [快速开始](#快速开始)
- [工作原理](#工作原理)
- [选项](#选项)
- [实测结果](#实测结果)

---

## 编译

```bash
go build -o ov .
```

支持 Linux / macOS / Windows，amd64 / arm64 架构。

## 快速开始

```bash
# 标准版本号，自动枚举 0.0.0 ~ 锚点版本全部组合
./ov "https://autoglm.aminer.cn/autoclaw/updates/autoclaw-1.17.4-cn.dmg"

# 纯数字版本（构建号）
./ov -mode num "https://app-download.xaminim.com/xingye-android-release/754/xingye_arm_xy_guanwang.apk"

# 只验证某个 URL 是否可下载（不做枚举；带 ? 参数会自动去掉参数重试）
./ov -verify "https://kimi-img.moonshot.cn/app/download/mac/kimi_3.2.1.dmg?download_id=xxx"

# 多平台高级模式：从 windows exe 发现 mac / linux 的全部安装包
./ov -platform "https://cdn-zcode.z.ai/zcode/electron/releases/3.8.1/windows-x64/ZCode-3.8.1-win-x64.exe"

# 指定目标版本替换（URL 中多个版本号同时替换）
./ov -to 3.8.1 "https://cdn-zcode.z.ai/zcode/electron/releases/3.8.1/windows-x64/ZCode-3.8.1-win-x64.exe"
```

## 工作原理

### 1. 版本识别（`-mode`，默认 auto）

| 模式 | 说明 | 示例 |
| --- | --- | --- |
| `std` | 标准版本号（2-6 段） | 1.17.4, 4.1.0.175, 1.83.1 |
| `date` | 日期（带点/横线） | 2025.08.22, 2025-08-22 |
| `num` | 纯数字构建号（≥3 位） | 754, 37271 |
| `alnum` | 数字+字母后缀混合 | 3.0.2-arm64, v1.2.3-beta.1 |

可按逗号组合：`-mode std,num`。识别结果按"带点程度+出现顺序"去重保留，
**URL 中所有同版本串一次性全部替换**为占位符（多个版本号同时修改，如 ZCode）。

### 2. 不可遍历检测

路径含 6+ 位十六进制串/哈希目录（如 QQ 的 `3f89efc5`）、UUID、7+ 位数字目录、
Build 号或长数字序列的 URL 无法枚举，直接提示并退出。
确认可枚举时可 `-force-tpl` 强制使用 `{v}` 模板。
纯 8 位日期（yyyymmdd）不作为默认可枚举对象，同样走 `-force-tpl -from -to`。

### 3. 探测（HEAD 快探 + 异步 GET 校验）

1. **HEAD** 请求：404 直接判不存在（绝大多数候选在此结束，极快）；
2. HEAD 响应头已表明是下载文件时**直接确认、跳过 GET**：
   - `Content-Disposition: attachment`；
   - 或体积 ≥ 1 MiB 且 Content-Type 不像文本（真实安装包均为 MB 级二进制，
     "假 200"几乎总是小体积文本页）。`-strict` 下不跳过（必须读魔数）。
3. 其余 2xx（可能"假 200"）或不支持 HEAD（405/403/5xx）→ 由**独立校验队列**
   并发 **GET + `Range: bytes=0-2047`** 只读开头 2KB，不阻塞发现流水线：
   - 内容为文本（HTML/JSON 错误页）→ 判为不可下载；
   - 内容非文本（压缩包/安装包魔法字节）→ 命中；
4. 逐字节判断已知魔法：zip/apk/jar、ELF、MZ(exe)、mach-o、7z/rar/pdf、gzip/bzip2/xz、tar、dmg、deb/ar，
   `-sizes` 可查看大小与类型；
5. 416 视为存在（空文件）；网络错误/429/5xx 自动重试（`-retry`）；
6. 带 `?query` 的 URL 未命中时会**去掉参数再试一次**（kimi 的 `?download_id=` 场景）。

### 4. 版本枚举与探测策略（`-strategy`）

版本空间理论上限是 200×200×200 = 8,000,000，但实际命中率极低（大部分版本号到不了 199），
所以默认采用 **`smart`（前沿生长）策略**，分两区探测：

**历史区**（≤ 已知版本）：按组件组合全量枚举（如 0.0.0 ~ 1.17.4 共 180 个）。
真实锚点的历史区通常只有几百个请求，保证不遗漏任何老版本。
组件数以识别版本为准，前导零格式（`2025.08.22`）原样保留；
`-from` 组件多则截断、少则补零；`-beyond N` 在此区域之上再额外探测
已知版本之后的 N 个更新版本（默认 3）。

**前沿区**（> 已知版本）：不无脑枚举，而是逐维度"生长"：
- 主版本前沿：探测 `(M+1).0.0`、`(M+2).0.0` … 连续 `-front-stop` 次未命中即停；
- 副版本前沿：对每个存在的主版本探测 `M.m.0`，同样连停即止；
- 补丁探索：对锚点基座与前沿发现的新 (主,副) 基座，从已知补丁+1 起线性探测，
  连续未命中停止（补丁段通常密集，命中率高）。

例如 autoglm 锚点 1.17.4：历史区 288 请求 + 前沿区 14 请求，
前沿发现了老策略漏掉的主版本 **3.0.0**（409MB），总命中 25 → 26。

`-strategy exhaust` 可回到纯全量枚举（只探测到 `-to`）；
引擎总请求数始终受 `-max`（默认 500000）硬顶；
`-universe`（默认 199）定义分量最大值 0..universe，即 200 个取值。

**滚动窗口**：当候选空间过大（如单组件大数字 72203 > stop×10）时自动触发。
维护一个宽度为 `-stop`（默认 10）的滑动窗口，遇命中重置、连续未命中达窗口宽则停止。
单组件场景还会自动修正上界，避免锚点远大于 universe 时无谓地向下搜索。

### 5. 多平台扩展（`-platform`）

把 URL 中的平台/架构/扩展名标记当作**槽位**（如 `windows-x64` 目录、
`win-x64` 文件名、`.exe` 扩展名），组合替换生成候选并逐一探测。
已发现：zcode 从 windows → macos/linux 全平台、MiniMax Design arm64 → x64、
kimi mac → windows exe。组合数上限保护（128）。

## 选项

| 选项 | 默认 | 说明 |
| --- | ---: | --- |
| `-url` / 位置参数 | — | 下载地址（含版本号，或含 `{v}` 占位符） |
| `-mode` | `auto` | 识别模式：auto/std/date/num/alnum，逗号组合 |
| `-strategy` | `smart` | 探测策略：smart（前沿生长，默认）/exhaust（全量枚举到 -to） |
| `-universe` | `199` | 版本分量最大值 0..universe（=200 个取值，200×200×200 上限） |
| `-stop` | `200` | 滚动窗口宽度：连续未命中多少次即停止（命中立即刷新）；候选空间过大时自动触发此模式 |
| `-front-stop` | `5` | 前沿探测中连续未命中多少次即停止该维度 |
| `-from` | `0.0.0` | 起始版本（含） |
| `-to` | 锚点+beyond | 结束版本（含） |
| `-beyond` | `3` | 历史区之上再探测的更新版本数量 |
| `-max` | `500000` | 总探测请求上限（含历史区与前沿区） |
| `-c` | `50` | 并发数 |
| `-timeout` | `10s` | 单请求超时 |
| `-retry` | `1` | 网络错误/429/5xx 的额外重试次数 |
| `-minsize` | `0` | 最小文件大小（字节），过滤小"假 200"文件 |
| `-strict` | false | 仅接受已知魔法字节（压缩包/安装包） |
| `-sizes` | false | 输出文件大小与类型 |
| `-o` | stdout | 结果输出文件 |
| `-reverse` | false | 结果按版本语义降序输出（从新到旧），默认升序（从早到晚） |
| `-k` | false | 跳过 TLS 证书校验 |
| `-tls-fingerprint` | (原生) | TLS 指纹伪装：`chrome`/`firefox`/`ios`/`android`/`edge`/`safari` 等，自动支持 HTTP/2 |
| `-q` | false | 关闭进度输出 |
| `-verify` | false | 只验证给定 URL 是否可下载 |
| `-platform` | false | 高级模式：发现其他平台/架构的下载地址 |
| `-force-tpl` | false | 跳过不可遍历检查，强制模板模式（需地址含 `{v}`） |
| `-path-variants` | false | 主形态 404 时试探通用路径变体（去掉一层发布子目录 / 同 OS 换架构），用于跨发布目录布局漂移找回旧版 URL |
| `-ua` | `Mozilla/5.0 (compatible; ov-prober/1.0)` | User-Agent |

## 实测结果

<details>
<summary>点击展开全部实测数据</summary>

| 地址 | 命中 | 请求数 | 备注 |
| --- | ---: | ---: | --- |
| AutoTyper-072203.exe | 0 | 21 | 滚动窗口模式；域名仅此一版 |
| Trae 2.3.73734 dmg | 1 | 51 | 滚动窗口模式；命中锚点自身 |
| autoclaw 1.17.4 dmg | 26 | 315 | 历史区 288 + 前沿 27；含 3.0.0 |
| xingye 754 APK (num) | 53 | 117 | 滚动窗口模式；686~782 |
| MiniMax Design 3.0.2 arm64 dmg | 3 | 39 | 历史区 24 + 前沿 15 |
| MiniMax Design 3.0.2 x64 dmg | 3 | 39 | 同上 |
| MiniMax Design 3.0.2 x64-Setup.exe | 3 | 39 | 同上 |
| MiniMax Code 3.0.67 arm64 dmg | 31 | 299 | 历史区 284 + 前沿 15；3.0.33~3.0.67 |
| ChatGLM 2.0.0 dmg | 1 | 27 | 历史区 12 + 前沿 15；仅 2.0.0 有效 |
| ChatGLM 3.7.2 guanwang APK | 63 | 207 | 历史区 192 + 前沿 15；2.3.0~3.7.3 |
| kimi 3.0.7 Android APK | 5 | 59 | 历史区 44 + 前沿 15 |
| kimi 3.2.1 mac dmg | 12 | 75 | 历史区 60 + 前沿 15；含 3.0.0~3.2.1 |
| kimi 3.0.12 macos_x64 dmg | 4 | 79 | 历史区 64 + 前沿 15 |
| kimi 3.2.1 mac dmg (?download_id) | 12 | 75 | query 参数自动去除重试命中 |
| QQ 3.2.32 deb | — | — | 正确判为不可遍历（哈希目录） |
| Qianwen V4.1.0.175 dmg | — | — | 正确判为不可遍历（版本+Build 混合） |
| VS Code 1.83.1 linux-deb-arm64 | 97 | 850 | 历史区 840 + 前沿 10；1.50.0~1.83.1 |
| ZCode 3.8.1 win-x64 exe | 13 | 195 | 历史区 180 + 前沿 15；3.4.0~3.8.1 |
| ZCode 3.8.1 mac-arm64 dmg | 14 | 195 | 同上；含 3.3.3 |
| ZCode 3.8.1 mac-x64 dmg | 13 | 195 | 同上 |
| ZCode 3.8.1 linux-x64 deb | 13 | 195 | 同上 |
| ZCode 3.8.1 linux-x64 AppImage | 13 | 195 | 同上 |
| ZCode 3.8.1 linux-arm64 deb | 13 | 195 | 同上 |
| ZCode 3.8.1 linux-arm64 AppImage | 13 | 195 | 同上 |

</details>

## 测试用例

<details>
<summary>点击展开全部测试 URL</summary>

```
https://update.autotyper.top/Download/AutoTyper-072203.exe
https://lf-cdn.trae.com.cn/obj/trae-com-cn/pkg/app/releases/stable/2.3.73734/darwin/TraeWork_CN-darwin-arm64.dmg
https://autoglm.aminer.cn/autoclaw/updates/autoclaw-1.17.4-cn.dmg
https://app-download.xaminim.com/xingye-android-release/754/xingye_arm_xy_guanwang.apk
https://filecdn.minimax.chat/public/minimax-hub/release/domestic/MiniMax%20Design-3.0.2-arm64.dmg
https://filecdn.minimax.chat/public/minimax-hub/release/domestic/MiniMax%20Design-3.0.2-x64.dmg
https://filecdn.minimax.chat/public/minimax-hub/release/domestic/MiniMax%20Design-3.0.2-x64-Setup.exe
https://filecdn.minimax.chat/public/minimax-agent-prod/release/MiniMax%20Code-3.0.67-arm64.dmg
https://sfile.chatglm.cn/apk/xinyu/windows/chatglm_2.0.0_universal.dmg
https://sfile.chatglm.cn/apk/xinyu/android/ChatGLM_3.7.2_guanwang_64.apk
https://kimi-img.moonshot.cn/app/download/android/kimi_3.0.7.apk
https://kimi-img.moonshot.cn/app/download/mac/kimi_3.2.1.dmg
https://kimi-img.moonshot.cn/app/download/macos_x64/kimi_3.0.12.dmg
https://kimi-img.moonshot.cn/app/download/mac/kimi_3.2.1.dmg?download_id=0c33964cc6230b49f6a9255e28539f0aa4
https://qqdl.gtimg.cn/qqfile/QQNT/9.9.33/release/3f89efc5/QQ_3.2.32_260812_loongarch64_01.deb
https://pc-download.qianwen.com/download/37271/qianwenmac/pcqwen@default/QianwenMac_V4.1.0.175_mac_pf3000_(zh-cn)_releasemini_(Build3130705).dmg?response-content-disposition=attachment;filename*=UTF-8''Qianwen_V4.1.0.175_fp_dapi-0eff522d-6e38-4329-86ce-2d21e7ec71b9_fp.dmg
https://update.code.visualstudio.com/1.83.1/linux-deb-arm64/stable
https://cdn-zcode.z.ai/zcode/electron/releases/3.8.1/windows-x64/ZCode-3.8.1-win-x64.exe
https://cdn-zcode.z.ai/zcode/electron/releases/3.8.1/macos-arm64/ZCode-3.8.1-mac-arm64.dmg
https://cdn-zcode.z.ai/zcode/electron/releases/3.8.1/macos-x64/ZCode-3.8.1-mac-x64.dmg
https://cdn-zcode.z.ai/zcode/electron/releases/3.8.1/linux-x64/ZCode-3.8.1-linux-x64.deb
https://cdn-zcode.z.ai/zcode/electron/releases/3.8.1/linux-x64/ZCode-3.8.1-linux-x64.AppImage
https://cdn-zcode.z.ai/zcode/electron/releases/3.8.1/linux-arm64/ZCode-3.8.1-linux-arm64.deb
https://cdn-zcode.z.ai/zcode/electron/releases/3.8.1/linux-arm64/ZCode-3.8.1-linux-arm64.AppImage
```

其中的带签名 URL（QQ、千问）会被正确地判为不可遍历。

</details>

## 注意事项

- 版本组合按组件笛卡尔积增长，组件多（如 4 段+日期）时先用 `-from/-to` 约束；
  `-max` 会在候选数超限时给出明确错误。
- 仅对你有授权的目标使用，遵守目标网站的服务条款；探测时建议调低 `-c` 避免给对方压力。
- 部分 CDN 对任意路径返回 200 的 HTML/JSON 错误页，程序会 GET 读开头 2KB 排除；
  若仍有假阳性可加 `-sizes -minsize` 或 `-strict`。
