package agent

// G1（方案 v10 §15）工具面装配缓存。
//
// 为什么需要它：AugmentTools 在每次 Worker.Execute / Planner.Plan（还有 mainagent /
// chat）都全量重跑装配 —— 重读解析全部 SKILL.md、重 spawn 每个 stdio MCP 进程 +
// tools/list 往返。这些是秒级开销，而工具面（agentKey → 可见 skills/MCP/host 工具）
// 在两次配置变更之间是不变的；稳态下每轮 run 付同样的几秒钟纯属重复劳动。
//
// 路线选择（方案给的两条里选了【路线 2：只缓存连接与解析产物】）：
//   路线 1（缓存整份 AugmentTools 产物 tools/def/closers）不可行的原因 —— 产物里有
//   per-run 可变状态：DeferredInfo.Unlock 是会话级解锁集（run A 解锁的 skill 不能泄漏
//   进 run B）；Skill 元工具的 OnInvoke 闭包捕获本 run 的 UnlockSet；meterSkillTool
//   捕获本 run 的 RunInfo（用量归因）。跨 run 复用它们等于改行为。
//   路线 2 缓存【不变的部分】：skill 文档解析产物、MCP 客户端连接（mcp.StdioClient /
//   mcphttp.Client 的 Call 都是 per-call ctx + 并发安全，连接本身与 run 无关）、工具
//   schema 列表、deferred/global 名单。每次 run 用缓存产物重建轻包装（新 UnlockSet、
//   新 OnInvoke、新 Skill 工具、host 工具现取）—— 不重 spawn 进程、不重读文件，产出
//   与全量装配逐字段等价。
//
// 失效有两条通道，缺一不可：
//  1. 包级 generation 计数器：宿主在配置写入点调用 InvalidateAssembly()（server 侧：
//     MCP save/delete、可见性 handler、agent 配置 handler、skill 文件增删改）。
//  2. 宿主 validate 探针：命中时宿主用几条廉价查询复核装配输入（可见性行、MCP 配置行）
//     是否与建产物时的快照一致。这是给【绕过 handler 的直写 DB】兜底的 —— 例如测试与
//     种子代码直接写 agent_skill_visibility；没有探针，这类写入要到下一次无关失效才生效，
//     行为就变了。
//
// cleanup 语义的变化：全量装配时代每轮 run 结束 cleanup 关掉本 run 私有的 MCP 连接；
// 缓存后连接跨 run 共享，命中的 cleanup 是 no-op，连接由进程退出统一收（stdio 子进程
// 随父进程退出；SSE 流同理）。【正在跑的 run 绝不能被配置变更掐断连接】—— 这是旧
// 语义（每 run 私有连接活到 run 结束）的硬保证，所以条目带引用计数：失效/替换只是把
// 条目逐出缓存（evicted），连接等最后一个持有者（在跑的 run）交还后才关闭。仅当装配
// 走了【不可缓存】的降级路径（如某个 MCP 拨号失败——该路径本来就要每 run 重试重报错）
// 时，release 才关掉本次新建的连接，与旧行为逐字等价。
//
// 并发：多 worker 同时首次访问同一 agentKey 时，per-key 互斥 + 双检查保证 build 每个
// (agentKey, generation) 至多跑一次，其余等待者拿到同一份产物；不同 agentKey 并行构建。

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
)

// AssemblyBuild is what the host's build func returns for one agent assembly.
// Cacheable=false marks a degraded build (transient failure inside — e.g. an MCP
// dial error the old code retried every run): its Value is still returned for this
// run, but nothing is stored and release() closes its Closers at run end.
type AssemblyBuild struct {
	Value     any
	Closers   []io.Closer
	Cacheable bool
}

type assemblyEntry struct {
	gen     uint64
	value   any
	closers []io.Closer // MCP 连接等跨 run 资源；最后一个持有者交还后关闭
	refcnt  int         // 持有者数：缓存本身算 1，每个拿到产物的在跑 run 再 +1
	evicted bool        // 已被失效/替换移出缓存；refcnt 归零即关连接
}

var (
	assemblyGen atomic.Uint64 // InvalidateAssembly 每次 +1；条目记着自己是哪一代建的

	assemblyMu    sync.Mutex
	assemblyCache = map[string]*assemblyEntry{}

	assemblyKeyLocks sync.Map // agentKey → *sync.Mutex（per-key 构建互斥）
)

// assemblyMaxAttempts bounds the build-retry loop: an InvalidateAssembly racing a
// build discards the build and retries at the new generation. Two consecutive races
// already mean a config-edit storm — give up rather than spin.
const assemblyMaxAttempts = 3

var errAssemblyInvalidated = errors.New("assembly cache: 构建期间被反复失效，本轮放弃缓存装配")

// InvalidateAssembly bumps the generation and evicts every cached assembly.
// Hosts call it after any write that changes what an agent's tool surface resolves
// to (MCP config, skill/agent visibility, agent config, skill files). In-flight
// runs keep using the artifact they already hold（引用计数保证它们的连接不被当场
// 掐断）；下一个装配才按新配置重建。
func InvalidateAssembly() {
	assemblyGen.Add(1)
	assemblyMu.Lock()
	defer assemblyMu.Unlock()
	for k, e := range assemblyCache {
		e.evicted = true
		e.releaseCacheHoldLocked()
		delete(assemblyCache, k)
	}
}

