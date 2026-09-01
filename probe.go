package main

// 探测模块:
//   - HEAD 优先, 405/网络错误时回退 GET+Range(只读响应头, 不下载正文);
//   - HEAD 2xx 时再发 GET 读开头 2KB 验证: 排除"假 200"错误页(HTML/JSON),
//     判定真实安装包(魔术字节或非文本)。
//   - 416 视为存在(空文件)。

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
)

const magicProbeSize = 2048

// headTrustSize: HEAD 已给出明确二进制 Content-Type 且大小不低于该值时,
// 直接信任 HEAD 结论、省去慢速 GET 魔数校验(真实安装包都是 MB 级, 假 200 文本页很小)。
// -strict 模式下不启用(严格模式必须读到魔数字节)。
const headTrustSize = int64(1 << 20) // 1 MiB

// strictMagic 由 -strict 设置: 仅接受已知魔法字节(压缩包/安装包), 拒绝其他非文本。
var strictMagic bool

// probeResult 一次探测的结论。
type probeResult struct {
	found    bool
	size     int64 // 文件大小, -1 未知
	kind     string
	status   int
	url      string // 实际命中的 URL(路径变体启用时可能与主模板不同)
	verified bool   // true=已确认真实可下载(无需再做 GET 校验); false=疑似, 待校验
}

func notFoundResult(kind string, size int64, status int) probeResult {
	return probeResult{found: false, size: size, kind: kind, status: status}
}

func foundResult(kind string, size int64, status int) probeResult {
	// findResult 的产物都是经实际检查(HEAD 置信或 GET 魔数校验)确认的真实下载,
	// 因此统一标记 verified, 供实时输出层直接采信。
	return probeResult{found: true, verified: true, size: size, kind: kind, status: status}
}

// applyExtraHeaders 把 -H 设置的自定义请求头合并进请求(覆盖默认 UA 等)。
func applyExtraHeaders(req *http.Request) {
	if globalHeaders == nil {
		return
	}
	for k, vs := range globalHeaders {
		req.Header.Del(k)
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
}

// headProbe 发 HEAD 请求, 返回状态码、Content-Length、Content-Type 与 Content-Disposition。
func headProbe(hc *http.Client, u, ua string) (int, int64, string, string, error) {
	req, err := http.NewRequest(http.MethodHead, u, nil)
	if err != nil {
		return 0, -1, "", "", err
	}
	req.Header.Set("User-Agent", ua)
	applyExtraHeaders(req)
	resp, err := hc.Do(req)
	if err != nil {
		return 0, -1, "", "", err
	}
	resp.Body.Close()
	return resp.StatusCode, resp.ContentLength,
		resp.Header.Get("Content-Type"), resp.Header.Get("Content-Disposition"), nil
}

// headTrustOK 报告能否仅凭 HEAD 响应头(状态 200 + 大小 + 类型 + 处置)就确认真实可下载。
// 两个信号任一成立即可:
//  1. Content-Disposition 含 attachment —— 服务器明确告之这是下载文件, 直接信任;
//  2. 体积达到 headTrustSize 且 Content-Type 不像文本错误页(真实安装包都是 MB 级二进制,
//     而"假 200"几乎总是小体积文本页)。
//
// -strict 下不启用(必须读魔数字节)。
func headTrustOK(size int64, ct, disposition string) bool {
	if strictMagic {
		return false
	}
	if strings.Contains(strings.ToLower(disposition), "attachment") {
		return true
	}
	if size < headTrustSize {
		return false
	}
	return !ctLooksTextish(ct)
}

// ctLooksTextish 报告 Content-Type 是否像文本/网页(可能是"假 200"错误页)。
// 返回 true 时不可仅凭 HEAD 信任, 必须 GET 读魔数校验。
// 空 CT 或二进制容器(含 application/octet-stream)不视为文本。
func ctLooksTextish(ct string) bool {
	if ct == "" {
		return false
	}
	ct = strings.ToLower(ct)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if strings.HasPrefix(ct, "text/") {
		return true
	}
	switch ct {
	case "application/json", "application/javascript", "application/xml",
		"xhtml+xml", "application/xhtml+xml", "application/x-www-form-urlencoded",
		"image/svg+xml":
		return true
	}
	return false
}

// getFirstBytes 发 GET + Range 只读开头 n 字节并立即关闭连接。
// 返回状态码、读到的字节、文件总大小(206 解析 Content-Range, 200 取 Content-Length)、Content-Type。
func getFirstBytes(hc *http.Client, u, ua string, n int) (int, []byte, int64, string, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, -1, "", err
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", n-1))
	applyExtraHeaders(req)
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, -1, "", err
	}
	defer resp.Body.Close()

	size := resp.ContentLength
	if resp.StatusCode == http.StatusPartialContent {
		if total := parseContentRangeTotal(resp.Header.Get("Content-Range")); total >= 0 {
			size = total
		}
	}

	body := make([]byte, 0, 512)
	buf := make([]byte, 512)
	remaining := n
	for remaining > 0 {
		read, err := resp.Body.Read(buf)
		if read > 0 {
			chunk := buf[:read]
			if read > remaining {
				chunk = chunk[:remaining]
			}
			body = append(body, chunk...)
			remaining -= len(chunk)
		}
		if err != nil {
			break
		}
	}
	return resp.StatusCode, body, size, resp.Header.Get("Content-Type"), nil
}

