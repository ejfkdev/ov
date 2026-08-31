package main

// 多平台扩展: 从一个平台的下载 URL 生成其他平台/架构的候选 URL 并探测。
// 把 URL 里的平台/架构/扩展名标记视为"槽位", 每个槽位可选原值或替换值,
// 组合生成全部变体(上限保护); 扩展名含大小写变体(如 .AppImage)。

import (
	"net/http"
	"sort"
	"strings"
)

// platformToken 一个可替换的平台标记。
type platformToken struct {
	keyword string // 需要匹配的词(小写)
	alts    []string
}

// 平台/架构/扩展名标记表(顺序即优先级, 先匹配更具体的)。
var platformCatalog = []platformToken{
	{"windows-x64", []string{"macos-arm64", "macos-x64", "linux-x64", "linux-arm64"}},
	{"windows-x86", []string{"macos-arm64", "macos-x64", "linux-x64", "linux-arm64"}},
	{"win-x64", []string{"mac-arm64", "mac-x64", "linux-x64", "linux-arm64"}},
	{"win-x86", []string{"mac-arm64", "mac-x64", "linux-x64", "linux-arm64"}},
	{"macos-arm64", []string{"windows-x64", "linux-x64", "linux-arm64"}},
	{"macos-x64", []string{"windows-x64", "linux-x64", "linux-arm64"}},
	{"mac-arm64", []string{"win-x64", "linux-x64", "linux-arm64"}},
	{"mac-x64", []string{"win-x64", "linux-x64", "linux-arm64"}},
	{"linux-arm64", []string{"windows-x64", "macos-arm64", "linux-x64"}},
	{"linux-x64", []string{"windows-x64", "macos-arm64", "linux-arm64"}},
	{"macos", []string{"windows", "linux", "android"}},
	{"mac", []string{"windows", "linux", "android"}},
	{"darwin", []string{"windows", "linux"}},
	{"windows", []string{"macos", "linux", "android"}},
	{"linux", []string{"macos", "windows", "android"}},
	{"android", []string{"macos", "windows", "linux"}},
	{"arm64", []string{"x64"}},
	{"aarch64", []string{"x64"}},
	{"x64", []string{"arm64"}},
	{"amd64", []string{"arm64"}},
	{"x86_64", []string{"arm64"}},
	{"x86", []string{"arm64"}},
	{"universal", []string{"x64", "arm64", "intel"}},
	{".exe", []string{".dmg", ".deb", ".appimage"}},
	{".dmg", []string{".exe", ".deb", ".appimage"}},
	{".deb", []string{".dmg", ".exe", ".appimage", ".rpm"}},
	{".appimage", []string{".dmg", ".deb", ".exe"}},
	{".apk", []string{".dmg", ".exe"}},
}

const (
	maxPlatformCombos = 128
	maxPlatformSlots  = 4
)

// platformSlot 一个匹配到的槽位。
type platformSlot struct {
	start, end int
	alts       []string
}

// platformVariants 组合生成一个 URL 的其他平台变体。
// 每个匹配槽位要么保持原值, 要么换成某个替换值; 全原值(原 URL)除外。
func platformVariants(rawURL string) []string {
	lower := strings.ToLower(rawURL)

	// 找出所有不重叠的槽位(按出现顺序), 同位置的取更长关键词。
	var slots []platformSlot
	for _, tok := range platformCatalog {
		for _, loc := range findToken(lower, tok.keyword) {
			overlap := false
			for _, s := range slots {
				if loc[0] < s.end && loc[1] > s.start {
					overlap = true
					break
				}
			}
			if overlap {
				continue
			}
			slots = append(slots, platformSlot{loc[0], loc[1], tok.alts})
		}
		if len(slots) >= maxPlatformSlots {
			break
		}
	}
	if len(slots) == 0 {
		return nil
	}

	// 组合数 = ∏(|alt|+1) - 1(去掉全原值)。
	combos := 1
	for _, s := range slots {
		combos *= len(s.alts) + 1
	}
	if combos-1 > maxPlatformCombos {
		return nil // 组合爆炸, 放弃(避免对目标打太多请求)
	}

	seen := map[string]bool{}
	out := []string{}
	for mask := 1; mask < combos; mask++ { // 0 = 全原值, 跳过
		var sb strings.Builder
		prev := 0
		m := mask
		for _, s := range slots {
			choice := m % (len(s.alts) + 1)
			m /= len(s.alts) + 1
			sb.WriteString(rawURL[prev:s.start])
			if choice == 0 {
				sb.WriteString(rawURL[s.start:s.end])
			} else {
				sb.WriteString(s.alts[choice-1])
			}
			prev = s.end
		}
		sb.WriteString(rawURL[prev:])
		u := sb.String()
		for _, cu := range caseExtVariants(u) {
			if !seen[cu] {
				seen[cu] = true
				out = append(out, cu)
			}
		}
	}
	sort.Strings(out)
	return out
}

// caseExtVariants 对可能大小写敏感的扩展名(如 .AppImage)补充大小写变体。
func caseExtVariants(u string) []string {
	if !strings.Contains(u, ".appimage") {
		return []string{u}
	}
	return []string{u, strings.ReplaceAll(u, ".appimage", ".AppImage")}
}