// releaseCacheHoldLocked 归还缓存本身的持有权（assemblyMu 必须已持有）。
func (e *assemblyEntry) releaseCacheHoldLocked() {
	e.refcnt--
	if e.refcnt <= 0 {
		for _, c := range e.closers {
			_ = c.Close()
		}
		e.closers = nil
	}
}

func assemblyKeyLock(key string) *sync.Mutex {
	l, _ := assemblyKeyLocks.LoadOrStore(key, &sync.Mutex{})
	return l.(*sync.Mutex)
}

// GetOrBuildAssembly returns the assembly artifact for key: the cached one when
// fresh, otherwise built by build (at most one build per key+generation;
// concurrent callers wait and reuse). validate, when non-nil, re-checks a hit
// against cheap per-run inputs — false forces a rebuild. The returned release
// func must be deferred by the caller: it drops the caller's hold on the artifact
// (closing shared connections once the cache has evicted them AND the last
// in-flight run finished), or closes the build's own connections at run end for
// an uncached (degraded) build — matching the pre-cache per-run cleanup.
func GetOrBuildAssembly(key string, validate func(value any) bool, build func() (AssemblyBuild, error)) (value any, release func(), err error) {
	noop := func() {}
	for attempt := 0; attempt < assemblyMaxAttempts; attempt++ {
		gen := assemblyGen.Load()
		e := lookupAssembly(key, gen) // 快路径：拿持有权（refcnt 在锁内 +1）
		if e != nil {
			if validate == nil || validate(e.value) {
				// validate 可能花了毫秒级查询；复核身份与代次 —— 期间被失效/替换
				// 的条目（连接可能已被收走）绝不能交出去。
				assemblyMu.Lock()
				still := assemblyCache[key] == e && e.gen == assemblyGen.Load()
				assemblyMu.Unlock()
				if still {
					return e.value, e.releaseHold, nil
				}
			}
			e.dropHold() // validate 不过 / 期间被换掉：归还持有权，走重建
		}
		// Miss（或过期）→ 按 key 串行构建。其他 key 并行构建；同 key 等待者在
		// 双检查处直接命中现成的产物。
		l := assemblyKeyLock(key)
		l.Lock()
		gen = assemblyGen.Load()
		e = lookupAssembly(key, gen) // 双检查（同样先占持有权再校验）
		if e != nil {
			if validate == nil || validate(e.value) {
				assemblyMu.Lock()
				still := assemblyCache[key] == e && e.gen == assemblyGen.Load()
				assemblyMu.Unlock()
				if still {
					l.Unlock()
					return e.value, e.releaseHold, nil
				}
			}
			e.dropHold()
		}
		b, berr := build()
		if berr != nil {
			l.Unlock()
			return nil, noop, berr
		}
		if !b.Cacheable {
			// 降级产物：本轮照旧返回（调用方拿到等价于旧路径的「部分装配」），
			// 连接随本 run 结束关闭、下轮重试 —— 与缓存前的失败路径行为一致。
			closers := b.Closers
			l.Unlock()
			return b.Value, func() {
				for _, c := range closers {
					_ = c.Close()
				}
			}, nil
		}
		if assemblyGen.Load() != gen {
			// 构建期间被失效：这批连接属于旧一代，当场关掉、按新一代重来。
			for _, c := range b.Closers {
				_ = c.Close()
			}
			l.Unlock()
			continue
		}
		assemblyMu.Lock()
		old := assemblyCache[key]
		e = &assemblyEntry{gen: gen, value: b.Value, closers: b.Closers, refcnt: 2} // 缓存 + 本 run
		assemblyCache[key] = e
		if old != nil {
			// validate 探针判过期导致的替换：缓存不再持有旧条目；仍有在跑 run
			// 握着它就等最后一个交还时再关（连接不被中途掐断）。
			old.evicted = true
			old.releaseCacheHoldLocked()
		}
		assemblyMu.Unlock()
		l.Unlock()
		return e.value, e.releaseHold, nil
	}
	return nil, noop, errAssemblyInvalidated
}

// lookupAssembly 返回当前代次的缓存条目并替调用者占住一个持有权（refcnt 在锁内
// +1）；没有或代次不符时返回 nil。validate 在锁外跑（可能含毫秒级查询），
// 占住持有权保证校验期间失效最多 evict 条目、不会在本 run 手里关连接。
func lookupAssembly(key string, gen uint64) *assemblyEntry {
	assemblyMu.Lock()
	defer assemblyMu.Unlock()
	cached := assemblyCache[key]
	if cached == nil || cached.gen != gen {
		return nil
	}
	cached.refcnt++
	return cached
}

// releaseHold 归还一个在跑 run 的持有权（作为 run 结束的 cleanup 被调用）。
func (e *assemblyEntry) releaseHold() {
	assemblyMu.Lock()
	defer assemblyMu.Unlock()
	e.refcnt--
	if e.refcnt <= 0 && e.evicted {
		for _, c := range e.closers {
			_ = c.Close()
		}
		e.closers = nil
	}
}

// dropHold 是内部路径（validate 不过 / 重试循环）归还持有权的简写。
func (e *assemblyEntry) dropHold() {
	e.releaseHold()
}