// parseContentRangeTotal 从 Content-Range 头解析文件总大小, 失败返回 -1。
func parseContentRangeTotal(cr string) int64 {
	var total int64
	var digits int
	negative := false
	i := strings.LastIndexByte(cr, '/')
	if i < 0 {
		return -1
	}
	for _, r := range cr[i+1:] {
		switch {
		case r >= '0' && r <= '9':
			total = total*10 + int64(r-'0')
			digits++
		case r == ' ' || r == '\t':
			// 忽略
		case r == '-':
			negative = true
		default:
			if digits == 0 {
				return -1
			}
		}
	}
	if digits == 0 {
		return -1
	}
	if negative {
		return -1
	}
	return total
}

// looksLikeTextPage 判断响应开头字节是否像"假 200"错误页(纯文本)。
// 真安装包(二进制)应在此处判 false。
func looksLikeTextPage(body []byte, ct string) bool {
	if len(body) == 0 {
		return false
	}
	ct = strings.ToLower(ct)
	if ct != "" {
		isTextCT := strings.Contains(ct, "text/") || strings.Contains(ct, "application/json") ||
			strings.Contains(ct, "application/javascript") || strings.Contains(ct, "application/xml") ||
			strings.Contains(ct, "application/x-www-form-urlencoded") || strings.Contains(ct, "json")
		if !isTextCT && ct != "application/octet-stream" && ct != "" {
			// 明确的二进制 Content-Type(如 application/zip), 直接视为可信。
			return false
		}
	}
	// 从内容判断: 有控制字符/NUL 即二进制。
	ctrl := 0
	for _, b := range body {
		if b == 0 || (b < 0x20 && b != '\n' && b != '\r' && b != '\t') {
			ctrl++
			if ctrl >= 1 {
				return false
			}
		}
	}
	// 全是可打印 → 文本页。
	return true
}

