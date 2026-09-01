package main

// 版本号识别与枚举。
// 支持多模式识别: 标准版本号 / 日期 / 纯数字 / 字母数字混合;
// URL 中所有同版本串会一次性全部替换为占位符(多个版本号同时修改)。

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	maxVersionComponents = 6
	// 版本值上限, 防止日期当作可枚举范围(如 yyyymmdd)。
	maxVersionValue = 99999
)

type versionMode struct {
	name    string
	desc    string
	re      *regexp.Regexp
	enabled bool
}

func newVersionModes(enabled map[string]bool) []*versionMode {
	defs := []struct {
		name, desc, pattern string
	}{
		{"std", "标准版本号 1.17.4 / 4.1.0.175", `\d+(?:\.\d+){1,5}`},
		{"date", "日期 20250822 / 2025.08.22", `\d{8}|\d{4}[.-]\d{2}[.-]\d{2}`},
		{"num", "纯数字 754 / 37271(至少 3 位)", `\d{3,}`},
		{"alnum", "字母数字混合 v1.2.3 / 1.2.3-beta.1", `[vV]?[0-9]+(?:[-_.][0-9]+)+(?:[-_][a-zA-Z0-9]+)*`},
	}
	out := make([]*versionMode, 0, len(defs))
	for _, d := range defs {
		on := enabled[d.name]
		if enabled["auto"] {
			on = true
		}
		out = append(out, &versionMode{name: d.name, desc: d.desc, re: regexp.MustCompile(d.pattern), enabled: on})
	}
	return out
}

// 不可遍历 URL 的特征(签名/随机串/版本无关的哈希目录)。
var (
	reHexToken    = regexp.MustCompile(`/([a-f0-9]{6,})(/|\.)`)
	reUUID        = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	reLongNumPath = regexp.MustCompile(`/\d{7,}(/|\.)`)
	reBuildSeq    = regexp.MustCompile(`[Bb]uild\d{4,}|[._-]\d{6,}`)
	// 查询串里的签名/鉴权参数: 换版本会使签名失效(403), 不可遍历。
	// 覆盖 CloudFront(Key-Pair-Id/Signature)、S3(X-Amz-Signature)、OSS、
	// 七牛(q-sign)、通用 token/sign/auth_key 等。
	reQuerySig = regexp.MustCompile(`(?i)([?&](x-?amz-|key-pair-id|signature|awsaccesskeyid|q-sign|ossaccesskeyid|accesskeysecret|security-token|x-?oss-)=[^&]*|[?&](sign|token|auth_key|authkey|access_token|secret|sig)=)`)
	// stdVersionRe 用于 isTraversable 判断"路径是否已有点分隔的独立版本号"。
	stdVersionRe = regexp.MustCompile(`\d+(?:\.\d+){1,5}`)
)

// isTraversable 判断 URL 是否适合版本遍历。
// 带签名/随机字符/哈希目录的 URL(如 QQ 安装包)无法遍历, 返回 false 及原因。
func isTraversable(u string) (bool, string) {
	// 先查查询串签名参数(在剥离 query 前): CloudFront/OSS/S3 等带签名的直链,
	// 换版本号会使签名失效返回 403, 无法遍历。
	if reQuerySig.MatchString(u) {
		return false, tr("URL 含签名/鉴权查询串(不同版本的签名不同, 换版本即失效), 不可遍历", "URL has a signed/authenticated query (signatures differ per version) and cannot be traversed")
	}
	path := u
	if i := strings.Index(path, "://"); i >= 0 {
		path = path[i+3:]
	}
	if i := strings.IndexByte(path, '/'); i >= 0 {
		path = path[i:]
	} else {
		return false, tr("地址没有路径", "URL has no path")
	}
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}

	if reUUID.MatchString(path) {
		return false, tr("路径含 UUID(随机签名, 不可遍历)", "path contains a UUID (random signature, not traversable)")
	}
	if reHexToken.MatchString(path) {
		return false, tr("路径含 6+ 位十六进制串(签名/哈希目录, 不可遍历)", "path contains a 6+ hex token (signature/hash dir, not traversable)")
	}
	if reLongNumPath.MatchString(path) {
		return false, tr("路径含 7+ 位数字目录(分发 ID, 不可遍历)", "path contains a 7+ digit dir (distribution ID, not traversable)")
	}
	// Build/日期后缀(如 _260609、-20260819、.36279234)。
	// 仅当路径同时存在点分隔的独立版本号时才判定为"构建元数据";
	// 否则(如 AutoTyper_072203, 长数字本身就是版本)放行交给识别。
	if reBuildSeq.MatchString(path) && stdVersionRe.MatchString(path) {
		return false, tr("路径同时含版本号与 Build/日期后缀, 不可遍历", "path contains both a version and a build/date suffix, not traversable")
	}
	return true, ""
}

// detectVersions 按模式在 URL 里找版本串(优先文件名, 其次整个路径),
// 返回所有去重后的版本串。
func (p *Prober) detectVersions(rawURL string) []string {
	pathStart := rawURL
	if i := strings.Index(pathStart, "://"); i >= 0 {
		pathStart = pathStart[i+3:]
	}
	if i := strings.IndexByte(pathStart, '/'); i >= 0 {
		pathStart = pathStart[i:]
	}
	if i := strings.IndexByte(pathStart, '?'); i >= 0 {
		pathStart = pathStart[:i]
	}

	var versions []string
	// 文件名优先。
	if i := strings.LastIndexByte(pathStart, '/'); i >= 0 {
		versions = append(versions, p.matchVersions(pathStart[i+1:])...)
	}
	// 文件名里没有, 再找整个路径。
	if len(versions) == 0 {
		versions = append(versions, p.matchVersions(pathStart)...)
	}
	// 去重保序。
	seen := map[string]bool{}
	uniq := versions[:0]
	for _, v := range versions {
		if !seen[v] {
			seen[v] = true
			uniq = append(uniq, v)
		}
	}
	return uniq
}

