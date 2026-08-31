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
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// frontier 前沿探测器。
type frontier struct {
	hc         *http.Client
	o          *options
	tpl        string
	ua         string
	widths     []int           // 组件宽度(保持 2025.08.22 式前导零)
	hi         []int           // 每维度上界(universe 或显式 -to)
	seeds      map[string]bool // 历史命中版本(播种已知主/次/补丁)
	hits       map[string]probeResult
	probed     *atomic.Int64
	hitsFound  *atomic.Int64 // 全程命中计数(用于实时进度)
	budget     int64
	aborted    atomic.Bool // 预算是否耗尽(并发探测下用原子标志)
	mu         sync.Mutex
}

func newFrontier(hc *http.Client, o *options, tpl string, widths []int,
	seeds map[string]bool, probed *atomic.Int64, hitsFound *atomic.Int64, budget int64) *frontier {
	return &frontier{
		hc: hc, o: o, tpl: tpl, ua: o.ua, widths: widths,
		seeds: seeds, hits: map[string]probeResult{},
		probed: probed, hitsFound: hitsFound, budget: budget,
	}
}

// reportHit 在并发扫描命中新基座时刷新实时进度。
func (f *frontier) reportHit() {
	if f.hitsFound == nil {
		return
	}
	f.hitsFound.Add(1)
	if !f.o.quiet {
		fmt.Fprintf(os.Stderr, "\r探测中 ... %d 次请求, 命中 %d 个    ", f.probed.Load(), f.hitsFound.Load())
	}
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
	type future struct{ c chan probeOutcome }
	launch := func(comp []int) *future {
		c := make(chan probeOutcome, 1)
		go func() {
			hit, added := f.probe(comp)
			c <- probeOutcome{hit: hit, added: added}
		}()
		return &future{c: c}
	}

	var window []*future
	genDone := false
	miss := 0
	var out []bool
	for {
		// 补充在途窗口至 limit, 但不超过 miss 边界。
		for !genDone && !f.isAborted() && len(window) < limit && (stop <= 0 || miss < stop) {
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
		if o.added {
			f.reportHit()
		}
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
					if oo.added {
						f.reportHit()
					}
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
	hit   bool // 候选是否存在(含 seeds 预置命中)
	added bool // 是否首次写入 hits(真正的新发现)
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
	r := probeTemplate(f.hc, f.o, f.tpl, v)
	if usableHit(f.o, r) {
		f.mu.Lock()
		_, exists := f.hits[v]
		if !exists {
			f.hits[v] = r
		}
		f.mu.Unlock()
		return true, !exists
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

// probeMajorExists 顺序探主版本 M 的代表性入口, 任一命中即认为该主版本存在并早停。
// 命中的入口经 probe() 记入 hits(是真实可下载版本)。已存在的主版本因此只花 1 次探测。
func (f *frontier) probeMajorExists(M, P int, anchor []int) bool {
	for _, pc := range majorEntryProbes(M, P, anchor) {
		if f.isAborted() {
			return false
		}
		hit, _ := f.probe(pc)
		if hit {
			return true
		}
	}
	return false
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
	var out []int
	for _, m := range cands {
		if f.isAborted() {
			break
		}
		comp := make([]int, len(f.hi))
		comp[0], comp[1] = M, m
		hit, _ := f.probe(comp)
		if hit {
			out = append(out, m)
		}
	}
	return out
}

// sparseMajorCandidates 生成"广撒网"的候选主版本号(升序去重):
//  1. 近锚点稠密: [anchor-D, anchor+D] 每个值;
//  2. 中程步长 2: 直到 anchor+midBand;
//  3. 整数倍撒点: 5/10 的倍数(只到 hi 与 midBand 的较小者, 避免覆盖整个 universe);
//  4. 年份带: 另一套编号体系(2021..2030 或锚点±4), 不受 universe 限制。
// 整体上限 capCount 个候选, 使"广撒网"成本稳定在几十次而非上千。
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
	midBand := anchor[0] + D + 25
	for m := anchor[0] + D + 1; m <= midBand; m += 2 {
		add(m)
	}
	// 整数倍只撒到近端范围, 不覆盖整个 universe。
	// 小锚点(主版本 <20)下 5/10 倍数的远端主版本基本不存在, 属噪声, 仅大锚点启用。
	if anchor[0] >= 20 {
		for m := 5; m <= hi[0] && m <= midBand+D; m += 5 {
			add(m)
		}
		for m := 10; m <= hi[0] && m <= midBand+D; m += 10 {
			add(m)
		}
	}
	if !(anchor[0] >= 1900 && anchor[0] <= 2100) {
		for y := 2021; y <= 2030; y++ {
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
	shape := [][2]int{{0, 0}, {0, 1}, {1, 0}, {2, 0}}
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
	if _, added := f.probe(anchor); added {
		f.reportHit()
	}

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

// scanDim 从 lo 起逐值探测维度 buildCand 直到 hi(含), 连续 front-stop 次未命中即停。
// 已存在于 skip 集合的值视为命中且跳过探测(不重复探测、不累计 miss)。
// 返回命中的取值升序列表。流水线并发: 至多 -c 个请求在途, 完成一个立即补下一个。
func (f *frontier) scanDim(buildCand func(int) []int, lo, hi int, skip map[int]bool) []int {
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
	return hits
}

// computeHi 计算各维度前沿上界: 默认 universe; 锚点分量超过 universe
// (年份式主版本 2026.x、尾号大数)时以锚点为下界再留前瞻余量, 使前沿能向上生长。
// 显式 -to 若更高则覆盖。
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
	if o.to != "" {
		if tc, _, err := parseIntsW(o.to); err == nil {
			for i, c := range tc {
				if i < len(hi) && c > hi[i] {
					hi[i] = c
				}
			}
		}
	}
	return hi
}

// run 执行前沿探索。
func (f *frontier) run(anchor []int) {
	P := len(anchor)
	if P == 0 || f.isAborted() {
		return
	}
	f.hi = computeHi(anchor, f.o)
	majors, minors, patches := parseSeeds(f.seeds, P)

	// ---- 主版本前沿: (M,0,0,..) ----
	majors[anchor[0]] = true
	buildMajor := func(M int) []int {
		c := make([]int, P)
		c[0] = M
		return c
	}
	majorHits := f.scanDim(buildMajor, anchor[0], f.hi[0], majors)

	// 广撒网(wide net): 稀疏探测一批候选主版本(近锚点稠密 + 远端稀疏撒点 + 年份带),
	// 逐候选"早停"探测: 命中任一代表性入口即认为该主版本存在(已存在的主版本只花 1 次)。
	// 命中的主版本纳入, 后续按主版本稠密展开。总请求量几十到一两百。
	if P >= 2 {
		reached := map[int]bool{}
		for _, M := range majorHits {
			reached[M] = true
		}
		cands := sparseMajorCandidates(anchor, f.o, f.hi, reached)
		var wg sync.WaitGroup
		var mu sync.Mutex
		sem := make(chan struct{}, f.o.conc)
		for _, M := range cands {
			if f.isAborted() {
				break
			}
			sem <- struct{}{}
			wg.Add(1)
			go func(M int) {
				defer wg.Done()
				defer func() { <-sem }()
				if f.probeMajorExists(M, P, anchor) {
					mu.Lock()
					if !reached[M] {
						reached[M] = true
						majorHits = append(majorHits, M)
					}
					mu.Unlock()
				}
			}(M)
		}
		wg.Wait()
		sort.Ints(majorHits)
	}

	// ---- 副版本前沿 + 补丁 ----
	for _, M := range majorHits {
		if f.isAborted() {
			return
		}
		ms := minors[M]
		if ms == nil {
			ms = map[int]bool{}
		}
		if P >= 2 && M == anchor[0] {
			ms[anchor[1]] = true
		}
		buildMinor := func(mm int) []int {
			c := make([]int, P)
			c[0], c[1] = M, mm
			return c
		}
		var minorHits []int
		if P >= 2 {
			minorHits = f.scanDim(buildMinor, 0, f.hi[1], ms)
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
			lo := 1
			if isAnchorBase {
				lo = anchor[2] + 1 // 锚点基座从已知补丁+1 起, 找更新的补丁
			}
			buildPatch := func(pz int) []int {
				c := make([]int, P)
				c[0], c[1], c[2] = M, mm, pz
				return c
			}
			f.scanDim(buildPatch, lo, f.hi[2], pSet)
		}
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