// classify 根据开头字节判断文件类型描述, 用于展示。
func classify(body []byte) string {
	if len(body) < 4 {
		return tr("未知(过短)", "unknown (too short)")
	}
	has := func(p string) bool { return bytes.HasPrefix(body, []byte(p)) }
	switch {
	case has("PK\x03\x04"):
		return "zip/apk/jar"
	case has("\x7fELF"):
		return "ELF"
	case has("MZ"):
		return "exe/dll(PE)"
	case has("\xca\xfe\xba\xbe"):
		return tr("mach-o 通用", "mach-o universal")
	case has(string([]byte{0xcf, 0xfa, 0xed, 0xfe})):
		return "mach-o arm64"
	case has(string([]byte{0xfe, 0xed, 0xfa, 0xcf})):
		return "mach-o x86_64"
	case has("7z\xbc\xaf\x27\x1c"):
		return "7z"
	case has("Rar!"):
		return "rar"
	case has("%PDF"):
		return "pdf"
	case has(string([]byte{0xd8, 0x41, 0xa9, 0x66})):
		return tr("dmg(加密)", "dmg (encrypted)")
	case has("xar!"):
		return "dmg(xar)"
	case has("\x1f\x8b"):
		return "gzip"
	case has("BZh"):
		return "bzip2"
	case has("\xfd7zXZ"):
		return "xz"
	case bytes.Contains(body[:min(len(body), 512)], []byte("ustar")):
		return "tar"
	case has("!<arch>"):
		return "deb/ar"
	default:
		return tr("二进制", "binary")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// probeURLHeadOnly 只做 HEAD 快速发现, 尽量不做 GET 魔数校验。
// 返回的 found 表示命中; verified=true 表示 HEAD 已足够确认(明确二进制 CT 且体积达标),
// 否则为疑似命中, 需后续 verifyHeadHit 用 GET 校验真伪(过滤"假 200"文本页)。
// 这样发现流水线不被慢速 GET 阻塞; 大多数真实安装包仅凭 HEAD 即可确认。
func probeURLHeadOnly(hc *http.Client, u, ua string) probeResult {
	status, sizeHint, ct, disp, err := headProbe(hc, u, ua)
	if err == nil && status == http.StatusNotFound {
		return notFoundResult("404", -1, status)
	}
	if err != nil ||
		status == http.StatusMethodNotAllowed || status == http.StatusNotImplemented ||
		status == http.StatusForbidden || status == http.StatusTooManyRequests ||
		status >= 500 || status == 0 {
		// HEAD 不可用/状态可疑: 标记为需 GET 判定的疑似命中。
		return probeResult{found: true, size: -1, kind: tr("待校验", "pending"), status: status}
	}
	if status >= 200 && status < 300 {
		// HEAD 响应头已表明是下载文件(attachment 处置 / 二进制+体积达标): 直接确认, 省去慢速 GET。
		if headTrustOK(sizeHint, ct, disp) {
			return probeResult{found: true, verified: true, size: sizeHint, kind: tr("HEAD 确认", "HEAD confirmed"), status: status}
		}
		return probeResult{found: true, size: sizeHint, kind: tr("待校验", "pending"), status: status}
	}
	if status == http.StatusRequestedRangeNotSatisfiable {
		return probeResult{found: true, verified: true, size: 0, kind: tr("416 空文件", "416 empty file"), status: status}
	}
	return notFoundResult("HTTP "+fmt.Sprint(status), -1, status)
}

// verifyHeadHit 对疑似命中(或待判定)的 URL 做 GET+魔数校验, 判定是否真实可下载。
// head 是之前 HEAD 的结果(提供 sizeHint 与状态)。
func verifyHeadHit(hc *http.Client, o *options, u string, head probeResult) probeResult {
	if head.status == http.StatusRequestedRangeNotSatisfiable {
		return head // 416 已是确定结论
	}
	r := verifyGet(hc, u, o.ua, head.size)
	if r.found && head.size > 0 && r.size <= 0 {
		r.size = head.size
	}
	return r
}

// probeURL 完整探测一个 URL 是否可下载:
//  1. HEAD 探测: 404 直接判不存在; 响应头已表明是下载文件(attachment 处置 /
//     二进制+体积达标)则直接确认(省 GET);
//  2. HEAD 405/403/429/5xx/网络错误: 回退 GET+Range 只读开头字节判真;
//  3. 其余 2xx 以 GET 读到的开头字节为准: 非文本(安装包)才算命中(防"假 200")。
func probeURL(hc *http.Client, u, ua string) probeResult {
	status, sizeHint, ct, disp, err := headProbe(hc, u, ua)

	// HEAD 明确 404: 不存在。
	if err == nil && status == http.StatusNotFound {
		return notFoundResult("404", -1, status)
	}

	// HEAD 不可用或状态可疑, 直接 GET 判真。
	if err != nil ||
		status == http.StatusMethodNotAllowed || status == http.StatusNotImplemented ||
		status == http.StatusForbidden || status == http.StatusTooManyRequests ||
		status >= 500 ||
		status == 0 {
		return verifyGet(hc, u, ua, -1)
	}

	switch {
	case status >= 200 && status < 300:
		// HEAD 响应头已表明是下载文件: 直接确认, 省去慢速 GET。
		if headTrustOK(sizeHint, ct, disp) {
			return probeResult{found: true, verified: true, size: sizeHint, kind: tr("HEAD 确认", "HEAD confirmed"), status: status}
		}
		// 其余: 存在或假 200, 用 GET 读开头字节验证。
		return verifyGet(hc, u, ua, sizeHint)
	case status == http.StatusRequestedRangeNotSatisfiable:
		// 416: 资源存在(空文件)。
		return probeResult{found: true, verified: true, size: 0, kind: tr("416 空文件", "416 empty file"), status: status}
	case status >= 400:
		return notFoundResult("HTTP "+fmt.Sprint(status), -1, status)
	default:
		return notFoundResult(tr("状态", "status")+fmt.Sprint(status), -1, status)
	}
}

// verifyGet 发 GET 读开头字节, 判定是否真实安装包。
// sizeHint 是 HEAD 拿到的 Content-Length, 用于空响应时兜底。
func verifyGet(hc *http.Client, u, ua string, sizeHint int64) probeResult {
	status, body, size, ct, err := getFirstBytes(hc, u, ua, magicProbeSize)
	if err != nil {
		return notFoundResult(tr("网络错误", "network error"), -1, status)
	}
	switch {
	case status == http.StatusRequestedRangeNotSatisfiable:
		// 416: 存在但 Range 不支持(如空文件)。
		if size >= 0 || status == 416 {
			return foundResult(tr("空文件(416)", "empty file (416)"), 0, status)
		}
		return notFoundResult("416", -1, status)
	case status >= 200 && status < 300:
		if len(body) == 0 {
			if size > 0 {
				return foundResult(tr("有大小无内容", "has size but no content"), size, status)
			}
			if sizeHint > 0 {
				// GET 没读到头但 HEAD 有大小, 视为存在。
				return foundResult(tr("HEAD 有大小", "HEAD has size"), sizeHint, status)
			}
			return notFoundResult(tr("空响应", "empty response"), -1, status)
		}
		if looksLikeTextPage(body, ct) {
			return notFoundResult(tr("文本错误页", "text error page"), size, status)
		}
		kind := classify(body)
		if strictMagic && kind == "二进制" {
			return notFoundResult(tr("未知二进制(严格模式)", "unknown binary (strict mode)"), size, status)
		}
		return foundResult(kind, size, status)
	default:
		return notFoundResult("HTTP "+fmt.Sprint(status), size, status)
	}
}
