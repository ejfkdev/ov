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
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tlsutil "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

const defaultUserAgent = "Mozilla/5.0 (compatible; ov-prober/1.0)"

// version 构建时注入的版本号(-ldflags "-X main.version=v1.2.3"), 本地构建默认 dev。
var version = "dev"

// repoURL 项目仓库地址。
const repoURL = "https://github.com/ejfkdev/ov"

// 由 -x / -H 设置的全局联网配置, 所有请求构造处统一应用。
var (
	globalProxyURL *url.URL
	globalHeaders  http.Header
)

// interactive 是否交互式运行: 标准输出与标准错误都是终端时为 true。
// 非交互(被脚本/管道/其他程序调用)时: stdout 只流式输出发现的 URL(一行一个,
// 确认一个立即打印一个); 进度/模板/完成等辅助信息只在交互式下打往 stderr。
var interactive bool

// debugFake200: OV_DEBUG_FAKE=1 时打印被校验判为"假 200"而回退的疑似命中,
// 便于观察 CDN 假 200 的分布; 默认关闭。
var debugFake200 = os.Getenv("OV_DEBUG_FAKE") != ""

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// 非交互式实时输出的去重集合(进程内单任务)。
var (
	streamedMu sync.Mutex
	streamed   = map[string]bool{}
)

// emitURL 非交互模式下实时打印一个确认可下载的 URL(一行一个, 按 URL 去重)。
// 交互模式下不做任何事(结果在最后统一排序输出)。
func emitURL(u string) {
	if interactive || u == "" {
		return
	}
	streamedMu.Lock()
	defer streamedMu.Unlock()
	if streamed[u] {
		return
	}
	streamed[u] = true
	fmt.Println(u)
}

type Prober struct {
	modes []*versionMode
}

// 命令行选项。
type options struct {
	// 内置固定配置(不再暴露为命令行选项)。
	from      string
	mode      string
	strategy  string
	universe  int
	frontStop int
	stop      int
	retry     int
	max       int
	beyond    int
	ua        string

	conc           int
	timeout        time.Duration
	skipTLS        bool
	sizes          bool
	platform       bool
	forceTpl       bool
	reverse        bool
	tlsFingerprint string   // TLS 指纹伪装: chrome、firefox、ios 等
	pathVariants   bool     // 主形态 miss 时试探通用路径变体(应对发布目录改版/改名)
	proxy          string   // HTTP(S)/SOCKS5 代理 (curl 风格 -x)
	headers        []string // 自定义请求头 (curl 风格 -H, 可重复)
}

func parseFlags() *options {
	flag.Usage = usage
	o := &options{}
	showVersion := flag.Bool("version", false, "显示版本号")
	flag.BoolVar(showVersion, "V", false, "显示版本号")
	// 版本范围/识别/策略/校验等相关参数已内置为最优配置, 不再暴露为命令行选项。
	o.from = "0.0.0"
	o.mode = "auto"
	o.strategy = "smart"
	o.universe = 199
	o.frontStop = 5
	o.stop = 200
	o.retry = 1
	o.max = 500000
	o.ua = defaultUserAgent
	o.beyond = 3

	flag.IntVar(&o.conc, "c", 50, "并发探测数")
	flag.DurationVar(&o.timeout, "timeout", 10*time.Second, "单请求超时")
	flag.BoolVar(&o.skipTLS, "k", false, "跳过 TLS 证书校验")
	flag.StringVar(&o.proxy, "x", "", "代理 (curl 风格): http://host:port 或 socks5://host:port")
	flag.Var((*headerFlag)(&o.headers), "H", "自定义请求头 (curl 风格, 可重复): \"Name: Value\"")
	flag.BoolVar(&o.sizes, "sizes", false, "输出文件大小(字节)与类型")
	flag.BoolVar(&o.reverse, "reverse", false, "结果按新到旧输出")
	flag.BoolVar(&o.platform, "platform", false, "从给定 URL 探测其他平台/架构变体")
	flag.BoolVar(&o.pathVariants, "path-variants", false, "主路径 404 时回退通用路径变体(去子目录/换架构名)")
	flag.StringVar(&o.tlsFingerprint, "tls-fingerprint", "", "TLS 指纹伪装: chrome、firefox、ios、android 等")
	flag.BoolVar(&o.forceTpl, "force-tpl", false, "强制使用 {v} 模板探测(绕过不可遍历检查)")
	flag.Parse()
	if *showVersion {
		fmt.Printf("ov v%s\n%s\n", version, repoURL)
		os.Exit(0)
	}
	return o
}

