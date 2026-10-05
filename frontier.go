package main

// "前沿生长"探测策略(smart): 在历史区(0..已知版本)的全量枚举之外,
// 向上做逐维度的前沿扫描来发现更多存在的版本, 而不是盲目枚举整个 99x99x99。
//
// 做法:
//  1. 主版本前沿: 从 anchor 主版本+1 起逐主版本探测 (M,0,0..),
//     连续 -front-stop 次未命中即停止;
//  2. 副版本前沿: 对每个已知/发现的主版本, 逐副版本探测 (M,m,0..), 同样连停即止;
//  3. 补丁填充: 对锚点所在基座和前沿发现的 (主,副) 基座,
//     从已知补丁+1 起线性探测补丁, 连续停止(补丁段通常密集, 命中率高)。

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// frontier 前沿探测器。
type frontier struct {
	hc        *http.Client
	o         *options
	tpl       string
	ua        string
	widths    []int           // 组件宽度(保持 2025.08.22 式前导零)
	hi        []int           // 每维度上界(universe 或显式 -to)
	anchor    []int           // 锚点版本(run() 设置): 用于判定"旧主版本"以放宽后向扫描
	seeds     map[string]bool // 历史命中版本(播种已知主/次/补丁)
	hits      map[string]probeResult
	probed    *atomic.Int64
	hitsFound *atomic.Int64 // 全程命中计数(用于实时进度)
	budget    int64
	aborted   atomic.Bool // 预算是否耗尽(并发探测下用原子标志)
	done      atomic.Bool // run() 已结束: 收尾在途探测不再刷新进度行, 避免污染结果输出
	mu        sync.Mutex
	progMu    sync.Mutex    // 进度行输出的串行化
	sem       chan struct{} // 全局在途请求上限(与 -c 一致), 防嵌套并发把并发度叠到数百
}

func newFrontier(hc *http.Client, o *options, tpl string, widths []int,
	seeds map[string]bool, probed *atomic.Int64, hitsFound *atomic.Int64, budget int64) *frontier {
	n := o.conc
	if n < 1 {
		n = 1
	}
	return &frontier{
		hc: hc, o: o, tpl: tpl, ua: o.ua, widths: widths,
		seeds: seeds, hits: map[string]probeResult{},
		probed: probed, hitsFound: hitsFound, budget: budget,
		sem: make(chan struct{}, n),
	}
}

// reportProbe 刷新实时进度行(每次探测完成都刷新, 避免前沿阶段看似卡住)。
func (f *frontier) reportProbe() {
	if f.done.Load() || f.probed == nil || f.hitsFound == nil {
		return
	}
	f.progMu.Lock()
	prerr(t("probingRequestsHits"), f.probed.Load(), f.hitsFound.Load())
	f.progMu.Unlock()
}

// reportHit 在并发扫描命中新基座时计入命中并刷新实时进度。
func (f *frontier) reportHit() {
	if f.hitsFound != nil {
		f.hitsFound.Add(1)
	}
	f.reportProbe()
}

// probeParallel 流水线并发探测: gen 按序产出候选, 维持最多 conc 个在途请求,
// 任一完成立即补充下一个(而非等一整批)。严格按消费顺序做连续 miss 截断,
// 语义与串行 scanDim 一致: 命中刷新 miss 计数, 连续 stop 个 miss 即停止消费。
// 返回按发射顺序记录的"是否命中"。stop<=0 表示不按 miss 截断(gen 自行终止)。
func (f *frontier) probeParallel(gen func() ([]int, bool), stop int) []bool {
	limit := f.o.conc
	if limit < 1 {
		limit = 1
	}
	// 有连空截断时预取窗口只需略超前于 stop(超出部分的探测大概率也是 miss,
	// 大窗口只是平白多打请求); 无截断(stop<=0)时保持 -c 全并发。
	lookahead := limit
	if stop > 0 && stop*2 < lookahead {
		lookahead = stop * 2
	}
	type future struct{ c chan probeOutcome }
	launch := func(comp []int) *future {
		c := make(chan probeOutcome, 1)
		go func() {
			hit, _ := f.probe(comp)
			c <- probeOutcome{hit: hit}
		}()
		return &future{c: c}
	}

	var window []*future
	genDone := false
	miss := 0
	var out []bool
	for {
		// 补充在途窗口至 lookahead, 但不超过 miss 边界。
		for !genDone && !f.isAborted() && len(window) < lookahead && (stop <= 0 || miss < stop) {
			comp, ok := gen()
			if !ok {
				genDone = true
				break
			}
			window = append(window, launch(comp))
		}
		if len(window) == 0 {
			break
		}
		o := <-window[0].c
		window = window[1:]
		out = append(out, o.hit)
		if o.hit {
			miss = 0
		} else if stop > 0 {
			miss++
			if miss >= stop {
				// 触发截断: 已发射的在途探测自然完成, 把结果也收集进来,
				// 以免错过截断点之后恰好命中的版本(并发预取会提前发射)。
				for _, w := range window {
					oo := <-w.c
					out = append(out, oo.hit)
				}
				window = nil
				genDone = true
			}
		}
	}
	return out
}

// probeOutcome 一次并发探测的结果。
type probeOutcome struct {
	hit bool // 候选是否存在(含 seeds 预置命中)
}

