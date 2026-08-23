package main

// ov — 版本号探测工具: 给定一个包含版本的下载地址,
// 自动识别版本号(标准/日期/纯数字/字母混合), 枚举所有版本组合并发探测,
// HEAD 优先, GET 只读开头字节验证, 排除假 200 页面, 输出全部可下载的 URL。
// 高级模式: -platform 从一个 URL 发现其他平台的下载地址。

import (
	"bufio"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tlsutil "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

const defaultUserAgent = "Mozilla/5.0 (compatible; ov-prober/1.0)"

type Prober struct {
	modes []*versionMode
}

// 命令行选项。
type options struct {
	url       string
	from      string
	to        string
	mode      string
	strategy  string
	universe  int
	frontStop int
	stop      int
	conc      int
	timeout   time.Duration
	retry     int
	max       int
	skipTLS   bool
	out       string
	sizes     bool
	strict    bool
	quiet     bool
	ua        string
	minSize   int64
	verify    bool
	platform  bool
	beyond    int
	forceTpl      bool
	reverse       bool
	tlsFingerprint string // TLS 指纹伪装: chrome、firefox、ios 等
}

func parseFlags() *options {
	o := &options{}
	flag.StringVar(&o.url, "url", "", "下载地址(含版本号, 或含 {v} 占位符)")
	flag.StringVar(&o.from, "from", "0.0.0", "起始版本(含)")
	flag.StringVar(&o.to, "to", "", "结束版本(含), 默认取地址中识别的版本或更早的候选")
	flag.StringVar(&o.mode, "mode", "auto", "版本识别模式: auto|std|date|num|alnum 或逗号组合")
	flag.StringVar(&o.strategy, "strategy", "smart", "探测策略: smart(前沿生长, 默认)|exhaust(全量枚举到 -to)")
	flag.IntVar(&o.universe, "universe", 199, "版本分量最大值 0..universe(=200 个取值)")
	flag.IntVar(&o.frontStop, "front-stop", 5, "向上前沿探测中连续未命中多少次即停止")
	flag.IntVar(&o.stop, "stop", 200, "滚动窗口: 连续未发现可下载版本多少次即停止(命中即刷新窗口)")
	flag.IntVar(&o.conc, "c", 50, "并发数")
	flag.DurationVar(&o.timeout, "timeout", 10*time.Second, "单请求超时")
	flag.IntVar(&o.retry, "retry", 1, "网络错误/429/5xx 的额外重试次数")
	flag.IntVar(&o.max, "max", 500000, "总探测请求上限(含历史区与前沿区)")
	flag.BoolVar(&o.skipTLS, "k", false, "跳过 TLS 证书校验")
	flag.StringVar(&o.out, "o", "", "结果写入文件(默认标准输出)")
	flag.BoolVar(&o.sizes, "sizes", false, "输出文件大小(字节)与类型")
	flag.BoolVar(&o.strict, "strict", false, "仅接受已知魔术字节(压缩包/安装包), 拒绝其他非文本")
	flag.BoolVar(&o.quiet, "q", false, "关闭进度输出")
	flag.StringVar(&o.ua, "ua", defaultUserAgent, "User-Agent")
	flag.Int64Var(&o.minSize, "minsize", 0, "最小文件大小(字节), 0 关闭")
	flag.BoolVar(&o.verify, "verify", false, "只验证给定 URL 是否可下载(不做版本枚举)")
	flag.BoolVar(&o.platform, "platform", false, "高级模式: 从给定 URL 生成并探测其他平台变体")
	flag.IntVar(&o.beyond, "beyond", 3, "历史区探测到已知版本之后(更新)的版本数量, 0 表示只到已知版本")
	flag.BoolVar(&o.forceTpl, "force-tpl", false, "跳过不可遍历检查, 强制按模板探测(可用于已含 {v} 的地址)")
	flag.BoolVar(&o.reverse, "reverse", false, "结果按版本语义降序输出(从新到旧), 默认升序(从早到晚)")
	flag.StringVar(&o.tlsFingerprint, "tls-fingerprint", "", "TLS 指纹伪装: chrome、firefox、ios、android 等(默认原生)")
	flag.Parse()
	return o
}

func usage() {
	w := flag.CommandLine.Output()
	fmt.Fprint(w, `ov — 下载地址版本号探测工具

用法:
  ov [选项] <下载地址>
  ov [选项] -url <下载地址>

自动识别地址中的版本号(标准版本/日期/纯数字/字母混合, -mode 配置),
默认 smart 策略: 历史区(0..已知版本)全量枚举 + 前沿区逐维度生长探测
(主/次版本前沿, 连续 -front-stop 次未命中即停; 命中基座再补丁探索),
避免无脑枚举整个 200x200x200 而命中率极低。
探测细节: HEAD 优先; 可疑状态回退 GET+Range; 2xx 再 GET 读开头 2KB,
排除"假 200"文本错误页(HTML/JSON), 按魔法字节/非文本判定真实安装包。
URL 中多个版本号会同时替换; 带签名/随机串的 URL 会提示不可遍历。

示例:
  ov "https://autoglm.aminer.cn/autoclaw/updates/autoclaw-1.17.4-cn.dmg"
  ov -from 1.10.0 -sizes "https://host/app-{v}-setup.dmg"
  ov -mode num "https://app-download.xaminim.com/xingye-android-release/754/xingye.apk"
  ov -platform "https://cdn-zcode.z.ai/zcode/electron/releases/3.8.1/windows-x64/ZCode-3.8.1-win-x64.exe"
  ov -verify "https://kimi-img.moonshot.cn/app/download/mac/kimi_3.2.1.dmg?download_id=xxx"
  ov -tls-fingerprint chrome "https://cdn-zcode.z.ai/zcode/electron/releases/3.8.1/windows-x64/ZCode-3.8.1-win-x64.exe"

选项:
`)
	flag.PrintDefaults()
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "错误: "+format+"\n", a...)
	os.Exit(2)
}