// findToken 在 s 里找 keyword 的所有出现位置。
// 以 "." 开头的扩展名(如 .exe)左侧是文件基名, 不做左边界检查。
func findToken(s, keyword string) [][2]int {
	var out [][2]int
	idx := 0
	for {
		i := strings.Index(s[idx:], keyword)
		if i < 0 {
			break
		}
		abs := idx + i
		beforeOK := true
		if !strings.HasPrefix(keyword, ".") {
			beforeOK = abs == 0 || !isWordChar(s[abs-1])
		}
		after := abs + len(keyword)
		afterOK := after >= len(s) || !isWordChar(s[after])
		if beforeOK && afterOK {
			out = append(out, [2]int{abs, after})
		}
		idx = abs + 1
	}
	return out
}

func isWordChar(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9'
}

// maxPathVariants 限制单个候选的 URL 形态变体数(含主形态), 防止请求爆炸。
const maxPathVariants = 6

// archTokenPairs 架构 token 的等价替换组(只在"同 OS 同扩展名"内换架构, 不跨平台/跨格式)。
var archTokenPairs = [][]string{
	{"aarch64", "arm64"},
	{"amd64", "x86_64", "x64"},
	{"x86_64", "x64", "amd64"},
	{"arm64", "aarch64"},
	{"x64", "amd64", "x86_64"},
}

// archVariants 仅替换架构 token(保持 OS 标签与扩展名不变), 用于布局漂移的低成本兜底。
func archVariants(u string) []string {
	lower := strings.ToLower(u)
	seen := map[string]bool{u: true}
	out := []string{}
	for _, group := range archTokenPairs {
		src := group[0]
		// 词边界匹配(左侧非字母数字)。
		for _, loc := range findToken(lower, src) {
			for _, alt := range group[1:] {
				if alt == src {
					continue
				}
				cand := u[:loc[0]] + mapCase(u[loc[0]:loc[1]], alt) + u[loc[1]:]
				if !seen[cand] {
					seen[cand] = true
					out = append(out, cand)
				}
			}
		}
	}
	return out
}

// mapCase 尽量保留原 token 的大小写风格(全大写/首字母大写/全小写)。
func mapCase(orig, repl string) string {
	if orig == strings.ToUpper(orig) {
		return strings.ToUpper(repl)
	}
	if len(orig) > 0 && orig[0] >= 'A' && orig[0] <= 'Z' {
		return strings.ToUpper(repl[:1]) + repl[1:]
	}
	return repl
}

// pathVariants 生成一个候选 URL 的若干"通用路径形态"(保序去重, 首个为主形态)。
// 不针对任何具体站点, 只做两类与版本无关、且保持同一 OS/扩展名的结构试探:
//  1. 去掉紧邻文件的最后一级静态目录段(发布目录改版, 如旧版少一层 macos-x64/ 子目录);
//  2. 仅架构 token 替换(同 OS 同扩展, 如 mac-arm64 ↔ mac-x64)。
// 用于版本枚举中主形态 404 时低成本试探同一产物类型(other 架构/少一层目录)的发布路径。
// 刻意不做跨 OS(mac↔win)或跨扩展名(.dmg↔.exe)替换——那是 -platform 的职责, 放这里会请求爆炸。
func pathVariants(u string) []string {
	seen := map[string]bool{u: true}
	out := []string{u}
	add := func(x string) {
		if x != "" && !seen[x] && len(out) < maxPathVariants {
			seen[x] = true
			out = append(out, x)
		}
	}
	// 1. 去掉一层发布子目录(同架构/同扩展名)。
	var collapsed []string
	if c, ok := dropLastDir(u); ok {
		add(c)
		collapsed = append(collapsed, c)
	}
	// 2. 原结构的纯架构替换。
	for _, av := range archVariants(u) {
		add(av)
	}
	// 3. 去目录 + 纯架构替换(目录和文件名同时改版)。
	for _, c := range collapsed {
		for _, av := range archVariants(c) {
			add(av)
		}
	}
	return out
}

// dropLastDir 去掉文件段之前紧邻的那一级目录(通用结构变体)。
func dropLastDir(u string) (string, bool) {
	base, query := u, ""
	if i := strings.IndexByte(u, '?'); i >= 0 {
		base, query = u[:i], u[i:]
	}
	fi := strings.LastIndexByte(base, '/')
	if fi <= 0 {
		return "", false
	}
	dir, file := base[:fi], base[fi+1:]
	if file == "" {
		return "", false
	}
	// dir 形如 "https://host/a/b/c" ; 去掉末级 c。
	si := strings.LastIndexByte(dir, '/')
	if si <= 0 { // "https://" 之后至少要有一级
		return "", false
	}
	// 末级目录段若含 '{v}' 则跳过(版本目录本身不能去掉)。
	if strings.Contains(dir[si+1:], "{v}") {
		return "", false
	}
	return dir[:si] + "/" + file + query, true
}

// probePlatforms 探测一个 URL 的其他平台变体, 返回可下载的变体列表。
func probePlatforms(hc *http.Client, ua, rawURL string) []foundURL {
	var results []foundURL
	for _, v := range platformVariants(rawURL) {
		r := probeURL(hc, v, ua)
		if r.found {
			results = append(results, foundURL{URL: v, Size: r.size, Kind: r.kind, Status: r.status})
		}
	}
	return results
}

type foundURL struct {
	URL    string
	Size   int64
	Kind   string
	Status int
}