// matchVersions 按启用模式在 s 里找版本串。
// 同一位置多个模式命中时取优先级高(列表序)的; 过滤日期/长序列等不可枚举值。
func (p *Prober) matchVersions(s string) []string {
	type hit struct {
		start, end int
		v          string
	}

	// 第一轮: 高优先级模式(名称列表序)先占领位置。
	occupied := [][2]int{}
	hits := []hit{}
	for _, m := range p.modes {
		if !m.enabled {
			continue
		}
		for _, loc := range m.re.FindAllStringIndex(s, -1) {
			overlap := false
			for _, o := range occupied {
				if loc[0] < o[1] && loc[1] > o[0] {
					overlap = true
					break
				}
			}
			if overlap {
				continue
			}
			v := s[loc[0]:loc[1]]
			if !isEnumeratable(v) {
				continue
			}
			occupied = append(occupied, [2]int{loc[0], loc[1]})
			hits = append(hits, hit{loc[0], loc[1], v})
		}
	}

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].start != hits[j].start {
			return hits[i].start < hits[j].start
		}
		return hits[i].end > hits[j].end
	})

	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.v
	}
	return out
}

// isEnumeratable 判断识别出的版本串是否可枚举: 过滤纯日期、Build 号、8+ 位数字序列等。
// 以 "." / "-" 分隔的日期形式(如 2025.08.22)最长片段为 4 位, 视为可枚举。
func isEnumeratable(v string) bool {
	if len(v) == 0 || len(v) > 30 {
		return false
	}
	// 最长连续数字段。
	longest := 0
	cur := 0
	for _, r := range v {
		if r >= '0' && r <= '9' {
			cur++
			if cur > longest {
				longest = cur
			}
		} else {
			cur = 0
		}
	}
	if longest >= 8 {
		return false // yyyymmdd 等纯长数字不可枚举
	}
	return true
}

// buildTemplate 把 rawURL 里所有版本串替换为 {v}(多个版本号同时修改)。
func buildTemplate(rawURL string, versions []string) string {
	t := rawURL
	for _, v := range versions {
		t = strings.ReplaceAll(t, v, "{v}")
	}
	return t
}

func parseInts(s string) ([]int, error) {
	comp, _, err := parseIntsW(s)
	return comp, err
}

// parseIntsW 解析版本并返回各组件宽度: 组件以 0 开头(如日期 08)时宽度为实际位数,
// 否则为 0(渲染时按自然位数)。用于保持 2025.08.22 这类前导零格式。
func parseIntsW(s string) ([]int, []int, error) {
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) == 0 || len(parts) > maxVersionComponents {
		return nil, nil, fmt.Errorf(tr("非法版本 %q: 组件数量应在 1-%d 之间", "invalid version %q: 1-%d components expected"), s, maxVersionComponents)
	}
	comp := make([]int, len(parts))
	widths := make([]int, len(parts))
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, nil, fmt.Errorf(tr("非法版本 %q: 存在空组件", "invalid version %q: empty component"), s)
		}
		if strings.HasPrefix(p, "0") && len(p) > 1 {
			widths[i] = len(p)
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > maxVersionValue {
			return nil, nil, fmt.Errorf(tr("非法版本 %q: 组件必须是 0-%d 的数字", "invalid version %q: components must be 0-%d"), s, maxVersionValue)
		}
		comp[i] = n
	}
	return comp, widths, nil
}

func padComps(comp []int, n int) []int {
	out := make([]int, n)
	copy(out, comp)
	return out
}

// trimComps 组件数超过 n 时截断(只保留高 n 位)。
func trimComps(comp []int, n int) []int {
	if len(comp) <= n {
		return comp
	}
	return comp[:n]
}

func leComps(a, b []int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return true
}

func joinComps(comp []int) string {
	ss := make([]string, len(comp))
	for i, n := range comp {
		ss[i] = strconv.Itoa(n)
	}
	return strings.Join(ss, ".")
}

// joinCompsW 按宽度渲染组件: widths[i]>0 时补零到指定位数。
func joinCompsW(comp, widths []int) string {
	ss := make([]string, len(comp))
	for i, n := range comp {
		if i < len(widths) && widths[i] > 0 {
			ss[i] = fmt.Sprintf("%0*d", widths[i], n)
		} else {
			ss[i] = strconv.Itoa(n)
		}
	}
	return strings.Join(ss, ".")
}

// enumerateVersions 按组件组合从小到大枚举 from 到 to 之间的全部版本,
// 按 widths 保持前导零格式。
func enumerateVersions(from, to, widths []int, maxCandidates int) ([]string, error) {
	n := len(to)
	cur := padComps(from, n)
	w := padComps(widths, n)
	out := make([]string, 0, 1024)
	for {
		if len(out) >= maxCandidates {
			return nil, fmt.Errorf(tr("候选空间过大(%d+), 将切换滚动窗口模式", "candidate space too large (%d+); switching to rolling window"), maxCandidates)
		}
		out = append(out, joinCompsW(cur, w))
		i := n - 1
		for i >= 0 {
			cur[i]++
			if cur[i] <= to[i] {
				break
			}
			cur[i] = 0
			i--
		}
		if i < 0 {
			break
		}
	}
	return out, nil
}