// utlsRoundTripper 对 http.RoundTripper 的 uTLS 实现:
// 替代标准库的 http.Transport, 在 TLS 握手阶段使用指定指纹。
type utlsRoundTripper struct {
	helloID tlsutil.ClientHelloID
	skipTLS bool
}

func (r *utlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Hostname()
	port := req.URL.Port()
	if port == "" {
		if req.URL.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	addr := net.JoinHostPort(host, port)

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}

	tlsConfig := &tlsutil.Config{
		ServerName:         host,
		InsecureSkipVerify: r.skipTLS,
		NextProtos:         []string{"h2", "http/1.1"},
	}
	uconn := tlsutil.UClient(conn, tlsConfig, r.helloID)
	if err := uconn.Handshake(); err != nil {
		uconn.Close()
		return nil, err
	}

	switch uconn.ConnectionState().NegotiatedProtocol {
	case "h2":
		// HTTP/2 over TLS: 用 http2.Transport 包装 uTLS 连接
		tr2 := &http2.Transport{}
		cconn, err := tr2.NewClientConn(uconn)
		if err != nil {
			uconn.Close()
			return nil, err
		}
		return cconn.RoundTrip(req)
	default:
		// HTTP/1.1: 手动写请求再读响应
		if err := req.Write(uconn); err != nil {
			uconn.Close()
			return nil, err
		}
		return http.ReadResponse(bufio.NewReader(uconn), req)
	}
}

// fingerprints 预定义的 TLS 客户端指纹映射。
var fingerprints = map[string]tlsutil.ClientHelloID{
	// Chrome
	"chrome": tlsutil.HelloChrome_Auto, // = HelloChrome_133
	"chrome58":  tlsutil.HelloChrome_58,
	"chrome62":  tlsutil.HelloChrome_62,
	"chrome70":  tlsutil.HelloChrome_70,
	"chrome72":  tlsutil.HelloChrome_72,
	"chrome83":  tlsutil.HelloChrome_83,
	"chrome87":  tlsutil.HelloChrome_87,
	"chrome96":  tlsutil.HelloChrome_96,
	"chrome100": tlsutil.HelloChrome_100,
	"chrome102": tlsutil.HelloChrome_102,
	"chrome106": tlsutil.HelloChrome_106_Shuffle,
	"chrome120": tlsutil.HelloChrome_120,
	"chrome131": tlsutil.HelloChrome_131,
	"chrome133": tlsutil.HelloChrome_133,
	// Firefox
	"firefox": tlsutil.HelloFirefox_Auto, // = HelloFirefox_120
	"firefox55":  tlsutil.HelloFirefox_55,
	"firefox56":  tlsutil.HelloFirefox_56,
	"firefox63":  tlsutil.HelloFirefox_63,
	"firefox65":  tlsutil.HelloFirefox_65,
	"firefox99":  tlsutil.HelloFirefox_99,
	"firefox102": tlsutil.HelloFirefox_102,
	"firefox105": tlsutil.HelloFirefox_105,
	"firefox120": tlsutil.HelloFirefox_120,
	// iOS
	"ios": tlsutil.HelloIOS_Auto, // = HelloIOS_14
	"ios11": tlsutil.HelloIOS_11_1,
	"ios12": tlsutil.HelloIOS_12_1,
	"ios13": tlsutil.HelloIOS_13,
	"ios14": tlsutil.HelloIOS_14,
	// Android
	"android": tlsutil.HelloAndroid_11_OkHttp,
	// Safari / macOS
	"safari": tlsutil.HelloSafari_Auto, // = HelloSafari_16_0
	"safari16": tlsutil.HelloSafari_16_0,
	// Edge
	"edge": tlsutil.HelloEdge_Auto, // = HelloEdge_85
	"edge85":  tlsutil.HelloEdge_85,
	"edge106": tlsutil.HelloEdge_106,
	// 360
	"360": tlsutil.Hello360_Auto, // = Hello360_7_5
	"3607":   tlsutil.Hello360_7_5,
	"360_11": tlsutil.Hello360_11_0,
	// QQ
	"qq": tlsutil.HelloQQ_11_1,
}

