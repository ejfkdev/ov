# ov — 下载地址版本号探测工具（中文文档）

> :us: English: [README.md](./README.md)

[![Go Version](https://img.shields.io/badge/Go-1.25-blue)](https://golang.org)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

给定一个包含版本号的下载地址，自动识别版本号（标准版本 / 日期 / 纯数字 / 字母混合），
枚举全部版本组合**并发探测**，输出所有可下载的完整 URL。
支持从单一平台 URL 发现其他平台的下载地址（`-platform` 高级模式）。
交互式与管道/脚本两种运行方式自动适配（见下文「管道与非交互输出」）。

## 目录

- [编译](#编译)
- [安装（预编译产物）](#安装预编译产物)
- [快速开始](#快速开始)
- [工作原理](#工作原理)
  - [1. 版本识别（自动）](#1-版本识别自动)
  - [2. 不可遍历检测](#2-不可遍历检测)
  - [3. 探测（HEAD 快探 + 异步 GET 校验）](#3-探测head-快探--异步-get-校验)
  - [4. 探测策略（动态扩展）](#4-探测策略动态扩展)
  - [5. 多平台扩展（`-platform`）](#5-多平台扩展-platform)
  - [6. 管道与非交互输出](#6-管道与非交互输出)
- [选项](#选项)
- [实测结果](#实测结果)

---

## 编译

```bash
go build -o ov .
```

支持 Linux / macOS / Windows，amd64 / arm64 架构。

## 安装（预编译产物）

每个 `v*` tag 由 GitHub Actions 构建出 6 个**裸二进制**（不再二次打包压缩包），
直接附到 [Releases](https://github.com/ejfkdev/ov/releases) 页面：

| 产物 | 说明 |
|---|---|
| `ov-linux-amd64`、`ov-linux-arm64` | 已 UPX 压缩 |
| `ov-darwin-amd64`、`ov-darwin-arm64` | 不压缩（UPX 不支持 macOS 二进制） |
| `ov-windows-amd64.exe`、`ov-windows-arm64.exe` | 工具链支持该格式时 UPX 压缩 |

二进制均已去除符号表、调试信息、编译路径等全部额外信息
（`-s -w -trimpath -buildvcs=false -buildid=`）并静态链接，体积尽可能小。
`-version`/`-V` 与 `-h` 中显示的版本号即对应 tag 的版本号。

## 快速开始

```bash
# 标准版本号，自动枚举同系列全部可下载版本
./ov "https://autoglm.aminer.cn/autoclaw/updates/autoclaw-1.17.4-cn.dmg"

# 输出大小与类型、新版本在前
./ov -sizes -reverse "https://download.manus.im/Manus-Setup-1.7.2.dmg"

# 多平台高级模式：从 windows exe 发现 mac / linux 的全部安装包
./ov -platform "https://cdn-zcode.z.ai/zcode/electron/releases/3.8.1/windows-x64/ZCode-3.8.1-win-x64.exe"

# 发布目录改过版（旧版少一层子目录）也能找回旧版
./ov -path-variants "https://cdn-zcode.z.ai/zcode/electron/releases/3.10.2/windows-x64/ZCode-3.10.2-win-x64.exe"

# 走代理 / 带自定义头（curl 风格）
./ov -x http://127.0.0.1:7890 "https://host/app-1.7.2.dmg"
./ov -H "Authorization: Bearer xxx" "https://host/app-1.7.2.dmg"

# 管道/脚本消费: 只流式输出 URL, 一行一个, 无任何进度噪音
./ov "https://host/app-1.7.2.dmg" | while read -r url; do echo "发现 $url"; done
```

## 工作原理

### 1. 版本识别（自动）

自动识别 URL 中的版本号：标准版本（1.17.4）、日期（2025.08.22）、
纯数字构建号（754）、数字+字母混合（3.0.2-arm64）等。
识别结果按"带点程度+出现顺序"去重保留，
**URL 中所有同版本串一次性全部替换**为占位符（多个版本号同时修改，如 ZCode）。

### 2. 不可遍历检测

路径含 6+ 位十六进制串/哈希目录（如 QQ 的 `3f89efc5`）、UUID、7+ 位数字目录、
Build 号或长数字序列的 URL 无法枚举，直接提示并退出。
确认可枚举时可 `-force-tpl` 强制使用 `{v}` 模板。

### 3. 探测（HEAD 快探 + 异步 GET 校验）

1. **HEAD** 请求：404 直接判不存在（绝大多数候选在此结束，极快）；
2. HEAD 响应头已表明是下载文件时**直接确认、跳过 GET**：
   - `Content-Disposition: attachment`；
   - 或体积 ≥ 1 MiB 且 Content-Type 不像文本（真实安装包均为 MB 级二进制，
     "假 200"几乎总是小体积文本页）。
3. 其余 2xx（可能"假 200"）或不支持 HEAD（405/403/5xx）→ 由**独立校验队列**
   并发 **GET + `Range: bytes=0-2047`** 只读开头 2KB，不阻塞发现流水线：
   - 内容为文本（HTML/JSON 错误页）→ 判为不可下载；
   - 内容非文本（压缩包/安装包魔法字节）→ 命中；
4. 逐字节判断已知魔法：zip/apk/jar、ELF、MZ(exe)、mach-o、7z/rar/pdf、gzip/bzip2/xz、tar、dmg、deb/ar，
   `-sizes` 可查看大小与类型；
5. 416 视为存在（空文件）；网络错误/429/5xx 自动重试一次；
6. 带 `?query` 的 URL 未命中时会**去掉参数再试一次**（kimi 的 `?download_id=` 场景）。

### 4. 探测策略（动态扩展）

版本空间理论上限是 200×200×200 = 8,000,000，但实际命中率极低，所以采用
**历史区枚举 + 广撒网 + 前沿生长** 的动态策略：

**历史区**（≤ 已知版本）：先逐主版本预扫代表入口，空主版本整段跳过；
对保活的主版本按组件组合枚举（真实锚点的历史区通常只有几十到几百个请求），
不遗漏老版本。组件数以识别版本为准，前导零格式（`2025.08.22`）原样保留。

**广撒网**（主版本）：对锚点附近稠密、远端稀疏撒点、当前年份附近，
每主版本试几个代表入口，命中即展开——从中间版本（如 1.17.4）也能
一次撒网探到远处主版本（如 3.0.0）。

**前沿区**（> 已知版本）：逐维度"生长"——主版本前沿连续 5 次未命中即停、
副版本前沿同样连停即止、命中基座再线性探测补丁。总请求数有硬顶（默认 500000）。

**滚动窗口**：候选空间过大（如单组件大数字 72203）时自动触发，
连续 200 次未命中即停（命中刷新）。

### 5. 多平台扩展（`-platform`）

把 URL 中的平台/架构/扩展名标记当作**槽位**（如 `windows-x64` 目录、
`win-x64` 文件名、`.exe` 扩展名），组合替换生成候选并逐一探测。
已发现：zcode 从 windows → macos/linux 全平台、MiniMax Design arm64 → x64、
kimi mac → windows exe。组合数上限保护（128）。

### 6. 管道与非交互输出

ov 会自动检测运行方式，输出行为随之切换：

**交互式运行**（stdout 与 stderr 都是终端）：
- 进度与阶段信息（模板/识别/探测/完成）打到 **stderr**；
- 结果在探测结束后按版本排序统一输出到 **stdout**（`-sizes` 附加大小与类型）。

**非交互运行**（stdout 或 stderr 任一为管道/文件/程序捕获，如 `./ov URL | cat`）：
- **stdout 只输出 URL，一行一个**，且**实时流式**——发现一个确认可下载的
  URL 立即打印一个（HEAD 头确认的在发现当下、需魔数校验的在 GET 校验通过当下），
  不必等整个探测结束；
- **stderr 完全静默**：不打印任何进度/说明信息（错误信息除外）。

因此可直接串联到管道或脚本里逐行消费：

```bash
./ov "https://host/app-1.7.2.dmg" | while read -r url; do
    echo "下载 $url"
done
```

## 选项

| 选项 | 默认 | 说明 |
| --- | ---: | --- |
| 位置参数 | — | 下载地址（含版本号，或含 `{v}` 占位符） |
| `-c` | `50` | 并发数 |
| `-timeout` | `10s` | 单请求超时 |
| `-k` | false | 跳过 TLS 证书校验 |
| `-x` | 无 | 代理（curl 风格）：`http://host:port` 或 `socks5://host:port` |
| `-H` | 无 | 自定义请求头（curl 风格，可重复）：`"Name: Value"` |
| `-sizes` | false | 输出文件大小与类型 |
| `-reverse` | false | 结果按新到旧输出 |
| `-platform` | false | 高级模式：发现其他平台/架构的下载地址 |
| `-path-variants` | false | 主形态 404 时试探通用路径变体（去掉一层发布子目录 / 同 OS 换架构），用于跨发布目录布局漂移找回旧版 URL |
| `-tls-fingerprint` | (原生) | TLS 指纹伪装：`chrome`/`firefox`/`ios`/`android`/`edge`/`safari` 等，自动支持 HTTP/2 |
| `-force-tpl` | false | 跳过不可遍历检查，强制模板模式（需地址含 `{v}`） |
| `-version`, `-V` | — | 显示版本号与仓库地址 |

CLI 文案（帮助、进度、错误）随系统语言自动切换：中文语系（含 zh_HK / zh_TW）
显示中文，其余情况显示英文。

## 实测结果

<details>
<summary>点击展开全部实测数据</summary>

| 地址 | 命中 | 请求数 | 备注 |
| --- | ---: | ---: | --- |
| AutoTyper-072203.exe | 0 | 21 | 滚动窗口模式；域名仅此一版 |
| Trae 2.3.73734 dmg | 1 | 51 | 滚动窗口模式；命中锚点自身 |
| autoclaw 1.17.4 dmg | 26 | 315 | 历史区 288 + 前沿 27；含 3.0.0 |
| xingye 754 APK | 53 | 117 | 滚动窗口模式；686~782 |
| MiniMax Design 3.0.2 arm64 dmg | 3 | 39 | 历史区 24 + 前沿 15 |
| MiniMax Design 3.0.2 x64 dmg | 3 | 39 | 同上 |
| MiniMax Design 3.0.2 x64-Setup.exe | 3 | 39 | 同上 |
| ZCode windows-x64 3.10.2 | 20 | 228 | 主版本预扫跳过空旧主版本（历史区 264 → 66） |
| kimi mac | 15 | 281 | 历史区 225 + 前沿 56 |
| jadx 1.5.0 | 90 | 988 | 含日期式版本 |

</details>