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
	hc         *http.Client
	o          *options
	tpl        string
	ua         string
	widths     []int           // 组件宽度(保持 2025.08.22 式前导零)
	hi         []int           // 每维度上界(universe 或显式 -to)
	seeds      map[string]bool // 历史命中版本(播种已知主/次/补丁)
	hits       map[string]probeResult
	probed     *atomic.Int64
	budget     int64
	aborted    bool
	windowMiss int // 滚动窗口: 连续未命中计数(命中刷新)
	mu         sync.Mutex
}

func newFrontier(hc *http.Client, o *options, tpl string, widths []int,
	seeds map[string]bool, probed *atomic.Int64, budget int64) *frontier {
	return &frontier{
		hc: hc, o: o, tpl: tpl, ua: o.ua, widths: widths,
		seeds: seeds, hits: map[string]probeResult{},
		probed: probed, budget: budget,
	}
}

func (f *frontier) render(comp []int) string {
	if f.widths != nil {
		return joinCompsW(comp, f.widths)
	}
	return joinComps(comp)
}

// probe 探测一个版本候选, 命中即记录; 返回 (是否命中)。
func (f *frontier) probe(comp []int) bool {
	if f.aborted {
		return false
	}
	v := f.render(comp)
	// 已在历史区命中或本策略命中过, 跳过。
	if f.seeds[v] {
		return true
	}
	f.mu.Lock()
	if _, exists := f.hits[v]; exists {
		f.mu.Unlock()
		return true
	}
	f.mu.Unlock()

	if f.probed.Add(1) > f.budget {
		f.aborted = true
		return false
	}
	u := strings.ReplaceAll(f.tpl, "{v}", v)
	r := probeURLWithQueryFallback(f.hc, f.o, u)
	if r.found && (r.size < 0 || r.size >= f.o.minSize) {
		f.mu.Lock()
		f.hits[v] = r
		f.mu.Unlock()
		return true
	}
	return false
}

// abortedProbeCount 返回因预算中止的探测数。
func (f *frontier) isAborted() bool { return f.aborted }

// resetWindow 重置滚动未命中窗口(用于维度切换)。
func (f *frontier) resetWindow() {
	f.mu.Lock()
	f.windowMiss = 0
	f.mu.Unlock()
}

// windowProbe 探测一个候选并维护滚动窗口: 命中刷新计数; 连续未命中达到
// -stop 次后返回 false(调用方应停止当前方向的扫描)。命中/未命中均返回是否继续。
// windowProbe 探测 + 滚动窗口计数; 调用方需持有 f.mu。
func (f *frontier) windowProbe(comp []int) bool {
	hit := f.probe(comp)
	if hit {
		f.windowMiss = 0
	} else {
		f.windowMiss++
	}
	return f.windowMiss < f.o.stop
}

// oneDimUp 单组件向上滚动: v+1, v+2 ... 到达 v+universe 或窗口停止。
func (f *frontier) oneDimUp(base int) {
	hi := base + f.o.universe
	for v := base + 1; v <= hi && !f.isAborted(); v++ {
		if !f.windowProbe([]int{v}) {
			break
		}
	}
}