// newUTLSRoundTripper 根据指纹名称返回 http.RoundTripper，不支持时返回 nil。
func newUTLSRoundTripper(name string, skipTLS bool) http.RoundTripper {
	if name == "" {
		return nil
	}
	id, ok := fingerprints[name]
	if !ok {
		return nil
	}
	return &utlsRoundTripper{
		helloID: id,
		skipTLS: skipTLS,
	}
}

func main() {
	o := parseFlags()
	if flag.NArg() == 0 && o.url == "" {
		usage()
		os.Exit(2)
	}
	raw := strings.TrimSpace(o.url)
	if raw == "" {
		raw = flag.Arg(0)
	}
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		fatal("地址必须以 http:// 或 https:// 开头: %s", raw)
	}

	p := &Prober{modes: newVersionModes(modeSet(o.mode))}
	strictMagic = o.strict

	var transport http.RoundTripper
	if t := newUTLSRoundTripper(o.tlsFingerprint, o.skipTLS); t != nil {
		transport = t
	} else {
		transport = &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: o.skipTLS}, //nolint:gosec
			MaxIdleConns:        256,
			MaxIdleConnsPerHost: 64,
			IdleConnTimeout:     30 * time.Second,
			DisableCompression:  true,
		}
	}
	hc := &http.Client{
		Timeout:   o.timeout,
		Transport: transport,
	}

	if o.verify {
		runVerify(o, p, hc)
		return
	}

	if o.platform {
		runPlatform(o, hc)
		return
	}

	runEnum(o, p, hc, raw)
}

// modeSet 解析 -mode 值(auto 或逗号组合)。
func modeSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		switch part {
		case "", "auto":
			m["auto"] = true
		case "std", "date", "num", "alnum":
			m[part] = true
		}
	}
	if len(m) == 0 {
		m["auto"] = true
	}
	return m
}

// runVerify 只验证给定 URL 是否可下载, 不做枚举。
func runVerify(o *options, p *Prober, hc *http.Client) {
	urls := flag.Args()
	if len(urls) == 0 {
		urls = []string{o.url}
	}
	for _, u := range urls {
		fmt.Fprintf(os.Stderr, "验证: %s\n", u)
		r := probeURLWithQueryFallback(hc, o, u)
		if r.found {
			fmt.Printf("[命中] %s", u)
			printSizes(o, r)
			fmt.Println()
		} else {
			fmt.Fprintf(os.Stderr, "  不存在: %s (状态 %d)\n", r.kind, r.status)
		}
	}
}

// runPlatform 高级模式: 探测给定 URL 的其他平台变体。
func runPlatform(o *options, hc *http.Client) {
	raw := flag.Arg(0)
	if raw == "" {
		raw = o.url
	}
	// 先验证原 URL 是否有效。
	r := probeURLWithRetry(hc, o, raw)
	if !r.found {
		fmt.Fprintf(os.Stderr, "原 URL 当前不可下载: %s (状态 %d), 仍然尝试平台变体\n", r.kind, r.status)
	}

	variants := platformVariants(raw)
	fmt.Fprintf(os.Stderr, "生成 %d 个平台变体, 探测中...\n", len(variants))
	for _, v := range variants {
		vr := probeURLWithRetry(hc, o, v)
		if vr.found {
			fmt.Printf("[平台] %s", v)
			printSizes(o, vr)
			fmt.Println()
		} else {
			verbosef(o, "  %s -> %s", v, vr.kind)
		}
	}
}