func (f *frontier) render(comp []int) string {
	if f.widths != nil {
		return joinCompsW(comp, f.widths)
	}
	return joinComps(comp)
}

// probe 探测一个版本候选, 命中即记录; 返回 (是否命中, 是否新增命中)。
// added=true 表示本次首次写入 f.hits(新发现), 用于实时命中计数。
func (f *frontier) probe(comp []int) (hit, added bool) {
	if f.aborted.Load() {
		return false, false
	}
	v := f.render(comp)
	// 已在历史区命中或本策略命中过, 跳过。
	if f.seeds[v] {
		return true, false
	}
	f.mu.Lock()
	if _, exists := f.hits[v]; exists {
		f.mu.Unlock()
		return true, false
	}
	f.mu.Unlock()

	if f.probed.Add(1) > f.budget {
		f.aborted.Store(true)
		return false, false
	}
	if f.sem != nil {
		f.sem <- struct{}{}
	}
	r := probeTemplate(f.hc, f.o, f.tpl, v)
	if f.sem != nil {
		<-f.sem
	}
	f.reportProbe()
	if usableHit(f.o, r) {
		f.mu.Lock()
		_, exists := f.hits[v]
		if !exists {
			f.hits[v] = r
		}
		f.mu.Unlock()
		added = !exists
		if added {
			// 命中计数统一在这里做: 所有入口(撒网/副版本扫描/补丁等)
			// 都经过 probe, 进度里的命中数才不会漏计。
			f.reportHit()
			if r.verified {
				// 仅在经 HEAD 头确认或 GET 魔数校验的真实命中才实时输出,
				// 与历史区"确认一个输出一个"的语义一致。
				emitURL(r.url)
			}
		}
		return true, added
	}
	return false, false
}

// isAborted 返回预算是否已耗尽。
func (f *frontier) isAborted() bool { return f.aborted.Load() }

