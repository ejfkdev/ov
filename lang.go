package main

// 界面文案目录: 每门语言一张 map, 调用点只传语义 key(见 t())。
// 新增语言 = 新增一张 msg<Lang> 目录 + 在 catalogs/supportedLangs 各登记一行, 调用点零改动。
// key 以英文文案为准绳(缺语言/缺条目时一律回退英文)。

import (
	"fmt"
	"os"
	"strings"
)

// lang 当前界面语言代码("zh"、"en" 或未来新增语言)。
var lang = detectLang()

// supportedLangs 语言注册表: 代码 → locale 检测前缀(表按顺序尝试)。
var supportedLangs = []struct {
	code     string
	prefixes []string
}{
	{"zh", []string{"zh"}}, // zh_CN / zh_TW / zh_HK / zh_SG 等中文语系
	// 未来示例: {"ja", []string{"ja"}}, {"ko", []string{"ko"}}
}

// catalogs 全部语言的文案目录。
var catalogs = map[string]map[string]string{
	"zh": msgZh,
	"en": msgEn,
}

// detectLang 按 LC_ALL → LC_MESSAGES → LANG 顺序识别系统语言, 默认英文。
func detectLang() string {
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		v := strings.ToLower(os.Getenv(k))
		for _, e := range supportedLangs {
			for _, p := range e.prefixes {
				if strings.HasPrefix(v, p) {
					return e.code
				}
			}
		}
	}
	return "en"
}

// t 取当前语言文案: 目录缺条目(该语言未译)时回退英文; 带参数时按 fmt 格式化。
func t(key string, args ...any) string {
	s := catalogs[lang][key]
	if s == "" {
		s = msgEn[key]
	}
	if len(args) == 0 {
		return s
	}
	return fmt.Sprintf(s, args...)
}