func verbosef(o *options, format string, a ...any) {
	if !o.quiet {
		fmt.Fprintf(os.Stderr, format+"\n", a...)
	}
}

// runEnum 核心: 识别版本 → 生成模板 → 枚举 → 并发探测 → 输出。
func runEnum(o *options, p *Prober, hc *http.Client, raw string) {
	// 不可遍历检查。
	if ok, reason := isTraversable(raw); !ok && !o.forceTpl {
		fatal("该地址不可遍历: %s\n  %s\n  若确认可枚举, 可用 -force-tpl 强制切换为模板模式(需地址含 {v})", raw, reason)
	}

	// 识别版本。
	versions := p.detectVersions(raw)
	if len(versions) == 0 && !o.forceTpl {
		fatal("没有识别到可枚举的版本号(当前模式 %q)。可用 -force-tpl 使用 {v} 模板, 或调整 -mode", o.mode)
	}

	tpl := buildTemplate(raw, versions)
	if !strings.Contains(tpl, "{v}") {
		fatal("模板中没有 {v} 占位符: %s", tpl)
	}

	// 确定探测范围(保持识别版本的位数格式, 如 2025.08.22)。
	toText := o.to
	var toWidths []int
	if toText == "" {
		if len(versions) > 0 {
			last := versions[len(versions)-1] // 文件名优先, 通常是最新版本
			maxComp, widths, err := parseIntsW(last)
			if err == nil {
				// 只抬高末位组件, 探测已知版本之后的 -beyond 个更新版本。
				maxComp[len(maxComp)-1] += o.beyond
				toText = joinCompsW(maxComp, widths)
				toWidths = widths
			}
		}
	}
	if toText == "" {
		fatal("未识别到版本且未指定 -to, 无法确定探测上限(可用 -force-tpl -to x.y.z)")
	}

	from, _, err := parseIntsW(o.from)
	if err != nil {
		fatal("%v", err)
	}
	to, w2, err := parseIntsW(toText)
	if err != nil {
		fatal("%v", err)
	}
	if toWidths == nil {
		toWidths = w2
	}
	n := len(to)
	widths := padComps(toWidths, n)
	from = padComps(trimComps(from, n), n)
	to = padComps(to, n)
	if !leComps(from, to) {
		fatal("-from (%s) 不能大于 -to (%s)", joinComps(from), joinComps(to))
	}

	// smart 策略的锚点: 识别到的最后一个版本(前沿探索以此为下界)。
	anchor := []int(nil)
	if o.strategy == "smart" && len(versions) > 0 {
		if ac, _, e := parseIntsW(versions[len(versions)-1]); e == nil {
			anchor = ac
		}
	}

	candidates, candErr := enumerateVersions(from, to, widths, o.max)

	// 单组件大数值版本(如 072203、73734): 虽然候选空间 < max,
	// 但扫描范围宽(0..大值)太慢, 自动改用滚动窗口以锚点为中心双向探测。
	useRolling := candErr != nil && o.strategy == "smart" && len(anchor) > 0
	if !useRolling && o.strategy == "smart" && len(anchor) == 1 && anchor[0] > o.stop*10 {
		useRolling = true
	}
	if useRolling {
		// 切换为以锚点为中心的滚动窗口探测(命中刷新, 连续 -stop 次未命中停止)。
		runRolling(o, hc, tpl, anchor, widths)
		return
	}
	if candErr != nil {
		fatal("%v", candErr)
	}

	fmt.Fprintf(os.Stderr, "模板: %s\n", tpl)
	if len(versions) > 0 {
		fmt.Fprintf(os.Stderr, "识别到 %d 个版本串: %s\n", len(versions), strings.Join(versions, ", "))
	}
	fmt.Fprintf(os.Stderr, "探测: %s ~ %s, 历史区 %d 个候选, 并发 %d\n",
		joinCompsW(from, widths), joinCompsW(to, widths), len(candidates), o.conc)

	// 并发探测。
	start := time.Now()
	var probed atomic.Int64
	found := make(map[string]probeResult)
	var mu sync.Mutex

	jobs := make(chan string, 256)
	var wg sync.WaitGroup
	for i := 0; i < o.conc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for v := range jobs {
				u := strings.ReplaceAll(tpl, "{v}", v)
				r := probeURLWithQueryFallback(hc, o, u)
				if r.found && (r.size < 0 || r.size >= o.minSize) {
					mu.Lock()
					found[v] = r
					mu.Unlock()
				}
				probed.Add(1)
			}
		}()
	}

	done := make(chan struct{})
	if !o.quiet {
		go func() {
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					fmt.Fprintf(os.Stderr, "\r历史区探测中 ... %d/%d", probed.Load(), len(candidates))
				case <-done:
					return
				}
			}
		}()
	}

	go func() {
		for _, v := range candidates {
			jobs <- v
		}
		close(jobs)
	}()
	wg.Wait()
	close(done)
	if !o.quiet {
		fmt.Fprintln(os.Stderr)
	}

	// smart 策略: 前沿生长, 从锚点向上发现更多版本。
	frontierProbed := int64(0)
	frontierHits := 0
	if o.strategy == "smart" && len(anchor) > 0 {
		seeds := make(map[string]bool, len(found))
		for v := range found {
			seeds[v] = true
		}
		f := newFrontier(hc, o, tpl, widths, seeds, &probed, int64(o.max))
		f.run(anchor)
		for v, r := range f.hits {
			if _, exists := found[v]; !exists {
				found[v] = r
			}
		}
		frontierProbed = probed.Load() - int64(len(candidates))
		frontierHits = len(f.hits)
	}
	_ = frontierHits

	// 输出(按版本数值升序, 历史区+前沿区合并)。
	var out io.Writer = os.Stdout
	if o.out != "" {
		ofile, err := os.Create(o.out)
		if err != nil {
			fatal("无法创建输出文件: %v", err)
		}
		defer ofile.Close()
		out = ofile
	}

	keys := make([]string, 0, len(found))
	for v := range found {
		keys = append(keys, v)
	}
	sortVersionsNatural(keys)
	if o.reverse {
		for i, j := 0, len(keys)-1; i < j; i, j = i+1, j-1 {
			keys[i], keys[j] = keys[j], keys[i]
		}
	}

	count := 0
	for _, v := range keys {
		if r, ok := found[v]; ok {
			count++
			u := strings.ReplaceAll(tpl, "{v}", v)
			fmt.Fprintln(out, u)
			if o.sizes {
				fmt.Fprintf(out, "  -> %s (%s)\n", sizeText(r.size), r.kind)
			}
		}
	}
	if o.strategy == "smart" && len(anchor) > 0 {
		fmt.Fprintf(os.Stderr, "完成: 历史区 %d + 前沿区 %d = %d 个请求, 命中 %d 个可下载地址, 耗时 %s\n",
			len(candidates), frontierProbed, len(candidates)+int(frontierProbed), count, time.Since(start).Round(time.Millisecond))
	} else {
		fmt.Fprintf(os.Stderr, "完成: 探测 %d 个候选, 命中 %d 个可下载地址, 耗时 %s\n",
			len(candidates), count, time.Since(start).Round(time.Millisecond))
	}

	// 高级模式: 从命中的 URL 里再找其他平台。
	if o.platform {
		var urls []string
		for _, v := range keys {
			if r, ok := found[v]; ok {
				urls = append(urls, strings.ReplaceAll(tpl, "{v}", v))
				_ = r
			}
		}
		extra := map[string]bool{}
		for _, u := range urls {
			for _, v := range platformVariants(u) {
				if !extra[v] {
					extra[v] = true
				}
			}
		}
		if len(extra) > 0 {
			fmt.Fprintf(os.Stderr, "平台变体探测: %d 个候选\n", len(extra))
			keys := make([]string, 0, len(extra))
			for k := range extra {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, u := range keys {
				r := probeURLWithRetry(hc, o, u)
				if r.found {
					fmt.Fprintln(out, u)
					if o.sizes {
						fmt.Fprintf(out, "  -> %s (%s)\n", sizeText(r.size), r.kind)
					}
				}
			}
		}
	}
}