// scanDown 向下滚动扫描: 从锚点分量递减, 使用滚动窗口语义(连续 -stop 次未命中停止)。
// P==1: 单层直接递减.
// P==2: minor 递减 → major 递减.
// P>=3: patch 下探 → minor 下探(补 patch 上探) → major 下探(扫 minor/patch)。
func (f *frontier) scanDown(anchor []int) {
	P := len(anchor)

	if P == 1 {
		// 先探锚点自身, 再向两侧扩散.
		if f.windowProbe(anchor) {
			f.resetWindow()
		}
		for v := anchor[0] - 1; v >= 0 && !f.isAborted(); v-- {
			if !f.windowProbe([]int{v}) {
				break
			}
		}
		f.resetWindow()
		for v := anchor[0] + 1; v <= f.hi[0] && !f.isAborted(); v++ {
			if !f.windowProbe([]int{v}) {
				break
			}
		}
		return
	}

	if P == 2 {
		// 先探锚点自身.
		if f.windowProbe(anchor) {
			f.resetWindow()
		}
		// minor 两侧扩散.
		for mm := anchor[1] - 1; mm >= 0 && !f.isAborted(); mm-- {
			if !f.windowProbe([]int{anchor[0], mm}) {
				break
			}
		}
		f.resetWindow()
		for mm := anchor[1] + 1; mm <= f.hi[1] && !f.isAborted(); mm++ {
			if !f.windowProbe([]int{anchor[0], mm}) {
				break
			}
		}
		f.resetWindow()
		// major 两侧扩散.
		for M2 := anchor[0] - 1; M2 >= 0 && !f.isAborted(); M2-- {
			if !f.windowProbe([]int{M2}) {
				break
			}
		}
		f.resetWindow()
		for M2 := anchor[0] + 1; M2 <= f.hi[0] && !f.isAborted(); M2++ {
			if !f.windowProbe([]int{M2}) {
				break
			}
		}
		return
	}

	// P >= 3: 严格以锚点为中心的双向扩散.
	// 对每个维度: 先向下试探 stop 次, 再向上试探 stop 次; 命中则 resetWindow。
	const patchUpLimit = 5

	// 1) 探锚点自身.
	if f.windowProbe(anchor) {
		f.resetWindow()
	}

	// 2) patch 向下 (anchor[P-1]-1 .. 0).
	for p := anchor[P-1] - 1; p >= 0 && !f.isAborted(); p-- {
		if !f.windowProbe([]int{anchor[0], anchor[1], p}) {
			break
		}
	}
	f.resetWindow()
	// patch 向上 (anchor[P-1]+1 .. anchor[P-1]+patchUpLimit).
	for p := anchor[P-1] + 1; p <= anchor[P-1]+patchUpLimit && !f.isAborted(); p++ {
		if !f.windowProbe([]int{anchor[0], anchor[1], p}) {
			break
		}
	}
	f.resetWindow()

	// 3) minor 向下 (anchor[1]-1 .. 0).
	for mm := anchor[1] - 1; mm >= 0 && !f.isAborted(); mm-- {
		if !f.windowProbe([]int{anchor[0], mm, 0}) {
			break
		}
	}
	f.resetWindow()
	// minor 向上 (anchor[1]+1 .. anchor[1]+stop*2).
	for mm := anchor[1] + 1; mm <= anchor[1]+f.o.stop*2 && !f.isAborted(); mm++ {
		if !f.windowProbe([]int{anchor[0], mm, 0}) {
			break
		}
	}
	f.resetWindow()

	// 4) major 向下 (anchor[0]-1 .. 0).
	for M2 := anchor[0] - 1; M2 >= 0 && !f.isAborted(); M2-- {
		if !f.windowProbe([]int{M2, 0, 0}) {
			break
		}
	}
	f.resetWindow()
	// major 向上: 尾号型版本 major 变化范围有限, 用 stop*2 上界(与 minor 向上同量级);
	// 标准版本 major 可能跨度大, 放宽到 stop*5。
	majorUpper := anchor[0] + f.o.stop*5
	if f.widths[P-1] > 2 { // 尾号型, major 通常不变
		majorUpper = anchor[0] + f.o.stop*2
	}
	if majorUpper > f.hi[0] {
		majorUpper = f.hi[0]
	}
	for M2 := anchor[0] + 1; M2 <= majorUpper && !f.isAborted(); M2++ {
		if !f.windowProbe([]int{M2, 0, 0}) {
			break
		}
	}
}

// scanDim 从 lo 起逐值探测维度 buildCand 直到 hi(含), 连续 stop 次未命中即停。
// 已存在于 skip 集合的值跳过(不重复探测)。返回命中的取值升序列表。
func (f *frontier) scanDim(buildCand func(int) []int, lo, hi int, skip map[int]bool) []int {
	var hits []int
	miss := 0
	for v := lo; v <= hi; v++ {
		if skip[v] {
			hits = append(hits, v)
			continue
		}
		if f.probe(buildCand(v)) {
			hits = append(hits, v)
			miss = 0
		} else {
			miss++
			if miss >= f.o.frontStop {
				break
			}
		}
	}
	return hits
}

// run 执行前沿探索。
func (f *frontier) run(anchor []int) {
	P := len(anchor)
	if P == 0 || f.isAborted() {
		return
	}
	// 每维度上界: 默认 universe(0..universe, 即 99 个取值);
	// 显式 -to 时以其为每维度上界。
	f.hi = make([]int, P)
	for i := range f.hi {
		f.hi[i] = f.o.universe
	}
	if f.o.to != "" {
		if tc, _, err := parseIntsW(f.o.to); err == nil {
			for i, c := range tc {
				if i < P && c >= 0 && c < f.hi[i] {
					f.hi[i] = c
				}
			}
		}
	}
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

var _ = time.Second