// msgEn 英文目录(基准语言, 内容最全)。
var msgEn = map[string]string{
	"anchor":                         "anchor: %s\n",
	"binary":                         "binary",
	"bytes":                          "%d bytes",
	"candidateSpaceTooLarge":         "candidate space too large (%d+); switching to rolling window",
	"detectedVersions":               "detected %d version(s): %s\n",
	"dmgEncrypted":                   "dmg (encrypted)",
	"doneRequestsHitsTook":           "done: %d requests, %d hits, took %s\n",
	"emptyResponse":                  "empty response",
	"fake200Http":                    "\n[fake200] %s -> %s (HTTP %d)\n",
	"fatalPrefix":                    "error: ",
	"generatingPlatformVariants":     "generating %d platform variants...\n",
	"hasSizeButNoContent":            "has size but no content",
	"headConfirmed":                  "HEAD confirmed",
	"headHasSize":                    "HEAD has size",
	"invalidHeaderExpectedNameValue": "invalid header (expected \"Name: Value\"): %s",
	"invalidProxyUrl":                "invalid proxy URL: %s (e.g. http://127.0.0.1:7890 or socks5://127.0.0.1:1080)",
	"invalidRange":                   "invalid range: %s > %s",
	"invalidVersionCount":            "invalid version %q: 1-%d components expected",
	"invalidVersionEmptyComponent":   "invalid version %q: empty component",
	"invalidVersionValue":            "invalid version %q: components must be 0-%d",
	"machOUniversal":                 "mach-o universal",
	"majorPrescanSummary":            "major prescan: %d live majors, history candidates %d -> %d\n",
	"networkError":                   "network error",
	"noEnumerableVersion":            "no enumerable version found; use -force-tpl with a {v} template",
	"noVersionInUrl":                 "no version found in URL; cannot determine probe range",
	"originalUrlNotDownloadable":     "original URL not downloadable now: %s (status %d); trying platform variants anyway\n",
	"pathHasBuildSuffix":             "path contains both a version and a build/date suffix, not traversable",
	"pathHasDigitDir":                "path contains a 7+ digit dir (distribution ID, not traversable)",
	"pathHasHexToken":                "path contains a 6+ hex token (signature/hash dir, not traversable)",
	"pathHasUUID":                    "path contains a UUID (random signature, not traversable)",
	"pending":                        "pending",
	"platform":                       "[platform] ",
	"platformVariantsCandidates":     "platform variants: %d candidates\n",
	"probingConcurrency":             "probing: concurrency %d\n",
	"probingRequestsHits":            "\rprobing ... %d requests, %d hits    ",
	"proxyConnectFailed":             "proxy CONNECT failed: %s",
	"switchingToRollingWindow":       "candidate space too large; switching to rolling window (stop after %d consecutive misses)\n",
	"template":                       "template: %s\n",
	"templateNoPlaceholder":          "template has no {v} placeholder: %s",
	"textErrorPage":                  "text error page",
	"unknownBinaryStrictMode":        "unknown binary (strict mode)",
	"unknownSize":                    "unknown size",
	"unknownTooShort":                "unknown (too short)",
	"unsupportedProxy":               "unsupported proxy: %s (supported: http, https, socks5)",
	"urlHasNoPath":                   "URL has no path",
	"urlMustStartWithHttpOrHttps":    "URL must start with http:// or https://: %s",
	"urlNotTraversable":              "URL not traversable: %s\n  %s\n  if you are sure it is enumerable, use -force-tpl (URL must contain {v})",
	"urlSignedQuery":                 "URL has a signed/authenticated query (signatures differ per version) and cannot be traversed",
	"usage": `ov v%s — enumerate every downloadable version from one versioned link
Repository: %s

Usage:
  ov [options] <download-url>

Examples:
  # Basic: auto-detect the version, enumerate all downloadable versions
  ov "https://host/app-1.7.2.dmg"

  # Piped: stream found URLs only, one per line, as discovered
  ov "https://host/app-1.7.2.dmg" | while read -r u; do curl -LO "$u"; done

Options:
  -c N              probe concurrency (default 50)
  -timeout D        per-request timeout (default 10s)
  -k                skip TLS certificate verification
  -x PROXY          proxy (curl style): http://host:port or socks5://host:port
  -H "Name: Value"  custom request header (curl style, repeatable)
  -sizes            print file size (bytes) and type
  -reverse          output newest first
  -platform         discover other platform/arch variants from the URL
  -path-variants    on 404, fall back to generic path variants (drop a subdir / swap arch)
  -tls-fingerprint F TLS fingerprint spoofing: chrome, firefox, ios, android, ...
  -force-tpl        force {v} template probing (bypass non-traversable check)
  -version, -V      show version and repository
`,
}