// headerFlag 收集可重复的 -H 参数。
type headerFlag []string

func (h *headerFlag) String() string { return strings.Join(*h, "; ") }
func (h *headerFlag) Set(v string) error {
	*h = append(*h, v)
	return nil
}

func usage() {
	fmt.Fprintf(flag.CommandLine.Output(), t("usage"), version, repoURL)
}

// prerr 仅交互式模式下打印辅助信息到 stderr(进度/说明); 非交互时静默。
func prerr(format string, a ...any) {
	if !interactive {
		return
	}
	fmt.Fprintf(os.Stderr, format, a...)
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, t("fatalPrefix")+format+"\n", a...)
	os.Exit(2)
}

// utlsRoundTripper 对 http.RoundTripper 的 uTLS 实现:
// 替代标准库的 http.Transport, 在 TLS 握手阶段使用指定指纹。
type utlsRoundTripper struct {
	helloID   tlsutil.ClientHelloID
	skipTLS   bool
	proxyDial proxy.Dialer // 非 nil 时经 HTTP CONNECT/SOCKS5 代理建立隧道
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

	var conn net.Conn
	var err error
	if r.proxyDial != nil {
		conn, err = r.proxyDial.Dial("tcp", addr)
	} else {
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		conn, err = dialer.Dial("tcp", addr)
	}
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
		// HTTP/2 over TLS: 用 http2.Transport 包装 uTLS 连接。
		tr2 := &http2.Transport{}
		cconn, err := tr2.NewClientConn(uconn)
		if err != nil {
			uconn.Close()
			return nil, err
		}
		return cconn.RoundTrip(req)
	default:
		// HTTP/1.1: 手动写请求再读响应。
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
	"chrome":    tlsutil.HelloChrome_Auto, // = HelloChrome_133
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
	"firefox":    tlsutil.HelloFirefox_Auto, // = HelloFirefox_120
	"firefox55":  tlsutil.HelloFirefox_55,
	"firefox56":  tlsutil.HelloFirefox_56,
	"firefox63":  tlsutil.HelloFirefox_63,
	"firefox65":  tlsutil.HelloFirefox_65,
	"firefox99":  tlsutil.HelloFirefox_99,
	"firefox102": tlsutil.HelloFirefox_102,
	"firefox105": tlsutil.HelloFirefox_105,
	"firefox120": tlsutil.HelloFirefox_120,
	// iOS
	"ios":   tlsutil.HelloIOS_Auto, // = HelloIOS_14
	"ios11": tlsutil.HelloIOS_11_1,
	"ios12": tlsutil.HelloIOS_12_1,
	"ios13": tlsutil.HelloIOS_13,
	"ios14": tlsutil.HelloIOS_14,
	// Android
	"android": tlsutil.HelloAndroid_11_OkHttp,
	// Safari / macOS
	"safari":   tlsutil.HelloSafari_Auto, // = HelloSafari_16_0
	"safari16": tlsutil.HelloSafari_16_0,
	// Edge
	"edge":    tlsutil.HelloEdge_Auto, // = HelloEdge_85
	"edge85":  tlsutil.HelloEdge_85,
	"edge106": tlsutil.HelloEdge_106,
	// 360
	"360":    tlsutil.Hello360_Auto, // = Hello360_7_5
	"3607":   tlsutil.Hello360_7_5,
	"360_11": tlsutil.Hello360_11_0,
	// QQ
	"qq": tlsutil.HelloQQ_11_1,
}

