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

// strictMagic 由 -strict 设置: 仅接受已知魔法字节(压缩包/安装包), 拒绝其他非文本。
var strictMagic bool

// probeResult 一次探测的结论。
type probeResult struct {
	found  bool
	size   int64 // 文件大小, -1 未知
	kind   string
	status int
}

func notFoundResult(kind string, size int64, status int) probeResult {
	return probeResult{found: false, size: size, kind: kind, status: status}
}

func foundResult(kind string, size int64, status int) probeResult {
	return probeResult{found: true, size: size, kind: kind, status: status}
}

// headProbe 发 HEAD 请求, 返回状态码与 Content-Length。
func headProbe(hc *http.Client, u, ua string) (int, int64, error) {
	req, err := http.NewRequest(http.MethodHead, u, nil)
	if err != nil {
		return 0, -1, err
	}
	req.Header.Set("User-Agent", ua)
	resp, err := hc.Do(req)
	if err != nil {
		return 0, -1, err
	}
	resp.Body.Close()
	return resp.StatusCode, resp.ContentLength, nil
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
		return "未知(过短)"
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
		return "mach-o 通用"
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
		return "dmg(加密)"
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
		return "二进制"
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// probeURL 完整探测一个 URL 是否可下载:
//  1. HEAD 探测: 404 直接判不存在; 2xx 继续 GET 验证(防假 200);
//  2. HEAD 405/403/429/5xx/网络错误: 回退 GET+Range 只读开头字节判真;
//  3. 最终以 GET 读到的开头字节为准: 非文本(安装包)才算命中。
func probeURL(hc *http.Client, u, ua string) probeResult {
	status, sizeHint, err := headProbe(hc, u, ua)

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
		// HEAD 2xx: 存在或假 200, 用 GET 读开头字节验证。
		return verifyGet(hc, u, ua, sizeHint)
	case status == http.StatusRequestedRangeNotSatisfiable:
		// 416: 资源存在(空文件)。
		return foundResult("416 空文件", 0, status)
	case status >= 400:
		return notFoundResult("HTTP "+fmt.Sprint(status), -1, status)
	default:
		return notFoundResult("状态"+fmt.Sprint(status), -1, status)
	}
}

// verifyGet 发 GET 读开头字节, 判定是否真实安装包。
// sizeHint 是 HEAD 拿到的 Content-Length, 用于空响应时兜底。
func verifyGet(hc *http.Client, u, ua string, sizeHint int64) probeResult {
	status, body, size, ct, err := getFirstBytes(hc, u, ua, magicProbeSize)
	if err != nil {
		return notFoundResult("网络错误", -1, status)
	}
	switch {
	case status == http.StatusRequestedRangeNotSatisfiable:
		// 416: 存在但 Range 不支持(如空文件)。
		if size >= 0 || status == 416 {
			return foundResult("空文件(416)", 0, status)
		}
		return notFoundResult("416", -1, status)
	case status >= 200 && status < 300:
		if len(body) == 0 {
			if size > 0 {
				return foundResult("有大小无内容", size, status)
			}
			if sizeHint > 0 {
				// GET 没读到头但 HEAD 有大小, 视为存在。
				return foundResult("HEAD 有大小", sizeHint, status)
			}
			return notFoundResult("空响应", -1, status)
		}
		if looksLikeTextPage(body, ct) {
			return notFoundResult("文本错误页", size, status)
		}
		kind := classify(body)
		if strictMagic && kind == "二进制" {
			return notFoundResult("未知二进制(严格模式)", size, status)
		}
		return foundResult(kind, size, status)
	default:
		return notFoundResult("HTTP "+fmt.Sprint(status), size, status)
	}
}