func sizeText(size int64) string {
	if size < 0 {
		return "大小未知"
	}
	return fmt.Sprintf("%d 字节", size)
}

func printSizes(o *options, r probeResult) {
	if o.sizes {
		fmt.Printf("\t%s\t%s", sizeText(r.size), r.kind)
	}
}

// probeURLWithRetry 带重试的探测(网络错误/429/5xx 重试)。
func probeURLWithRetry(hc *http.Client, o *options, u string) probeResult {
	var last probeResult
	for attempt := 0; ; attempt++ {
		last = probeURL(hc, u, o.ua)
		sus := last.status >= 500 || last.status == 429 || (last.status == 0 && last.kind == "网络错误")
		if !sus || attempt >= o.retry {
			return last
		}
		time.Sleep(time.Duration(400*(1<<attempt)) * time.Millisecond)
	}
}

// probeURLWithQueryFallback 带查询串兜底的探测:
// 带 ? 参数的 URL 探测失败时, 去掉查询串再试一次(如 kimi 的 ?download_id=)。
func probeURLWithQueryFallback(hc *http.Client, o *options, u string) probeResult {
	r := probeURLWithRetry(hc, o, u)
	if !r.found {
		if i := strings.IndexByte(u, '?'); i > 0 {
			u2 := u[:i]
			if u2 != u {
				r = probeURLWithRetry(hc, o, u2)
			}
		}
	}
	return r
}

