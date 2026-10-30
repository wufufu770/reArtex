package sod

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"sync"
)

// cacheKeyVersion 是缓存键的版本串。
//
// 为什么带版本：键的构成会演进（多一个分量、改一个口径）。版本串一变 ⇒ 旧键
// 全部 miss ⇒ 老结果不会被新口径复用。少了它，改口径的那一刻就有一批按旧
// 口径算出来的结果继续被命中。
const cacheKeyVersion = "sod-normalizer-v1"

// Cache 是规整结果的进程内有界 LRU 缓存。
//
// ⚠️ **只缓存规整结果，绝不缓存裁决** —— 裁决依赖实时状态（额度 / 熔断 /
// scope 行 / finding 证据），缓存裁决 = 用过期状态放行。每一次调用都重跑
// 全部五道检查（见 chain.go 的 PreToolUse）。
//
// 无 TTL：失效靠键里的 scope_digest / task_id / prompt_digest。TTL 会引入
// 「同一命令在不同时间得到不同结论」的不确定性 —— 而这个缓存的全部价值就是
// 「同样的动作得到同样的规整结果」。
type Cache struct {
	mu  sync.Mutex
	max int
	// promptDigest 是规整提示词的摘要（缓存键的一个分量）。
	//
	// 为什么挂在缓存上而不是每次现算：它必须与「写缓存时的规整器」是同一份
	// 提示词。构造时注入 ⇒ 键与内容同源；现算则要求每次调用都拿着同一份
	// 提示词，调用方一旦换了提示词却忘了换缓存，就会命中旧结果。
	promptDigest string
	order        *list.List
	entries      map[string]*list.Element
}

// cacheEntry 是缓存里的一条规整结果。
type cacheEntry struct {
	key string
	val NormalizerResult
}

// NewCache 构造缓存。
//
// maxEntries <= 0 ⇒ 退化成 1 条（仍然可用，只是几乎不缓存）。
//
// ⚠️ promptDigest 由构造时注入（而不是每次现算）：它是「这份缓存属于哪个
// 规整器」的标识，与键的其余分量同源。选定这一种形态并在测试里钉住。
func NewCache(maxEntries int, promptDigest string) *Cache {
	if maxEntries <= 0 {
		maxEntries = 1
	}
	return &Cache{
		max:          maxEntries,
		promptDigest: promptDigest,
		order:        list.New(),
		entries:      map[string]*list.Element{},
	}
}

// Key 算缓存键。
//
// ⚠️ **必须**包含：版本串、工具名、载荷摘要、task_id、scope_digest、
// source_agent、提示词摘要。**scope_digest 缺失 = 范围旁路** —— 一个「范围
// 收紧前生成的规整结果」会被范围收紧后复用。
//
// 分量之间用 NUL 分隔：七个分量都是自由文本（agent key、工具名），不带分隔
// 符的话 ("a", "bc") 与 ("ab", "c") 会撞成同一个键。
func (c *Cache) Key(toolName, payloadDigest string, taskID int64, scopeDigest, sourceAgent string) string {
	h := sha256.New()
	write := func(s string) {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	write(cacheKeyVersion)
	write(toolName)
	write(payloadDigest)
	write(strconv.FormatInt(taskID, 10))
	write(scopeDigest)
	write(sourceAgent)
	write(c.promptDigest)
	return hex.EncodeToString(h.Sum(nil))
}

// Get 取一条规整结果（命中则把它移到队尾，维持 LRU 序）。
func (c *Cache) Get(key string) (NormalizerResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return NormalizerResult{}, false
	}
	c.order.MoveToBack(el)
	return el.Value.(*cacheEntry).val, true
}

// Put 写一条规整结果，超上限时淘汰队首（最久未用）。
func (c *Cache) Put(key string, v NormalizerResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		el.Value.(*cacheEntry).val = v
		c.order.MoveToBack(el)
		return
	}
	el := c.order.PushBack(&cacheEntry{key: key, val: v})
	c.entries[key] = el
	for c.order.Len() > c.max {
		front := c.order.Front()
		if front == nil {
			break
		}
		c.order.Remove(front)
		delete(c.entries, front.Value.(*cacheEntry).key)
	}
}

// Len 返回当前条目数（测试与观测用）。
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
