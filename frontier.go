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
				for _, w := range window { // 丢弃在途(已发出的请求自然完成)
					<-w.c
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
	u := strings.ReplaceAll(f.tpl, "{v}", v)
	r := probeURLWithQueryFallback(f.hc, f.o, u)
	if r.found && (r.size < 0 || r.size >= f.o.minSize) {
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

func containsInt(s []int, x int) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
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

	// 未来主版本的"入口探测": 有些软件下一个大版本没有 X.0.0, 而是 X.1.0 / X.Y.Z
	// (年份式 2026->2027.1.0、跳过式 5->6.2.0)。对 anchor 之后尚未命中的每个主版本,
	// 试探若干代表性入口 (M, m, p), 命中即认为该主版本存在并纳入扫描。
	// 这样从 5.5.0 也能探到 7.0.1(7 的入口在 patch), 2026.2.1 探到 2027.1.0。
	if P >= 2 {
		reached := map[int]bool{}
		for _, M := range majorHits {
			reached[M] = true
		}
		minorSet := []int{0, 1, 2, 3}
		if !containsInt(minorSet, anchor[1]) {
			minorSet = append(minorSet, anchor[1])
		}
		patchSet := []int{0, 1}
		if P >= 3 && !containsInt(patchSet, anchor[2]) {
			patchSet = append(patchSet, anchor[2])
		}
		emptyRun := 0 // 连续"无任何入口"的未来主版本数, 达 front-stop 即停(避免空扫到 199)
		for M := anchor[0] + 1; M <= f.hi[0] && !f.isAborted(); M++ {
			if reached[M] {
				emptyRun = 0
				continue
			}
			var probes [][]int
			for _, m := range minorSet {
				for _, p := range patchSet {
					c := make([]int, P)
					c[0], c[1] = M, m
					if P >= 3 {
						c[2] = p
					}
					probes = append(probes, c)
				}
			}
			pi := 0
			f.probeParallel(func() ([]int, bool) {
				if pi < len(probes) {
					pc := probes[pi]
					pi++
					return pc, true
				}
				return nil, false
			}, 0) // stop=0: 不因 miss 截断, 跑完该主版本的全部入口
			found := false
			for _, pc := range probes {
				if f.hitExists(f.render(pc)) {
					majorHits = append(majorHits, M)
					found = true
					break
				}
			}
			if found {
				emptyRun = 0
			} else {
				emptyRun++
				if emptyRun >= f.o.frontStop {
					break
				}
			}
		}
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
		} else {
			minorHits = []int{0}
		}

		for _, mm := range minorHits {
			if f.isAborted() {
				return
			}
			// 需要补丁前沿的基座: 锚点的 (主,副) 基座 或 高于锚点的前沿基座。
			isFront := M != anchor[0] || P < 3 || (M == anchor[0] && mm > anchor[1])
			isAnchorBase := M == anchor[0] && mm == anchor[1]
			if P < 3 || (!isFront && !isAnchorBase) {
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