// hitExists 报告某渲染版本串是否已命中(seeds 或本策略 hits)。
func (f *frontier) hitExists(v string) bool {
	if f.seeds[v] {
		return true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.hits[v]
	return ok
}

// probeMajorExists 并发探主版本 M 的代表性入口(每入口一个请求), 任一命中即认为该主版本
// 存在并早停(其余在途探测自然完成)。命中的入口经 probe() 记入 hits(是真实可下载版本)。
// 并发发射避免慢网络下入口探测串成 N 倍延迟。
func (f *frontier) probeMajorExists(M, P int, anchor []int) bool {
	entries := majorEntryProbes(M, P, anchor)
	res := make(chan bool, len(entries))
	launched := 0
	for _, pc := range entries {
		if f.isAborted() {
			break
		}
		launched++
		go func(pc []int) {
			hit, _ := f.probe(pc)
			res <- hit
		}(pc)
	}
	// 收集全部在途结果再返回(不提前退场): 游离探测会在 run() 结束后才完成,
	// 届时结果既漏计入 found 合并、进度行又会污染输出。
	hitAny := false
	for i := 0; i < launched; i++ {
		if <-res {
			hitAny = true
		}
	}
	return hitAny
}

// sparseMinorProbe 对主版本 M 做稀疏副版本撒网, 抓"跳过 frontStop 间距"的远端副版本。
// 稠密扫描(连续 -front-stop 次未命中即停)够不到稀疏间隔之外的副版本(如 0,1,2,9,10
// 锚点在 2 时的 9/10); 这里在稠密上界之上按近端步长 + 少量远点撒一小批并早停式探测。
// 返回命中的副版本号。成本受 cap(默认 8 个候选)约束。
func (f *frontier) sparseMinorProbe(M int, knownMinors map[int]bool) []int {
	if len(f.hi) < 2 {
		return nil
	}
	hi := f.hi[1]
	known := map[int]bool{}
	last := -1
	for m := range knownMinors {
		known[m] = true
		if m > last {
			last = m
		}
	}
	if last < 0 {
		last = 0
	}
	step := f.o.frontStop
	if step < 2 {
		step = 2
	}
	cands := []int{}
	seen := map[int]bool{}
	add := func(m int) {
		if m >= 0 && m <= hi && !known[m] && !seen[m] {
			seen[m] = true
			cands = append(cands, m)
		}
	}
	// 近端: 稠密截断点之后一小段逐值补探。
	for m := last + 1; m <= last+step && len(cands) < 8; m++ {
		add(m)
	}
	// 远端: 按步长撒点直到上界(或达候选上限)。
	for m := last + step + 1; m <= hi && len(cands) < 8; m += step {
		add(m)
	}
	sort.Ints(cands)
	// 候选并发探测(probeMinorExists 内部补丁也是并发的), 避免慢网络下串成 8×3 倍延迟。
	limit := f.o.conc
	if limit < 1 {
		limit = 1
	}
	results := make(chan int, len(cands))
	var wg sync.WaitGroup
	sem := make(chan struct{}, limit)
	for _, m := range cands {
		if f.isAborted() {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(m int) {
			defer wg.Done()
			defer func() { <-sem }()
			if f.probeMinorExists(M, m) {
				results <- m
			}
		}(m)
	}
	wg.Wait()
	close(results)
	var out []int
	for m := range results {
		out = append(out, m)
	}
	sort.Ints(out)
	return out
}

// probeMinorExists 探测 (M, mm) 基座是否存在: 并发试补丁 0..patchProbeHead, 任一命中
// 即存在(其余在途探测自然完成)。这样"只有 .1/.2 补丁、没有 .0 基座"的副版本能被发现,
// 靠前的补丁盲区(如整段只发布了 .5/.6/.7 这类低补丁孤岛——最古老的版本常落在这里)
// 也一并覆盖。并发发射避免慢速网络下顺序探测串成 N 倍延迟。2 分量版本无补丁维度。
func (f *frontier) probeMinorExists(M, mm int) bool {
	P := len(f.hi)
	maxPatch := patchProbeHead
	if P < 3 {
		maxPatch = 0
	}
	type patchRes struct {
		pz  int
		hit bool
	}
	res := make(chan patchRes, maxPatch+1)
	launched := 0
	for pz := 0; pz <= maxPatch; pz++ {
		if f.isAborted() {
			break
		}
		launched++
		go func(pz int) {
			comp := make([]int, P)
			comp[0], comp[1] = M, mm
			if P >= 3 {
				comp[2] = pz
			}
			hit, _ := f.probe(comp)
			res <- patchRes{pz, hit}
		}(pz)
	}
	// 同 probeMajorExists: 收齐在途结果再返回, 避免游离探测晚于 run() 完成。
	hitAny := false
	for i := 0; i < launched; i++ {
		if (<-res).hit {
			hitAny = true
		}
	}
	return hitAny
}

// scanMinorsPatchAware 扫描主版本 M 的副版本 0..hi[1], 每个副版本用
// probeMinorExists(并发补丁 0/1/2 早停)判定存在; skip 中的副版本免费计入。
// 流水线并发: 至多 -c 个副版本在途, 按副版本顺序消费以保持"连空截断"语义;
// 连空配额为 3×front-stop(命中即重置), 避免一段连续空副版本占满配额截掉其后真实副版本。
func (f *frontier) scanMinorsPatchAware(M int, skip map[int]bool) []int {
	if len(f.hi) < 2 {
		return nil
	}
	hi := f.hi[1]
	// 旧主版本(M < 锚点主版本)的版本史更稀疏, 连空配额放宽一倍: 后向要把
	// 最古老的版本也捞回来, 不能像前向那样一见空档就收手。
	stop := f.o.frontStop * 3
	limit := f.o.conc
	if limit < 1 {
		limit = 1
	}
	// 预取窗口只须略超前于连空配额, 过大只会对注定截断的空副版本多打请求。
	lookahead := limit
	if stop < lookahead {
		lookahead = stop
	}
	type fut struct {
		mm int
		c  chan bool
	}
	// 混合深度探测: 与最近已知副版本相邻(frontStop 内)的候选做 3 补丁深探,
	// 覆盖"只有 .1/.2、无 .0 基座"的跳号副版本; 远离已知副版本的连续空白只探 .0
	// (大概率真空), 高延迟网络下既保发现又少打请求。
	lastFound := -1
	for m := range skip {
		if m > lastFound {
			lastFound = m
		}
	}
	// pending 记录"已浅探(仅试 .0)但未命中、且还没补过深探"的副版本。
	// 窗口预取按发射那一刻的 lastFound 判定深浅, 判定可能滞后; 这类副版本
	// 若在消费时已与命中相邻、或随后被命中点覆盖, 需要补一轮 3 补丁深探——
	// "只有 .1/.2、没有 .0"的补丁型家族只有这样才不会被漏(如 autoclaw 1.17.x)。
	pending := map[int]bool{}
	// missed 记录本轮被判定为空(浅探/深探均未命中)的副版本;
	// tailProbed 保证每个副版本的补丁长尾探测至多做一次。
	missed := map[int]bool{}
	tailProbed := map[int]bool{}
	type reProbe struct {
		mm int
		c  chan bool
	}
	var reProbes []reProbe
	queueDeep := func(j int) {
		delete(pending, j)
		c := make(chan bool, 1)
		go func(j int) { c <- f.probeMinorExists(M, j) }(j)
		reProbes = append(reProbes, reProbe{mm: j, c: c})
	}
	launch := func(mm int) *fut {
		c := make(chan bool, 1)
		deep := len(f.hi) >= 3 && (lastFound < 0 || mm-lastFound <= f.o.frontStop)
		if !deep {
			pending[mm] = true
		}
		go func() {
			if deep {
				c <- f.probeMinorExists(M, mm)
			} else {
				comp := make([]int, len(f.hi))
				comp[0], comp[1] = M, mm
				hit, _ := f.probe(comp)
				c <- hit
			}
		}()
		return &fut{mm: mm, c: c}
	}

	var window []*fut
	var hits []int
	miss := 0
	mm := 0
	for {
		// 补窗: 递进副版本, skip 的免费计命中, 否则发射探测; 到截断边界即停。
		for !f.isAborted() && mm <= hi && len(window) < lookahead && miss < stop {
			if skip[mm] {
				hits = append(hits, mm)
				lastFound = mm
				miss = 0
			} else {
				window = append(window, launch(mm))
			}
			mm++
		}
		if len(window) == 0 {
			break
		}
		fu := window[0]
		window = window[1:]
		hit := <-fu.c
		if hit {
			delete(pending, fu.mm)
			hits = append(hits, fu.mm)
			if fu.mm > lastFound {
				lastFound = fu.mm
			}
			miss = 0
			// 命中点 ±frontStop 内此前浅探未中的副版本, 补一轮深探。
			for d := -f.o.frontStop; d <= f.o.frontStop; d++ {
				j := fu.mm + d
				if j >= 0 && j <= hi && pending[j] {
					queueDeep(j)
				}
			}
			// 紧下方这个副版本若本轮被判为空, 它仍可能藏着补丁长尾
			// (产品常见: N.0.33..74 长尾之后才切到 N.1.x), 用稀疏阶梯再探一遍
			// 其补丁空间——命中则按新基座计入, 稠密填充会补全整段。
			if len(f.hi) >= 3 && fu.mm-1 >= 0 && missed[fu.mm-1] && !tailProbed[fu.mm-1] {
				tailProbed[fu.mm-1] = true
				if f.probePatchTail(M, fu.mm-1) {
					hits = append(hits, fu.mm-1)
				}
			}
		} else {
			// 已知副版本的紧下方(如 anchor 副版本在 skip 中, 其下第一个空位):
			// .0/.1/.2 全空不代表没有补丁长尾, 用稀疏阶梯探一遍补丁空间。
			if len(f.hi) >= 3 && skip[fu.mm+1] && !tailProbed[fu.mm] {
				tailProbed[fu.mm] = true
				if f.probePatchTail(M, fu.mm) {
					hits = append(hits, fu.mm)
				}
			}
			missed[fu.mm] = true
			// 浅探时还不相邻、但此刻已与最新命中相邻 → 立即补深探
			// (覆盖"补丁型家族紧跟在最后一个命中之后"的情形)。
			if pending[fu.mm] && lastFound >= 0 && fu.mm-lastFound <= f.o.frontStop {
				queueDeep(fu.mm)
			}
			miss++
			if miss >= stop {
				for _, w := range window { // 丢弃在途(自然完成)
					<-w.c
				}
				window = nil
				break
			}
		}
	}
	// 收齐补救深探(不计入连空截断, 只补发现)。
	for _, rp := range reProbes {
		if <-rp.c {
			hits = append(hits, rp.mm)
		}
	}
	return hits
}

// sparseMajorCandidates 生成"广撒网"的候选主版本号(升序去重):
//  1. 近锚点稠密: [anchor-D, anchor+D] 每个值(真实产品的新主版本几乎都在此区间);
//  2. 远端尾点: 每隔 5 撒一个, 到 anchor+D+20 为止(少数跳号大版本);
//  3. 大锚点(>=20)加 5/10 整数倍点;
//  4. 年份带: 年份式锚点±4; std 锚点只撒当前年份附近(+1/0/-1)。
//
// 高延迟网络上每个 404 都贵, 因此刻意不铺密: 整体上限 capCount 个候选。
func sparseMajorCandidates(anchor []int, o *options, hi []int, reached map[int]bool) []int {
	set := map[int]bool{}
	add := func(m int) {
		if m >= 0 && m <= hi[0] && !reached[m] {
			set[m] = true
		}
	}
	addAbs := func(m int) {
		if m >= 0 && m <= 2100 && !reached[m] {
			set[m] = true
		}
	}
	D := o.frontStop * 2
	if D < 6 {
		D = 6
	}
	for m := anchor[0] - D; m <= anchor[0]+D; m++ {
		add(m)
	}
	// 远端尾点: 隔 5 撒到 D+20。
	tailHi := anchor[0] + D + 20
	for m := anchor[0] + D + 5; m <= tailHi; m += 5 {
		add(m)
	}
	// 大锚点(主版本 >=20)加 5/10 整数倍点(小锚点下属噪声)。
	if anchor[0] >= 20 {
		for m := 5; m <= hi[0] && m <= anchor[0]+D+20; m += 5 {
			add(m)
		}
		for m := 10; m <= hi[0] && m <= anchor[0]+D+20; m += 10 {
			add(m)
		}
	}
	if !(anchor[0] >= 1900 && anchor[0] <= 2100) {
		// std 锚点撒年份(当前年附近): 覆盖"本期切换为年份命名"的低成本试探, 不铺满十年。
		cur := time.Now().Year()
		for y := cur - 1; y <= cur+1; y++ {
			addAbs(y)
		}
	} else {
		for y := anchor[0] - 4; y <= anchor[0]+4; y++ {
			addAbs(y)
		}
	}
	out := make([]int, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	sort.Ints(out)
	// 候选数硬上限: 保留最靠近锚点的 capCount 个。
	capCount := 45
	if len(out) > capCount {
		// 按 |m - anchor[0]| 排序后截取。
		sort.Slice(out, func(i, j int) bool {
			di, dj := out[i]-anchor[0], out[j]-anchor[0]
			if di < 0 {
				di = -di
			}
			if dj < 0 {
				dj = -dj
			}
			return di < dj
		})
		out = out[:capCount]
		sort.Ints(out)
	}
	return out
}

// majorEntryProbes 为一个主版本 M 构造代表性入口候选 (M, m, p), 命中即认为该主版本存在。
// 覆盖小 minor/patch 的常见首发布点: X.0.0 / X.0.1 / X.1.0 / X.2.0。
// ({1,1} 作为"主版本首发"极罕见, 省略以省探测; 它会在后续稠密展开中被顺带探到。)
func majorEntryProbes(M int, P int, anchor []int) [][]int {
	shape := [][2]int{{0, 0}, {0, 1}, {1, 0}}
	var out [][]int
	seen := map[[2]int]bool{}
	for _, e := range shape {
		if seen[e] || (P < 3 && e[1] != 0) {
			continue
		}
		seen[e] = true
		c := make([]int, P)
		c[0], c[1] = M, e[0]
		if P >= 3 {
			c[2] = e[1]
		}
		out = append(out, c)
	}
	return out
}

// sweep 对一组取值(升序或降序皆可)用 build 生成候选, 流水线并发探测,
// 直到连续 o.stop 个未命中即停止该方向。命中副作用(记录/进度)在 probe 内完成。
func (f *frontier) sweep(build func(int) []int, vals []int) {
	i := 0
	f.probeParallel(func() ([]int, bool) {
		if i < len(vals) {
			v := vals[i]
			i++
			return build(v), true
		}
		return nil, false
	}, f.o.stop)
}

// rangeVals 生成 [lo,hi] 步进 step 的取值序列。
func rangeVals(lo, hi, step int) []int {
	var out []int
	if step > 0 {
		for v := lo; v <= hi; v += step {
			out = append(out, v)
		}
	} else {
		for v := lo; v >= hi; v += step {
			out = append(out, v)
		}
	}
	return out
}

// scanDown 向下滚动扫描: 以锚点为中心逐维双向扩散, 使用滚动窗口语义
// (连续 -stop 次未命中停止该方向)。每个方向均流水线并发探测。
func (f *frontier) scanDown(anchor []int) {
	P := len(anchor)

	// 先探锚点自身(恰好一次; 锚点通常命中, 不能放进带 miss 截断的并发流)。
	f.probe(anchor)

	if P == 1 {
		f.sweep(func(v int) []int { return []int{v} }, rangeVals(anchor[0]-1, 0, -1))
		f.sweep(func(v int) []int { return []int{v} }, rangeVals(anchor[0]+1, f.hi[0], 1))
		return
	}

	if P == 2 {
		// minor 两侧扩散.
		f.sweep(func(v int) []int { return []int{anchor[0], v} }, rangeVals(anchor[1]-1, 0, -1))
		f.sweep(func(v int) []int { return []int{anchor[0], v} }, rangeVals(anchor[1]+1, f.hi[1], 1))
		// major 两侧扩散.
		f.sweep(func(v int) []int { return []int{v, 0} }, rangeVals(anchor[0]-1, 0, -1))
		f.sweep(func(v int) []int { return []int{v, 0} }, rangeVals(anchor[0]+1, f.hi[0], 1))
		return
	}

	// P >= 3: 以锚点为中心逐维双向扩散(与原版方向语义一致)。
	const patchUpLimit = 5

	// 1) patch 下探 + 上探(小幅), 同基座 (anchor0,anchor1)。
	f.sweep(func(v int) []int { return []int{anchor[0], anchor[1], v} }, rangeVals(anchor[2]-1, 0, -1))
	f.sweep(func(v int) []int { return []int{anchor[0], anchor[1], v} },
		rangeVals(anchor[2]+1, anchor[2]+patchUpLimit, 1))

	// 2) minor 两侧扩散(基座 patch=0)。
	f.sweep(func(v int) []int { return []int{anchor[0], v, 0} }, rangeVals(anchor[1]-1, 0, -1))
	f.sweep(func(v int) []int { return []int{anchor[0], v, 0} },
		rangeVals(anchor[1]+1, anchor[1]+f.o.stop*2, 1))

	// 3) major 两侧扩散(基座 0,0)。尾号型 major 变化范围小(stop*2), 标准版本放宽 stop*5。
	f.sweep(func(v int) []int { return []int{v, 0, 0} }, rangeVals(anchor[0]-1, 0, -1))
	majorUpper := anchor[0] + f.o.stop*5
	if f.widths[P-1] > 2 {
		majorUpper = anchor[0] + f.o.stop*2
	}
	if majorUpper > f.hi[0] {
		majorUpper = f.hi[0]
	}
	f.sweep(func(v int) []int { return []int{v, 0, 0} }, rangeVals(anchor[0]+1, majorUpper, 1))
}

// patchProbeHead 副版本存在性深探时"密集头部"的补丁上界: 0..该值逐个探测。
// 低补丁孤岛(整段只有 .3.. 这类靠前补丁)常见于最古老的版本, 只试 0/1/2 会漏;
// 但每个深探副版本都要付这个开销, 取值刻意克制(更深的孤岛由上方的阶梯扫尾负责)。
const patchProbeHead = 5

// patchLadderStep 补丁维度"阶梯扫尾"的步长: 任何长度 ≥step 的连续补丁号段必含一个
// 阶梯点; 阶梯点命中后回探其下方 step-1 个补丁即可补齐段首。
const patchLadderStep = 5

// patchLadderBatch 阶梯/回探一次并发探测的点数。
const patchLadderBatch = 8

// patchTailReach 基座补丁填充时阶梯扫尾的额外纵深(超出该基座已知最大补丁的部分)。
const patchTailReach = 30

// patchFineTailHi 长尾探测的"近端细扫"上界: 该值以内每 patchLadderStep 个补丁探一个
// (步长 5, 任何 ≥5 的连续段必被命中); 之外改为 4 倍步长粗扫(任何 ≥20 的连续段必被
// 命中), 用更低成本覆盖远端长尾。
const patchFineTailHi = 60

// patchLadderPoints 生成从 from 到 hi 的阶梯探测点:
// 先密集扫 [from, patchProbeHead](低补丁孤岛=最古老版本常落此), 之后每
// patchLadderStep 个一点(任何 ≥step 的连续号段必被命中一点); coarseBeyond 时,
// 超过 patchFineTailHi 的远端改用 4 倍步长, 以更低成本覆盖远端长尾。
func patchLadderPoints(from, hi int, coarseBeyond bool) []int {
	var pts []int
	head := patchProbeHead
	if head > hi {
		head = hi
	}
	for p := from; p <= head; p++ {
		pts = append(pts, p)
	}
	p := patchLadderStep * ((head / patchLadderStep) + 1)
	if p < from {
		p = ((from + patchLadderStep - 1) / patchLadderStep) * patchLadderStep
	}
	for p <= hi {
		pts = append(pts, p)
		if coarseBeyond && p > patchFineTailHi {
			p += patchLadderStep * 4
		} else {
			p += patchLadderStep
		}
	}
	return pts
}

// probeMany 并发探测一组候选, 返回每个候选是否命中
// (命中记录/计数/进度由 probe 内部完成; 并发度由全局信号量约束)。
func (f *frontier) probeMany(comps [][]int) []bool {
	res := make([]bool, len(comps))
	var wg sync.WaitGroup
	for i, c := range comps {
		wg.Add(1)
		go func(i int, c []int) {
			defer wg.Done()
			res[i], _ = f.probe(c)
		}(i, c)
	}
	wg.Wait()
	return res
}

// scanDimStop 同 scanDim, 额外返回"下一个未探测取值": 稠密前沿被连空配额截断时,
// 续扫应从该值开始(全部探完则为 hi+1)。供补丁维度的阶梯续扫使用。
func (f *frontier) scanDimStop(buildCand func(int) []int, lo, hi int, skip map[int]bool) ([]int, int) {
	// 先记录非 skip 候选的取值序列, 供结果回填。
	var probeOrder []int
	for x := lo; x <= hi; x++ {
		if !skip[x] {
			probeOrder = append(probeOrder, x)
		}
	}

	pi := 0
	res := f.probeParallel(func() ([]int, bool) {
		// 跳过 skip 候选: 它们不算探测, 但要保持升序, 故逐值推进。
		for pi < len(probeOrder) {
			// 仅发射非 skip 候选; skip 已在外部计为命中。
			v := probeOrder[pi]
			pi++
			return buildCand(v), true
		}
		return nil, false
	}, f.o.frontStop)

	hits := []int{}
	for x := lo; x <= hi; x++ {
		if skip[x] {
			hits = append(hits, x)
		}
	}
	for i, h := range res {
		if h && i < len(probeOrder) {
			hits = append(hits, probeOrder[i])
		}
	}
	sort.Ints(hits)
	next := hi + 1
	if len(res) < len(probeOrder) {
		next = probeOrder[len(res)]
	}
	return hits, next
}

// scanDim 从 lo 起逐值探测维度 buildCand 直到 hi(含), 连续 front-stop 次未命中即停。
// 已存在于 skip 集合的值视为命中且跳过探测(不重复探测、不累计 miss)。
// 返回命中的取值升序列表。流水线并发: 至多 -c 个请求在途, 完成一个立即补下一个。
func (f *frontier) scanDim(buildCand func(int) []int, lo, hi int, skip map[int]bool) []int {
	hits, _ := f.scanDimStop(buildCand, lo, hi, skip)
	return hits
}

// scanPatches 补丁维度扫描: 稠密前沿(连空 front-stop 即停) + 稀疏阶梯扫尾续扫。
// 真实产品的补丁号段常是长尾/稀疏分布(如 3.0.33..3.0.74 与锚点 3.1.1 之间有 12 个
// 连空), 纯稠密扫描会在连空配额处停下, 永远够不到远处长尾。阶梯每 patchLadderStep
// 个补丁探一个(任何 ≥step 的连续号段必被命中一点), 命中后回探段首(下方 step-1 个),
// 再从命中点之上恢复稠密, 反复推进直到阶梯也扫不动为止。扫尾纵深为
// max(lo, knownMax) + patchTailReach: knownMax 是该基座已发现的最大补丁
// (含本轮之前由长尾探测/历史发现的), 避免在注定为空的远端白打请求。
func (f *frontier) scanPatches(buildPatch func(int) []int, lo, hi int, skip map[int]bool, knownMax, reach int) []int {
	build := func(ps []int) [][]int {
		out := make([][]int, 0, len(ps))
		for _, p := range ps {
			out = append(out, buildPatch(p))
		}
		return out
	}
	var hits []int
	cur := lo
	for cur <= hi && !f.isAborted() {
		seg, next := f.scanDimStop(buildPatch, cur, hi, skip)
		hits = append(hits, seg...)
		if next > hi {
			break
		}
		maxKnown := knownMax
		if maxKnown < lo {
			maxKnown = lo
		}
		for _, p := range hits {
			if p > maxKnown {
				maxKnown = p
			}
		}
		ladderHi := maxKnown + reach
		if ladderHi > hi {
			ladderHi = hi
		}
		// 阶梯: 低补丁密集头部 + 每 patchLadderStep 个一点(精度优先, 不用粗扫)。
		ladder := patchLadderPoints(next, ladderHi, false)
		found := -1
		for i := 0; i < len(ladder) && found < 0; i += patchLadderBatch {
			end := i + patchLadderBatch
			if end > len(ladder) {
				end = len(ladder)
			}
			for j, hit := range f.probeMany(build(ladder[i:end])) {
				if hit {
					found = ladder[i+j] // 批内取最靠前的命中
					break
				}
			}
		}
		if found < 0 {
			break
		}
		// 回探段首: found 下方最多 step-1 个补丁可能与它同段(命中记录由 probe 完成)。
		var back []int
		for q := found - 1; q >= lo && q > found-patchLadderStep; q-- {
			back = append(back, q)
		}
		f.probeMany(build(back))
		hits = append(hits, found)
		cur = found + 1
	}
	return hits
}

// probePatchTail 用稀疏阶梯把一个基座的整个补丁空间扫一遍(每 patchLadderStep 个
// 探一个), 任一命中即返回 true。用于"已知副版本紧下方"可能存在的补丁长尾——
// 该副版本自身低补丁都不存在(被判为空), 但长尾往往真实存在(产品常见模式:
// N.0.x 的 33..74 长尾之后才切到 N.1.x; 或最古老的版本是 .5/.6 这类低补丁孤岛)。
// 命中后调用方把该副版本计为命中, 后续的稠密填充会补全整段。
func (f *frontier) probePatchTail(M, mm int) bool {
	P := len(f.hi)
	hi := f.hi[2]
	build := func(ps []int) [][]int {
		out := make([][]int, 0, len(ps))
		for _, p := range ps {
			c := make([]int, P)
			c[0], c[1], c[2] = M, mm, p
			out = append(out, c)
		}
		return out
	}
	// 阶梯点: 低补丁密集头部 + 每 patchLadderStep 个一点(远端 4 倍步长)。
	pts := patchLadderPoints(3, hi, true)
	for i := 0; i < len(pts); i += patchLadderBatch {
		end := i + patchLadderBatch
		if end > len(pts) {
			end = len(pts)
		}
		for _, hit := range f.probeMany(build(pts[i:end])) {
			if hit {
				return true
			}
		}
	}
	return false
}

// computeHi 计算各维度前沿上界: 默认 universe; 锚点分量超过 universe
// (年份式主版本 2026.x、尾号大数)时以锚点为下界再留前瞻余量, 使前沿能向上生长。
func computeHi(anchor []int, o *options) []int {
	lookahead := o.frontStop * 5
	if lookahead < 20 {
		lookahead = 20
	}
	hi := make([]int, len(anchor))
	for i := range hi {
		hi[i] = o.universe
		if a := anchor[i]; a >= hi[i] {
			hi[i] = a + lookahead
		}
	}
	return hi
}

// wideNetBudget 广撒网投机探测的总墙钟预算: 高速网络下数秒即可撒完全部候选;
// 高延迟网络下到点优雅收尾, 不让投机探测无限拖时间。
const wideNetBudget = 12 * time.Second

// run 执行前沿探索。
func (f *frontier) run(anchor []int) {
	P := len(anchor)
	if P == 0 || f.isAborted() {
		return
	}
	f.hi = computeHi(anchor, f.o)
	f.anchor = anchor
	majors, minors, patches := parseSeeds(f.seeds, P)

	// ---- 主版本前沿: (M,0,0,..) ----
	majors[anchor[0]] = true
	buildMajor := func(M int) []int {
		c := make([]int, P)
		c[0] = M
		return c
	}
	majorHits := f.scanDim(buildMajor, anchor[0], f.hi[0], majors)

	// ---- 副版本前沿 + 补丁 ----
	// 锚点/历史主版本的副版本+补丁扫描与广撒网互相独立, 并行执行,
	// 高延迟网络上不再白白串一轮等待; 广撒网新发现的主版本边发现边开扫。
	var scanWG sync.WaitGroup
	startScan := func(M int) {
		scanWG.Add(1)
		go func() {
			defer scanWG.Done()
			f.scanMajor(M, anchor, P, minors, patches)
		}()
	}
	for _, M := range majorHits {
		if !f.isAborted() {
			startScan(M)
		}
	}
	// 广撒网节奏化 + 墙钟预算(见上), 命中的主版本即时开扫。
	if P >= 2 {
		reached := map[int]bool{}
		for _, M := range majorHits {
			reached[M] = true
		}
		cands := sparseMajorCandidates(anchor, f.o, f.hi, reached)
		wave := f.o.conc
		if wave < 1 {
			wave = 1
		}
		netStart := time.Now()
		for i := 0; i < len(cands); {
			if f.isAborted() || time.Since(netStart) > wideNetBudget {
				break
			}
			var wg sync.WaitGroup
			var wmu sync.Mutex
			end := i + wave
			if end > len(cands) {
				end = len(cands)
			}
			for ; i < end; i++ {
				M := cands[i]
				wg.Add(1)
				go func(M int) {
					defer wg.Done()
					if f.probeMajorExists(M, P, anchor) {
						wmu.Lock()
						if !reached[M] {
							reached[M] = true
							majorHits = append(majorHits, M)
							startScan(M)
						}
						wmu.Unlock()
					}
				}(M)
			}
			wg.Wait()
		}
	}
	scanWG.Wait()
	sort.Ints(majorHits)
	f.done.Store(true) // 收尾: 之后的探测(若有)不再刷新进度行
}

// scanMajor 对一个主版本 M 做副版本扫描 + 稀疏副版本撒网 + 补丁填充,
// 结果写入 f.hits(线程安全)。可对多个主版本并行调用。
func (f *frontier) scanMajor(M int, anchor []int, P int,
	minors map[int]map[int]bool, patches map[[2]int]map[int]bool) {
	ms := minors[M]
	if ms == nil {
		ms = map[int]bool{}
	}
	if P >= 2 && M == anchor[0] {
		ms[anchor[1]] = true
	}
	var minorHits []int
	if P >= 2 {
		// 副版本扫描按补丁 0/1/2 早停探测, 覆盖"只有 .1/.2、无 .0 基座"的副版本。
		// 历史区已命中的副版本在 skip 中, 直接计入不再探测。
		minorHits = f.scanMinorsPatchAware(M, ms)
		// 稀疏副版本撒网: 抓稠密扫描(连空即停)够不到的跳号副版本。
		knownMinors := map[int]bool{}
		for _, mm := range minorHits {
			knownMinors[mm] = true
		}
		for _, mm := range f.sparseMinorProbe(M, knownMinors) {
			if !knownMinors[mm] {
				knownMinors[mm] = true
				minorHits = append(minorHits, mm)
			}
		}
		sort.Ints(minorHits)
	} else {
		minorHits = []int{0}
	}

	// 各基座已发现的最大补丁快照(含本轮长尾探测/历史命中发现的), 供补丁填充的
	// 阶梯扫尾定纵深——否则"刚被长尾探测发现较远补丁"的基座会因纵深不足而扫不到。
	baseMaxPatch := map[[2]int]int{}
	f.mu.Lock()
	for v := range f.hits {
		if c := parseSimple(v); len(c) >= 3 {
			k := [2]int{c[0], c[1]}
			if c[2] > baseMaxPatch[k] {
				baseMaxPatch[k] = c[2]
			}
		}
	}
	f.mu.Unlock()

	for _, mm := range minorHits {
		if f.isAborted() {
			return
		}
		// 需要补丁前沿的基座: 锚点的 (主,副) 基座、高于锚点的前沿基座,
		// 以及任何"非种子新发现"的基座(如撒网抓到的跳号副版本)。
		isFront := M != anchor[0] || P < 3 || (M == anchor[0] && mm > anchor[1])
		isAnchorBase := M == anchor[0] && mm == anchor[1]
		seeded := P >= 2 && minors[M] != nil && minors[M][mm]
		isNewBase := !seeded && !isAnchorBase
		if P < 3 || (!isFront && !isAnchorBase && !isNewBase) {
			continue
		}
		pSet := patches[[2]int{M, mm}]
		if pSet == nil {
			pSet = map[int]bool{}
		}
		lo := 0
		if isAnchorBase {
			lo = anchor[2] + 1 // 锚点基座从已知补丁+1 起, 找更新的补丁
		}
		// 补丁阶梯纵深: maxKnown 已包含预扫深补丁点/长尾探测发现的远补丁,
		// 因此远长尾由"预扫播种 + 纵深外推"覆盖, 无需给旧主版本额外放大纵深。
		knownMax := baseMaxPatch[[2]int{M, mm}]
		reach := patchTailReach
		buildPatch := func(pz int) []int {
			c := make([]int, P)
			c[0], c[1], c[2] = M, mm, pz
			return c
		}
		f.scanPatches(buildPatch, lo, f.hi[2], pSet, knownMax, reach)
	}
}

// parseSeeds 从历史命版本里提取主版本/副版本/补丁集合。
func parseSeeds(seeds map[string]bool, P int) (map[int]bool, map[int]map[int]bool, map[[2]int]map[int]bool) {
	majors := map[int]bool{}
	minors := map[int]map[int]bool{}
	patches := map[[2]int]map[int]bool{}
	for v := range seeds {
		comp := parseSimple(v)
		if len(comp) < 1 || comp[0] < 0 {
			continue
		}
		majors[comp[0]] = true
		if len(comp) >= 2 {
			if minors[comp[0]] == nil {
				minors[comp[0]] = map[int]bool{}
			}
			minors[comp[0]][comp[1]] = true
		}
		if len(comp) >= 3 {
			key := [2]int{comp[0], comp[1]}
			if patches[key] == nil {
				patches[key] = map[int]bool{}
			}
			patches[key][comp[2]] = true
		}
	}
	return majors, minors, patches
}

func parseSimple(s string) []int {
	parts := strings.Split(s, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil && n >= 0 {
			out = append(out, n)
		}
	}
	return out
}

// sortVersionsNatural 按组件数值升序排序版本串。
func sortVersionsNatural(versions []string) {
	sort.Slice(versions, func(i, j int) bool {
		return lessVersionNatural(versions[i], versions[j])
	})
}

func lessVersionNatural(a, b string) bool {
	ac, bc := parseSimple(a), parseSimple(b)
	for i := 0; i < len(ac) && i < len(bc); i++ {
		if ac[i] != bc[i] {
			return ac[i] < bc[i]
		}
	}
	return len(ac) < len(bc)
}
