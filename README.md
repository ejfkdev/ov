# ov — download URL version prober

> :cn: 中文文档: [README.zh-CN.md](./README.zh-CN.md)

[![Go Version](https://img.shields.io/badge/Go-1.22-blue)](https://golang.org)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

Give it one download link containing a version number; it auto-detects the version
(standard / date / plain number / alphanumeric), enumerates every version combination
and **probes them concurrently**, printing all downloadable full URLs.
You can also discover other platform builds from a single-platform URL (`-platform`).
Interactive and piped/scripted usage are handled automatically (see
[Piped & non-interactive output](#6-piped--non-interactive-output)).

## Table of contents

- [Build](#build)
- [Install (prebuilt releases)](#install-prebuilt-releases)
- [Quick start](#quick-start)
- [How it works](#how-it-works)
  - [1. Version detection (auto)](#1-version-detection-auto)
  - [2. Non-traversable detection](#2-non-traversable-detection)
  - [3. Probing (fast HEAD + async GET verification)](#3-probing-fast-head--async-get-verification)
  - [4. Probe strategy (dynamic expansion)](#4-probe-strategy-dynamic-expansion)
  - [5. Multi-platform (`-platform`)](#5-multi-platform--platform)
  - [6. Piped & non-interactive output](#6-piped--non-interactive-output)
- [Options](#options)
- [Real-world results](#real-world-results)

---

## Build

```bash
go build -o ov .
```

Linux / macOS / Windows, amd64 / arm64 are supported.

## Install (prebuilt releases)

Every `v*` tag is built by GitHub Actions into 6 **bare binaries** (no archive),
attached to the [Releases](https://github.com/ejfkdev/ov/releases) page:

| Asset | Notes |
|---|---|
| `ov-linux-amd64`, `ov-linux-arm64` | UPX-compressed |
| `ov-darwin-amd64`, `ov-darwin-arm64` | not compressed (UPX does not support macOS binaries) |
| `ov-windows-amd64.exe`, `ov-windows-arm64.exe` | UPX-compressed when the toolchain supports the format |

Binaries are statically linked and stripped of all extra info
(`-s -w -trimpath -buildvcs=false -buildid=`) for the smallest possible size.
The version shown by `-version`/`-V` and in `-h` is the tag version.

## Quick start

```bash
# Standard versioning — auto-enumerate the whole family
./ov "https://autoglm.aminer.cn/autoclaw/updates/autoclaw-1.17.4-cn.dmg"

# Print sizes & types, newest first
./ov -sizes -reverse "https://download.manus.im/Manus-Setup-1.7.2.dmg"

# Discover mac / linux builds from a windows exe
./ov -platform "https://cdn-zcode.z.ai/zcode/electron/releases/3.8.1/windows-x64/ZCode-3.8.1-win-x64.exe"

# Recover older versions when the release layout changed
./ov -path-variants "https://cdn-zcode.z.ai/zcode/electron/releases/3.10.2/windows-x64/ZCode-3.10.2-win-x64.exe"

# Proxy / custom headers (curl style)
./ov -x http://127.0.0.1:7890 "https://host/app-1.7.2.dmg"
./ov -H "Authorization: Bearer xxx" "https://host/app-1.7.2.dmg"

# Pipe/script consumption: pure URL stream, one line each
./ov "https://host/app-1.7.2.dmg" | while read -r url; do echo "found $url"; done
```

## How it works

### 1. Version detection (auto)

The version in the URL is detected automatically: standard versions (1.17.4),
dates (2025.08.22), plain build numbers (754), alphanumeric mixes (3.0.2-arm64).
Detections are deduplicated by "dottedness + order of appearance", and
**every occurrence of the same version string in the URL is replaced at once**
(useful for URLs like ZCode's, where the version appears twice).

### 2. Non-traversable detection

URLs whose path contains a 6+ hex token / hash directory (e.g. QQ's `3f89efc5`),
a UUID, a 7+ digit directory, a build number, or a long numeral cannot be
enumerated — ov prints a reason and exits.
If you are sure the URL is enumerable, use `-force-tpl` with a `{v}` template.

### 3. Probing (fast HEAD + async GET verification)

1. **HEAD** request — a 404 immediately rules the candidate out (most candidates end here, very fast);
2. If the HEAD response headers already prove it is a download file, **confirm without a GET**:
   - `Content-Disposition: attachment`; or
   - size ≥ 1 MiB and a non-text Content-Type (real installers are multi-MB binaries;
     fake-200 pages are almost always small text pages).
3. Everything else that returns 2xx (possibly a "fake 200") or lacks HEAD support
   (405/403/5xx) goes through a **separate verify queue** that concurrently issues
   **GET + `Range: bytes=0-2047`** reading only the first 2KB, without blocking the
   discovery pipeline:
   - text content (HTML/JSON error page) → not downloadable;
   - non-text content (magic bytes of archives/installers) → hit;
4. Known magics are matched byte-wise: zip/apk/jar, ELF, MZ(exe), mach-o, 7z/rar/pdf,
   gzip/bzip2/xz, tar, dmg, deb/ar — `-sizes` prints size and type;
5. A 416 counts as existing (empty file); network errors/429/5xx are retried once;
6. URLs with `?query` that miss are retried **without the query string**
   (kimi's `?download_id=` case).

### 4. Probe strategy (dynamic expansion)

The theoretical space is up to 200×200×200 = 8,000,000 versions, but the real
hit-rate is tiny, so ov uses an adaptive strategy:

**History region** (≤ known version): first prescan each old major with a few
representative entries; majors that are entirely dead get skipped, and live majors
are enumerated component-wise (a real anchor usually costs tens to hundreds of
requests) so old versions are never missed. Leading-zero formats (`2025.08.22`)
are preserved.

**Wide net** (majors): dense band around the anchor, sparse flicks further out,
and a current-year band; each candidate major is tested with a few representative
entries and expanded on hit — a mid version (e.g. 1.17.4) can thus reach distant
majors (e.g. 3.0.0).

**Frontier region** (> known version): grows dimension by dimension — the major
frontier stops after 5 consecutive misses, the minor frontier likewise, and patch
exploration fills patches linearly until consecutive misses (patch ranges are
usually dense). Total requests are hard-capped (default 500000).

**Rolling window**: triggered automatically when the candidate space is too large
(e.g. a single big number like 72203); it stops after 200 consecutive misses
(hits refresh the window).

### 5. Multi-platform (`-platform`)

Platform/arch/extension markers in the URL (`windows-x64` dir, `win-x64` filename,
`.exe` extension) are treated as slots; alternatives are combined and probed.
Discovered so far: zcode windows → macos/linux, MiniMax Design arm64 → x64,
kimi mac → windows exe. Combination count is capped (128).

### 6. Piped & non-interactive output

ov auto-detects how it is being run:

**Interactive** (stdout and stderr are both terminals):
- progress and stage info go to **stderr**;
- results are printed sorted to **stdout** at the end (`-sizes` adds size & type).

**Non-interactive** (stdout or stderr is a pipe / file / program, e.g. `./ov URL | cat`):
- **stdout prints URLs only, one per line, streamed in real time** — a URL is
  printed as soon as it is confirmed (HEAD-confirmed hits immediately, suspected
  hits right after GET magic verification);
- **stderr is completely silent**: no progress or status text (errors excepted).

So you can consume it line by line in scripts:

```bash
./ov "https://host/app-1.7.2.dmg" | while read -r url; do
    echo "downloading $url"
done
```

## Options

| Option | Default | Description |
| --- | ---: | --- |
| positional | — | download URL (containing a version, or a `{v}` template) |
| `-c` | `50` | concurrency |
| `-timeout` | `10s` | per-request timeout |
| `-k` | false | skip TLS certificate verification |
| `-x` | none | proxy (curl style): `http://host:port` or `socks5://host:port` |
| `-H` | none | custom request header (curl style, repeatable): `"Name: Value"` |
| `-sizes` | false | print file size and type |
| `-reverse` | false | output newest first |
| `-platform` | false | advanced mode: discover other platform/arch download links |
| `-path-variants` | false | on 404, fall back to generic path variants (drop a subdir / same-OS arch swap) for layout drift |
| `-tls-fingerprint` | (native) | TLS fingerprint spoofing: `chrome`/`firefox`/`ios`/`android`/`edge`/`safari`, HTTP/2 supported |
| `-force-tpl` | false | bypass the non-traversable check, force template mode (URL must contain `{v}`) |
| `-version`, `-V` | — | show version and repository |

CLI text (help, progress, errors) follows the system locale: Chinese locales
(including zh_HK / zh_TW) display Chinese, everything else displays English.

## Real-world results

<details>
<summary>Click to expand real-world measurements</summary>

| URL | hits | requests | notes |
| --- | ---: | ---: | --- |
| AutoTyper-072203.exe | 0 | 21 | rolling-window mode; only one release on the domain |
| Trae 2.3.73734 dmg | 1 | 51 | rolling-window mode; hit the anchor itself |
| autoclaw 1.17.4 dmg | 26 | 315 | history 288 + frontier 27; found 3.0.0 |
| xingye 754 APK | 53 | 117 | rolling-window mode; 686~782 |
| MiniMax Design 3.0.2 arm64 dmg | 3 | 39 | history 24 + frontier 15 |
| MiniMax Design 3.0.2 x64 dmg | 3 | 39 | same |
| MiniMax Design 3.0.2 x64-Setup.exe | 3 | 39 | same |
| ZCode windows-x64 3.10.2 | 20 | 228 | major prescan skipped dead old majors (264 → 66 history) |
| kimi mac | 15 | 281 | history 225 + frontier 56 |
| jadx 1.5.0 | 90 | 988 | date-ish versions too |

</details>