// msgZh 中文目录。
var msgZh = map[string]string{
	"anchor":                         "锚点: %s\n",
	"binary":                         "二进制",
	"bytes":                          "%d 字节",
	"candidateSpaceTooLarge":         "候选空间过大(%d+), 将切换滚动窗口模式",
	"detectedVersions":               "识别到 %d 个版本串: %s\n",
	"dmgEncrypted":                   "dmg(加密)",
	"doneRequestsHitsTook":           "完成: %d 个请求, 命中 %d 个, 耗时 %s\n",
	"emptyResponse":                  "空响应",
	"fake200Http":                    "\n[假200] %s -> %s (HTTP %d)\n",
	"fatalPrefix":                    "错误: ",
	"generatingPlatformVariants":     "生成 %d 个平台变体, 探测中...\n",
	"hasSizeButNoContent":            "有大小无内容",
	"headConfirmed":                  "HEAD 确认",
	"headHasSize":                    "HEAD 有大小",
	"invalidHeaderExpectedNameValue": "请求头格式错误(应为 \"Name: Value\"): %s",
	"invalidProxyUrl":                "代理地址格式错误: %s (示例: http://127.0.0.1:7890 或 socks5://127.0.0.1:1080)",
	"invalidRange":                   "探测范围不合法: %s 大于 %s",
	"invalidVersionCount":            "非法版本 %q: 组件数量应在 1-%d 之间",
	"invalidVersionEmptyComponent":   "非法版本 %q: 存在空组件",
	"invalidVersionValue":            "非法版本 %q: 组件必须是 0-%d 的数字",
	"machOUniversal":                 "mach-o 通用",
	"majorPrescanSummary":            "主版本预扫: 保活 %d 个主版本, 历史区候选 %d -> %d 个\n",
	"networkError":                   "网络错误",
	"noEnumerableVersion":            "没有识别到可枚举的版本号。可用 -force-tpl 使用 {v} 模板",
	"noVersionInUrl":                 "地址中未找到版本号, 无法自动确定探测范围",
	"originalUrlNotDownloadable":     "原 URL 当前不可下载: %s (状态 %d), 仍尝试平台变体\n",
	"pathHasBuildSuffix":             "路径同时含版本号与 Build/日期后缀, 不可遍历",
	"pathHasDigitDir":                "路径含 7+ 位数字目录(分发 ID, 不可遍历)",
	"pathHasHexToken":                "路径含 6+ 位十六进制串(签名/哈希目录, 不可遍历)",
	"pathHasUUID":                    "路径含 UUID(随机签名, 不可遍历)",
	"pending":                        "待校验",
	"platform":                       "[平台] ",
	"platformVariantsCandidates":     "平台变体探测: %d 个候选\n",
	"probingConcurrency":             "探测: 并发 %d\n",
	"probingRequestsHits":            "\r探测中 ... %d 次请求, 命中 %d 个    ",
	"proxyConnectFailed":             "代理 CONNECT 失败: %s",
	"switchingToRollingWindow":       "候选空间过大, 切换为滚动窗口模式 (连续 %d 次未命中停止)\n",
	"template":                       "模板: %s\n",
	"templateNoPlaceholder":          "模板中没有 {v} 占位符: %s",
	"textErrorPage":                  "文本错误页",
	"unknownBinaryStrictMode":        "未知二进制(严格模式)",
	"unknownSize":                    "大小未知",
	"unknownTooShort":                "未知(过短)",
	"unsupportedProxy":               "不支持的代理地址: %s (支持 http、https、socks5)",
	"urlHasNoPath":                   "地址没有路径",
	"urlMustStartWithHttpOrHttps":    "地址必须以 http:// 或 https:// 开头: %s",
	"urlNotTraversable":              "该地址不可遍历: %s\n  %s\n  若确认可枚举, 可用 -force-tpl 强制切换为模板模式(需地址含 {v})",
	"urlSignedQuery":                 "URL 含签名/鉴权查询串(不同版本的签名不同, 换版本即失效), 不可遍历",
	"usage": `ov v%s — 从一条带版本号的下载链接, 自动枚举全部可下载版本
仓库: %s

用法:
  ov [选项] <下载地址>

示例:
  # 基本用法: 自动识别版本号, 枚举出全部可下载版本
  ov "https://host/app-1.7.2.dmg"

  # 管道: 只实时输出发现的 URL, 一行一个
  ov "https://host/app-1.7.2.dmg" | while read -r u; do curl -LO "$u"; done

选项:
  -c N              并发探测数 (默认 50)
  -timeout D        单请求超时 (默认 10s)
  -k                跳过 TLS 证书校验
  -x PROXY          代理 (curl 风格): http://host:port 或 socks5://host:port
  -H "Name: Value"  自定义请求头 (curl 风格, 可重复)
  -sizes            输出文件大小(字节)与类型
  -reverse          结果按新到旧输出
  -platform         从给定 URL 探测其他平台/架构变体
  -path-variants    主路径 404 时回退通用路径变体(去子目录/换架构名)
  -tls-fingerprint F TLS 指纹伪装: chrome、firefox、ios、android 等
  -force-tpl        强制使用 {v} 模板探测(绕过不可遍历检查)
  -version, -V      显示版本号与仓库地址
`,
}