// newUTLSRoundTripper 根据指纹名称返回 http.RoundTripper，不支持时返回 nil。
func newUTLSRoundTripper(name string, skipTLS bool, proxyDial proxy.Dialer) http.RoundTripper {
	if name == "" {
		return nil
	}
	id, ok := fingerprints[name]
	if !ok {
		return nil
	}
	return &utlsRoundTripper{
		helloID:   id,
		skipTLS:   skipTLS,
		proxyDial: proxyDial,
	}
}

func main() {
	o := parseFlags()
	interactive = isTerminal(os.Stdout) && isTerminal(os.Stderr)
	if flag.NArg() == 0 {
		usage()
		os.Exit(2)
	}
	raw := strings.TrimSpace(flag.Arg(0))
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		fatal(t("urlMustStartWithHttpOrHttps"), raw)
	}

	p := &Prober{modes: newVersionModes(modeSet(o.mode))}

	// -x 代理与 -H 自定义请求头(curl 风格)。
	var proxyFunc func(*http.Request) (*url.URL, error)
	var proxyDial proxy.Dialer
	if o.proxy != "" {
		u, err := url.Parse(o.proxy)
		if err != nil || u.Host == "" {
			fatal(t("invalidProxyUrl"), o.proxy)
		}
		globalProxyURL = u
		proxyFunc = http.ProxyURL(u)
		d, err := newProxyDialer(o.proxy)
		if err != nil {
			fatal(t("unsupportedProxy"), o.proxy)
		}
		proxyDial = d
	}
	for _, hs := range o.headers {
		k, v, ok := strings.Cut(hs, ":")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			fatal(t("invalidHeaderExpectedNameValue"), hs)
		}
		if globalHeaders == nil {
			globalHeaders = http.Header{}
		}
		globalHeaders.Add(k, strings.TrimSpace(v))
	}

	var transport http.RoundTripper
	if t := newUTLSRoundTripper(o.tlsFingerprint, o.skipTLS, proxyDial); t != nil {
		transport = t
	} else {
		proxyEnv := http.ProxyFromEnvironment
		if proxyFunc != nil {
			proxyEnv = proxyFunc
		}
		transport = &http.Transport{
			Proxy:               proxyEnv,
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
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// HEAD/GET 探测跟随 301/302(及 303/307/308), 最多 5 次;
			// 超限/循环返回 ErrUseLastResponse, 调用方按不存在处理。
			if len(via) >= 5 {
				return http.ErrUseLastResponse
			}
			return nil
		},
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

// runPlatform 高级模式: 探测给定 URL 的其他平台变体。
func runPlatform(o *options, hc *http.Client) {
	raw := flag.Arg(0)
	// 先验证原 URL 是否有效。
	r := probeURLWithRetry(hc, o, raw)
	if !r.found {
		prerr(t("originalUrlNotDownloadable"), r.kind, r.status)
	}

	variants := platformVariants(raw)
	prerr(t("generatingPlatformVariants"), len(variants))
	for _, v := range variants {
		vr := probeURLWithRetry(hc, o, v)
		if vr.found {
			emitURL(v) // 非交互: 直接流式输出 URL(交互模式则由下方打印带前缀的行)
			if interactive {
				fmt.Printf("%s%s", t("platform"), v)
				printSizes(o, vr)
				fmt.Println()
			}
		} else {
			prerr("  %s -> %s\n", v, vr.kind)
		}
	}
}

func verbosef(o *options, format string, a ...any) {
	prerr(format+"\n", a...)
}

// runEnum 核心: 识别版本 → 生成模板 → 枚举 → 并发探测 → 输出。
func runEnum(o *options, p *Prober, hc *http.Client, raw string) {
	// 不可遍历检查。
	if ok, reason := isTraversable(raw); !ok && !o.forceTpl {
		fatal(t("urlNotTraversable"), raw, reason)
	}

	// 识别版本。
	versions := p.detectVersions(raw)
	if len(versions) == 0 && !o.forceTpl {
		fatal(t("noEnumerableVersion"))
	}

	tpl := buildTemplate(raw, versions)
	if !strings.Contains(tpl, "{v}") {
		fatal(t("templateNoPlaceholder"), tpl)
	}

	// 确定探测范围: 以地址中识别的版本为上限再放宽几个补丁(保持位数格式, 如 2025.08.22)。
	toText := ""
	var toWidths []int
	if len(versions) > 0 {
		last := versions[len(versions)-1] // 文件名优先, 通常是最新版本
		maxComp, widths, err := parseIntsW(last)
		if err == nil {
			// 只抬高末位组件, 探测已知版本之后的几个更新版本;
			// 更高主/副版本由前沿生长与广撒网负责, 无需预先限定。
			maxComp[len(maxComp)-1] += o.beyond
			toText = joinCompsW(maxComp, widths)
			toWidths = widths
		}
	}
	if toText == "" {
		fatal(t("noVersionInUrl"))
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
		fatal(t("invalidRange"), joinComps(from), joinComps(to))
	}

	// smart 策略的锚点: 识别到的最后一个版本(前沿探索以此为下界)。
	anchor := []int(nil)
	if o.strategy == "smart" && len(versions) > 0 {
		if ac, _, e := parseIntsW(versions[len(versions)-1]); e == nil {
			anchor = ac
		}
	}

	candidates, candErr := enumerateVersions(from, to, widths, o.max)

	// 大数值主版本/尾号(如 2026.2.1 年份式、072203): 虽然候选空间可能 < max,
	// 但从 0 枚举到如此大的值既慢又无意义, 自动改用滚动窗口以锚点为中心双向探测。
	useRolling := candErr != nil && o.strategy == "smart" && len(anchor) > 0
	if !useRolling && o.strategy == "smart" && len(anchor) > 0 && anchor[0] > o.stop*10 {
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

	prerr(t("template"), tpl)
	if len(versions) > 0 {
		prerr(t("detectedVersions"), len(versions), strings.Join(versions, ", "))
	}
	// 范围为动态(历史区 + 广撒网 + 前沿扩展), 不预先固定终点。
	prerr(t("probingConcurrency"), o.conc)

	// 并发探测(两段式: 发现 + 校验):
	//  - 发现 worker 只做 HEAD 快探, 命中疑似即立刻把校验任务丢进独立的校验队列,
	//    随后继续取下一个候选 —— 慢的 GET 魔数校验不会阻塞发现流水线;
	//  - 校验 worker 池独立并发地做 GET+魔数判定(过滤"假 200"文本页), 一有疑似命中
	//    就立即开始校验, 不必等整批发现完成。
	// 进度显示已发现(疑似命中)数; 最终输出前等校验全部落定。
	start := time.Now()
	var probed atomic.Int64
	var hitsFound atomic.Int64
	found := make(map[string]probeResult)
	var mu sync.Mutex
	var progMu sync.Mutex
	report := func() {
		progMu.Lock()
		prerr(t("probingRequestsHits"), probed.Load(), hitsFound.Load())
		progMu.Unlock()
	}

	type verifyJob struct {
		v    string
		url  string
		head probeResult
	}
	// 历史区主版本预扫: 先逐主版本试几个代表入口, 空主版本(如布局改版后
	// 整段废弃的旧主版本)整段跳过稠密枚举 —— 高延迟网络上省下大量注定 404
	// 的请求; 命中的代表入口仍会在随后的稠密枚举中再探一次, 不引入重复发现。
	if o.strategy == "smart" && len(anchor) > 0 && anchor[0] > from[0] {
		liveMajors := prescanOldMajors(hc, o, tpl, widths, from[0], anchor[0], &probed)
		filtered := make([]string, 0, len(candidates))
		for _, v := range candidates {
			comp := parseSimple(v)
			if len(comp) >= 1 && liveMajors[comp[0]] {
				filtered = append(filtered, v)
			}
		}
		prerr(t("majorPrescanSummary"),
			len(liveMajors), len(candidates), len(filtered))
		candidates = filtered
	}
	verifyJobs := make(chan verifyJob, 256)
	var vwg sync.WaitGroup
	for i := 0; i < o.conc; i++ {
		vwg.Add(1)
		go func() {
			defer vwg.Done()
			for job := range verifyJobs {
				r := verifyHeadHit(hc, o, job.url, job.head)
				mu.Lock()
				if usableHit(o, r) {
					// 校验通过: 用确认结果(真实 size/kind)覆盖疑似占位。
					r.url = job.url
					found[job.v] = r
					mu.Unlock()
					emitURL(job.url) // 非交互: 确认一个立即输出一个
				} else if _, exists := found[job.v]; exists {
					// 校验判定为"假 200"(文本错误页等): 撤销疑似命中。
					delete(found, job.v)
					hitsFound.Add(-1)
					mu.Unlock()
					if debugFake200 {
						// 调试输出绕过 interactive 门控(prerr 仅在终端打印), 管道下也可见。
						fmt.Fprintf(os.Stderr, t("fake200Http"), job.url, r.kind, r.status)
					}
				} else {
					mu.Unlock()
				}
			}
		}()
	}

	jobs := make(chan string, o.conc*4)
	var wg sync.WaitGroup
	for i := 0; i < o.conc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for v := range jobs {
				r := probeTemplateDiscover(hc, o, tpl, v)
				probed.Add(1)
				if r.found {
					isNew := false
					mu.Lock()
					if _, exists := found[v]; !exists {
						found[v] = r // 先按疑似命中占位
						isNew = true
					}
					mu.Unlock()
					if isNew {
						hitsFound.Add(1)
						if r.verified {
							// HEAD 已确认: 无需 GET 校验, 直接实时输出。
							emitURL(r.url)
						} else {
							verifyJobs <- verifyJob{v: v, url: r.url, head: r}
						}
					}
				}
				report()
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
	close(verifyJobs)
	vwg.Wait() // 等所有魔数校验完成, found 里只剩真实可下载的版本

	// smart 策略: 前沿生长, 从锚点向上发现更多版本。
	// 进度行保持原地刷新, 不为前后两个阶段断开成两行。
	if o.strategy == "smart" && len(anchor) > 0 {
		seeds := make(map[string]bool, len(found))
		mu.Lock()
		for v := range found {
			seeds[v] = true
		}
		mu.Unlock()
		f := newFrontier(hc, o, tpl, widths, seeds, &probed, &hitsFound, int64(o.max))
		f.run(anchor)
		mu.Lock()
		for v, r := range f.hits {
			if _, exists := found[v]; !exists {
				found[v] = r
			}
		}
		mu.Unlock()
	}

	// 输出(按版本数值升序, 历史区+前沿区合并)。
	out := os.Stdout

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
	if interactive {
		prerr("\n") // 结束进度行, 让结果 URL 从新行开始
		// 交互模式: 最后统一排序输出(非交互模式已在确认时按 emitURL 实时单行输出)。
		for _, v := range keys {
			count++
			fmt.Fprintln(out, resolvedURL(tpl, v, found[v]))
			if o.sizes {
				fmt.Fprintf(out, "  -> %s (%s)\n", sizeText(found[v].size), found[v].kind)
			}
		}
	}
	prerr(t("doneRequestsHitsTook"),
		probed.Load(), count, time.Since(start).Round(time.Millisecond))

	// 高级模式: 从命中的 URL 里再找其他平台。
	if o.platform {
		var urls []string
		for _, v := range keys {
			urls = append(urls, strings.ReplaceAll(tpl, "{v}", v))
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
			prerr(t("platformVariantsCandidates"), len(extra))
			keys := make([]string, 0, len(extra))
			for k := range extra {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, u := range keys {
				r := probeURLWithRetry(hc, o, u)
				if r.found {
					emitURL(u)
					if interactive {
						fmt.Fprintln(out, u)
						if o.sizes {
							fmt.Fprintf(out, "  -> %s (%s)\n", sizeText(r.size), r.kind)
						}
					}
				}
			}
		}
	}
}

func sizeText(size int64) string {
	if size < 0 {
		return t("unknownSize")
	}
	return fmt.Sprintf(t("bytes"), size)
}

func printSizes(o *options, r probeResult) {
	if o.sizes {
		fmt.Printf("\t%s\t%s", sizeText(r.size), r.kind)
	}
}

// usableHit 报告探测结果是否算"可下载命中"。
func usableHit(o *options, r probeResult) bool {
	return r.found
}

// resolvedURL 返回某版本实际命中的 URL: 有路径变体时用变体 URL, 否则按主模板渲染。
func resolvedURL(tpl, v string, r probeResult) string {
	if r.url != "" {
		return r.url
	}
	return strings.ReplaceAll(tpl, "{v}", v)
}

// probeTemplate 对一个版本按主模板探测 URL; 若开启 -path-variants 且主形态未命中,
// 再低成本试探通用路径变体(平台/架构 token 替换、去掉一层发布子目录),
// 以应对发布目录结构随版本演进变化(旧版无子目录/不同架构名)。返回含实际命中 URL 的结果。
func probeTemplate(hc *http.Client, o *options, tpl, v string) probeResult {
	u := strings.ReplaceAll(tpl, "{v}", v)
	r := probeURLWithQueryFallback(hc, o, u)
	r.url = u
	if usableHit(o, r) || !o.pathVariants {
		return r
	}
	for _, pv := range pathVariants(u)[1:] { // [0] 即主形态, 已探过
		rv := probeURLWithQueryFallback(hc, o, pv)
		if usableHit(o, rv) {
			rv.url = pv
			return rv
		}
	}
	return r
}

// probeHeadRetry 仅 HEAD 的发现探测, 对瞬时错误(网络/429/5xx)重试。
// 返回 found 表示"疑似命中, 需后续 GET 校验"。
func probeHeadRetry(hc *http.Client, o *options, u string) probeResult {
	var last probeResult
	for attempt := 0; ; attempt++ {
		last = probeURLHeadOnly(hc, u, o.ua)
		sus := last.status >= 500 || last.status == 429 || (last.status == 0)
		if !sus || attempt >= o.retry {
			return last
		}
		time.Sleep(time.Duration(400*(1<<attempt)) * time.Millisecond)
	}
}

// probeHeadQueryFallback 仅 HEAD 的发现探测, 带查询串兜底。
func probeHeadQueryFallback(hc *http.Client, o *options, u string) probeResult {
	r := probeHeadRetry(hc, o, u)
	if !r.found {
		if i := strings.IndexByte(u, '?'); i > 0 {
			u2 := u[:i]
			if u2 != u {
				r = probeHeadRetry(hc, o, u2)
			}
		}
	}
	return r
}

// probeTemplateDiscover 仅做 HEAD 发现(含路径变体试探), 不做 GET 魔数校验。
// 返回疑似命中(待校验)的结果, 其 url 为命中的具体形态; 真伪由校验队列判定。
func probeTemplateDiscover(hc *http.Client, o *options, tpl, v string) probeResult {
	u := strings.ReplaceAll(tpl, "{v}", v)
	r := probeHeadQueryFallback(hc, o, u)
	r.url = u
	if r.found || !o.pathVariants {
		return r
	}
	for _, pv := range pathVariants(u)[1:] {
		rv := probeHeadQueryFallback(hc, o, pv)
		if rv.found {
			rv.url = pv
			return rv
		}
	}
	return r
}

// majorPrescanProbe 预扫主版本 M 是否存在: 并发试 (M, 0..4, 0) 代表入口,
// 任一命中即存在(早停)。真实产品每个主版本的首发几乎总在 minor 0..4。
func majorPrescanProbe(hc *http.Client, o *options, tpl string, widths []int, M int) bool {
	P := len(widths)
	if P < 2 {
		P = 2
	}
	res := make(chan bool, 5)
	launched := 0
	for m := 0; m <= 4; m++ {
		comp := make([]int, P)
		comp[0], comp[1] = M, m
		v := joinCompsW(comp, widths)
		launched++
		go func(v string) {
			res <- probeTemplateDiscover(hc, o, tpl, v).found
		}(v)
	}
	for i := 0; i < launched; i++ {
		if <-res {
			return true
		}
	}
	return false
}

// prescanOldMajors 对 [fromMajor, anchorMajor) 的每个主版本做代表入口预扫,
// 返回存在的主版本集合(恒含锚点主版本)。空主版本(布局改版后整段废弃的旧主版本)
// 的稠密枚举被整段跳过——高延迟网络上省下大量注定 404 的请求。
// 预扫失败方是开放的: 未及预扫的剩余主版本照常枚举, 不会漏发现。
func prescanOldMajors(hc *http.Client, o *options, tpl string, widths []int,
	fromMajor, anchorMajor int, probed *atomic.Int64) map[int]bool {
	live := map[int]bool{anchorMajor: true}
	var mu sync.Mutex
	sem := make(chan struct{}, o.conc)
	var wg sync.WaitGroup
	// 预扫上限: 超过 48 个旧主版本的超大区间直接交给稠密枚举(防预扫本身失控)。
	if anchorMajor-fromMajor > 48 {
		fromMajor = anchorMajor - 48
	}
	for M := fromMajor; M < anchorMajor; M++ {
		probed.Add(1)
		sem <- struct{}{}
		wg.Add(1)
		go func(M int) {
			defer wg.Done()
			defer func() { <-sem }()
			if majorPrescanProbe(hc, o, tpl, widths, M) {
				mu.Lock()
				live[M] = true
				mu.Unlock()
			}
		}(M)
	}
	wg.Wait()
	return live
}

// probeURLWithRetry 带重试的探测(网络错误/429/5xx 重试)。
func probeURLWithRetry(hc *http.Client, o *options, u string) probeResult {
	var last probeResult
	for attempt := 0; ; attempt++ {
		last = probeURL(hc, u, o.ua)
		sus := last.status >= 500 || last.status == 429 || (last.status == 0)
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
	prerr(t("switchingToRollingWindow"), o.stop)
	prerr(t("anchor"), joinCompsW(anchor, widths))

	found := make(map[string]probeResult)
	var mu sync.Mutex
	var probed atomic.Int64
	var hitsFound atomic.Int64
	start := time.Now()

	// 向下: 从锚点主分量递减。
	func() {
		f := newFrontier(hc, o, tpl, widths, map[string]bool{}, &probed, &hitsFound, int64(o.max))
		f.hi = computeHi(anchor, o)
		f.scanDown(anchor)
		// 合并 scanDown 命中到 found。
		mu.Lock()
		for v, r := range f.hits {
			if _, exists := found[v]; !exists {
				found[v] = r
			}
		}
		mu.Unlock()
	}()

	// 收集向下扫描的命中(作为前沿生长播种)。
	mu.Lock()
	seeds := make(map[string]bool, len(found))
	for v := range found {
		seeds[v] = true
	}
	mu.Unlock()

	// 向上: 前沿生长。
	if len(found) > 0 {
		var p2 atomic.Int64
		f := newFrontier(hc, o, tpl, widths, seeds, &p2, &hitsFound, int64(o.max))
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

	out := os.Stdout

	count := 0
	if interactive {
		prerr("\n") // 结束进度行, 让结果 URL 从新行开始
		for _, v := range keys {
			mu.Lock()
			r, ok := found[v]
			mu.Unlock()
			if ok {
				count++
				fmt.Fprintln(out, resolvedURL(tpl, v, r))
				if o.sizes {
					fmt.Fprintf(out, "  -> %s (%s)\n", sizeText(r.size), r.kind)
				}
			}
		}
	}
	prerr(t("doneRequestsHitsTook"),
		probed.Load(), count, time.Since(start).Round(time.Millisecond))
}