// runRolling 滚动窗口模式: 当候选空间过大时, 以锚点为中心,
// 向下扫描(主分量递减) + 向上前沿生长, 均使用滚动窗口命中刷新策略。
func runRolling(o *options, hc *http.Client, tpl string, anchor []int, widths []int) {
	fmt.Fprintf(os.Stderr, "候选空间过大, 切换为滚动窗口模式 (-stop=%d)\n", o.stop)
	fmt.Fprintf(os.Stderr, "锚点: %s\n", joinCompsW(anchor, widths))

	found := make(map[string]probeResult)
	var mu sync.Mutex
	var probed atomic.Int64
	start := time.Now()

	// 向下: 从锚点主分量递减。
	// 注意: scanDown 内部调用 probe -> f.mu.Lock(), 因此这里不加外层锁。
	func() {
		P := len(anchor)
		f := &frontier{hc: hc, o: o, tpl: tpl, widths: widths,
			hits: found, mu: sync.Mutex{}, probed: &probed,
			budget: int64(o.max)}
		f.hi = make([]int, P)
		for i := range f.hi {
			f.hi[i] = o.universe
		}
		// 尾号型单组件(如 072203): anchor 远大于 universe, hi 需以锚点为下界, 否则向上无空间。
		if P == 1 && anchor[0] > o.universe {
			f.hi[0] = anchor[0] + o.stop*10
		}
		f.windowMiss = 0
		f.scanDown(anchor)
	}()

	// 收集向下扫描的命中(临界区, 但 scanDown 已结束, 安全的读)。
	mu.Lock()
	seeds := make(map[string]bool, len(found))
	for v := range found {
		seeds[v] = true
	}
	mu.Unlock()

	// 向上: 前沿生长。
	if len(found) > 0 {
		var p2 atomic.Int64
		f := newFrontier(hc, o, tpl, widths, seeds, &p2, int64(o.max))
		f.run(anchor)
		mu.Lock()
		for v, r := range f.hits {
			if _, exists := found[v]; !exists {
				found[v] = r
			}
		}
		mu.Unlock()
		probed.Add(p2.Load())
	}

	// 输出。
	keys := make([]string, 0, len(found))
	mu.Lock()
	for v := range found {
		keys = append(keys, v)
	}
	mu.Unlock()
	sortVersionsNatural(keys)
	if o.reverse {
		for i, j := 0, len(keys)-1; i < j; i, j = i+1, j-1 {
			keys[i], keys[j] = keys[j], keys[i]
		}
	}

	var out io.Writer = os.Stdout
	if o.out != "" {
		ofile, _ := os.Create(o.out)
		if ofile != nil {
			defer ofile.Close()
			out = ofile
		}
	}

	count := 0
	for _, v := range keys {
		mu.Lock()
		r, ok := found[v]
		mu.Unlock()
		if ok {
			count++
			u := strings.ReplaceAll(tpl, "{v}", v)
			fmt.Fprintln(out, u)
			if o.sizes {
				fmt.Fprintf(out, "  -> %s (%s)\n", sizeText(r.size), r.kind)
			}
		}
	}
	fmt.Fprintf(os.Stderr, "完成: 滚动窗口探测 %d 个请求, 命中 %d 个可下载地址, 耗时 %s\n",
		probed.Load(), count, time.Since(start).Round(time.Millisecond))
}
