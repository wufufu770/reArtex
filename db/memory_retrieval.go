// 记忆检索（方案 B 的混合检索面）。
//
// 依据（session-groupchat-proposal.md §10.4 方案 B / §10.5 第 7、9 条 / §11.8）：
//
//	① **混合检索 + 确定性融合**：全文（tsvector）与向量（pgvector）两路各自排序，
//	   用 **RRF（Reciprocal Rank Fusion）** 融合 —— 不是拼接、不是加权求和。
//	   RRF 只看**名次**，不看两路各自的分数（ts_rank_cd 与余弦距离不可比）。
//	② **降级必须可观测**：pgvector 不可用 / 没有向量 / 没给查询向量 —— 每一种都
//	   在结果里**说清楚**，绝不假装两路都跑了（§10.5 第 7 条）。
//	③ **检索上下文按分级**：结果必须带 COMPLETE / PARTIAL / INSUFFICIENT，
//	   绝不把不完整的上下文当成完整的交出去（§10.5 第 9 条）。
//	④ **scope 过滤强制**：只查给定 scope 的当前行（valid_to IS NULL）。
//	⑤ **记忆是提示，不是安全边界**：检索失败只影响提示内容，不影响任何放行/拒绝。
//
// 中文分词：Postgres 的默认分词器把连续 CJK 当成**一个**词元（实测
// to_tsvector('simple','任务大厅') = '任务大厅'）⇒ 搜「大厅」匹配不到「任务大厅」。
// 本层用**字符二元组（bigram）**补上：把 CJK 连续段展开成相邻两字的组合，
// 索引侧与查询侧用**同一套**展开（同一条 SQL 表达式、同一个 Postgres 分词器）⇒
// 两边天然对齐，不存在「Go 侧切词、SQL 侧切词」那种会漂移的接缝。

package db

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed schema_memory_retrieval.sql
var memoryRetrievalSchemaSQL string

// memoryRetrievalSchemaReady 记录某个连接已经应用过记忆检索 DDL。
//
// 与 db/sod.go 的 sodSchemaReady 同理：用包级 sync.Map，键是底层 *sql.DB 指针。
var memoryRetrievalSchemaReady sync.Map

// EnsureMemoryRetrievalSchema 应用记忆检索的幂等 DDL。
//
// ⚠️ db/schema.sql 由别的单元持有（RESERVED 文件），本单元的 DDL 放在
// db/schema_memory_retrieval.sql，由这里在首次访问时幂等补齐（与 db/sod.go 的
// EnsureSoDSchema 同款）。
//
// ⚠️ **全文通道不依赖这张表**：tsvector 由 memory_index 现算 ⇒ 即使这张表建不出来
// （运行期角色没有 DDL 权限、或 pgvector 不存在），纯全文检索照样能用。
// 建表失败只影响向量通道，调用方会把它报成一条**降级**而不是静默。
func (d *DB) EnsureMemoryRetrievalSchema() error {
	if _, ok := memoryRetrievalSchemaReady.Load(d.DB); ok {
		return nil
	}
	if _, err := d.Exec(memoryRetrievalSchemaSQL); err != nil {
		return fmt.Errorf("应用记忆检索 schema 失败: %w", err)
	}
	memoryRetrievalSchemaReady.Store(d.DB, struct{}{})
	return nil
}

// ensureMemoryRetrievalSchema 是访问层的幂等入口。
func (d *DB) ensureMemoryRetrievalSchema() error { return d.EnsureMemoryRetrievalSchema() }

const (
	// MemoryRRFConstant 是 RRF 的 k（Reciprocal Rank Fusion 的平滑常数）。
	//
	// 取 60：Cormack 等人 2009 年那篇论文的原文取值，也是各家检索栈
	// （Elasticsearch / OpenSearch / Vespa）的默认。k 的作用是压住头部名次的
	// 统治力（名次 1 与名次 2 的分差是 1/61 与 1/62），k 越小头部越强势。
	// 不用可调参数：融合常数是**确定性**的一部分，跟着请求变就不可复现了。
	MemoryRRFConstant = 60

	// MemoryEmbeddingDim 是向量维度。
	//
	// 与 db/schema_memory_retrieval.sql 里的 vector(768) 是同一件事的两处写法。
	MemoryEmbeddingDim = 768

	// MemoryRetrievalDefaultLimit 是默认返回条数。
	MemoryRetrievalDefaultLimit = 10
	// MemoryRetrievalMaxLimit 是返回条数上限（防止一次把整个 scope 拖进上下文）。
	MemoryRetrievalMaxLimit = 100
	// MemoryRetrievalDefaultCandidateLimit 是**每一路**的候选条数（进入融合前）。
	//
	// 候选多于最终返回是 RRF 的意义所在：两路各自的头部可能不同，取并集再融合
	// 才能把「全文排第 8、向量排第 1」这种条目捞回来。
	MemoryRetrievalDefaultCandidateLimit = 50
)

// 检索上下文的分级（§10.5 第 9 条）。
const (
	// MemoryGradeComplete：上下文覆盖了这个 scope 的**全部**当前记忆。
	MemoryGradeComplete = "complete"
	// MemoryGradePartial：命中了一部分，但 scope 里还有没进上下文的东西。
	MemoryGradePartial = "partial"
	// MemoryGradeInsufficient：一条都没命中（或 scope 是空的）⇒ 拿它回答问题
	// 等于没有依据。
	MemoryGradeInsufficient = "insufficient"
)

// 降级原因码（每一种都必须在结果里显式出现）。
const (
	// MemoryDegradationNoVectorExtension：目标 Postgres 没有 pgvector 扩展。
	MemoryDegradationNoVectorExtension = "pgvector_unavailable"
	// MemoryDegradationNoVectorStore：向量表建不出来（DDL 权限 / 扩展缺失）。
	MemoryDegradationNoVectorStore = "vector_store_unavailable"
	// MemoryDegradationNoQueryEmbedding：调用方没给查询向量 ⇒ 没法跑向量通道。
	MemoryDegradationNoQueryEmbedding = "query_embedding_not_supplied"
	// MemoryDegradationNoVectorEntries：scope 里一条向量都没有。
	MemoryDegradationNoVectorEntries = "no_embeddings_in_scope"
	// MemoryDegradationVectorDimensionMismatch：查询向量维度与列维度对不上。
	MemoryDegradationVectorDimensionMismatch = "query_embedding_dimension_mismatch"
)

// ErrMemoryEmbeddingDimensionMismatch 表示查询向量的维度与存储列对不上。
var ErrMemoryEmbeddingDimensionMismatch = errors.New("查询向量维度与向量列不一致")

// ErrMemoryEmbeddingNotFound 表示该条目没有向量。
var ErrMemoryEmbeddingNotFound = errors.New("记忆条目没有向量")

// memoryCJKClass 是 CJK 统一表意文字的字符类（U+3400–U+4DBF 扩展 A +
// U+4E00–U+9FFF 基本区）。
//
// 用**字面量区间**而不是 \u 转义：Postgres 的 ARE 不支持 \u{...}，而
// regexp 支持的 \xhhh 是单字符转义 ⇒ 字面量区间是最稳的写法。
const memoryCJKClass = `[㐀-䶿一-鿿]`

// memoryTextRetrievalSQLTemplate 是全文通道的召回查询。
//
// 形状：
//
//	scoped  —— scope 内的**当前**行（valid_to IS NULL）+ 拼出来的检索文本 src；
//	docs    —— src 加上 CJK 二元组后交给 Postgres 分词器（to_tsvector）；
//	qterms  —— 查询串做同样的展开与分词，拼成 OR 的 tsquery；
//	最终   —— 按 ts_rank_cd 排序取前 N 名，并给出 1 起的名次（RRF 的输入）。
//
// ⚠️ 索引侧与查询侧的展开表达式**必须逐字一致** —— 不一致就会出现「索引里有
// '大厅'、查询里查 '任务大厅'」这种静默漏召回。两侧都调用 Postgres 自己的
// 分词器（to_tsvector）⇒ 切词规则只有一份。
const memoryTextRetrievalSQLTemplate = `
WITH scoped AS (
    SELECT m.id,
           m.title || ' ' || m.summary_line || ' ' || m.topic_key || ' ' ||
             COALESCE(array_to_string(m.tags, ' '), '') AS src
      FROM memory_index m
     WHERE m.scope_type = $1 AND m.scope_id = $2 AND m.valid_to IS NULL
       AND m.state = 'active'
),
docs AS (
    SELECT scoped.id,
           to_tsvector('simple', scoped.src || ' ' ||
             (SELECT COALESCE(string_agg(substr(scoped.src, g.i, 2), ' '), '')
                FROM generate_series(1, GREATEST(length(scoped.src) - 1, 0)) AS g(i)
               WHERE substr(scoped.src, g.i, 1) ~ '%[1]s'
                 AND substr(scoped.src, g.i + 1, 1) ~ '%[1]s')) AS tsv
      FROM scoped
),
qterms AS (
    SELECT to_tsquery('simple',
             (SELECT string_agg(quote_literal(lex), ' | ')
                FROM unnest(to_tsvector('simple', $3 || ' ' ||
                     (SELECT COALESCE(string_agg(substr($3, g.i, 2), ' '), '')
                        FROM generate_series(1, GREATEST(length($3) - 1, 0)) AS g(i)
                       WHERE substr($3, g.i, 1) ~ '%[1]s'
                         AND substr($3, g.i + 1, 1) ~ '%[1]s'))) t(lex, pos, wt))
           ) AS tsq
)
SELECT d.id,
       row_number() OVER (ORDER BY ts_rank_cd(d.tsv, q.tsq) DESC, d.id) AS rk
  FROM docs d, qterms q
 WHERE d.tsv @@ q.tsq
 ORDER BY ts_rank_cd(d.tsv, q.tsq) DESC, d.id
 LIMIT $4`

// memoryVectorRetrievalSQL 是向量通道的召回查询（余弦距离，最近的在前）。
//
// ⚠️ 这条 SQL 里出现了 vector 类型 ⇒ **只有在 pgvector 可用时才能解析**。
//
//	所以它绝不与全文查询拼在一起跑：向量不可用时整条语句都不发出去，
//	由调用方记一条降级。
const memoryVectorRetrievalSQL = `
SELECT e.entry_id,
       row_number() OVER (ORDER BY e.embedding <=> $3::vector) AS rk
  FROM memory_embedding e
 WHERE e.scope_type = $1 AND e.scope_id = $2
 ORDER BY e.embedding <=> $3::vector
 LIMIT $4`

// memoryScopeTotalSQL 数 scope 里的**当前且 active** 条目数（分级的分母）。
// X4（方案 v10 §6 T2）：两段式披露下可检索宇宙 = active 条目 —— 隔离态条目
// 不进分母，否则「全部 active 都命中了」会被隔离行拖成 PARTIAL（分级骗人）。
const memoryScopeTotalSQL = `
SELECT count(*) FROM memory_index
 WHERE scope_type = $1 AND scope_id = $2 AND valid_to IS NULL AND state = 'active'`

// memoryEntriesByIDsSQL 按 id 取索引行（融合之后拿元数据）。
// X4：只取 active —— 向量通道（memory_embedding 无 state 列）可能把隔离态
// 条目捞进名次，这里统一过滤（与「行在融合与取元数据之间被退役」同一处理：
// 拿不到元数据 = 跳过，不把不可见内容交给调用方）。
const memoryEntriesByIDsSQL = `
SELECT id, topic_key, version, kind, title, summary_line, written_by, valid_from
  FROM memory_index
 WHERE id = ANY($1::bigint[]) AND valid_to IS NULL AND state = 'active'`

// memoryVectorCountSQL 数 scope 里的向量条数。
const memoryVectorCountSQL = `
SELECT count(*) FROM memory_embedding WHERE scope_type = $1 AND scope_id = $2`

// memoryVectorDimSQL 读向量列的实际维度（写入侧校验用）。
const memoryVectorDimSQL = `
SELECT format_type(a.atttypid, a.atttypmod)
  FROM pg_attribute a
  JOIN pg_class c ON c.oid = a.attrelid
 WHERE c.relname = 'memory_embedding' AND a.attname = 'embedding'`

// memoryTextRetrievalSQL 是拼好的全文召回查询。
var memoryTextRetrievalSQL = fmt.Sprintf(memoryTextRetrievalSQLTemplate, memoryCJKClass)

// MemoryRetrievalQuery 是一次记忆检索的输入。
type MemoryRetrievalQuery struct {
	// Scope 定位归属范围（强制）。
	Scope MemoryScope
	// Text 是查询串（自然语言）。空 ⇒ 不跑全文通道。
	Text string
	// Embedding 是查询向量（可选）。
	//
	// 没有它 ⇒ 跑不了向量通道（记一条降级，不假装跑了）。
	Embedding []float32
	// Limit 是返回条数（0 ⇒ 默认 MemoryRetrievalDefaultLimit）。
	Limit int
	// CandidateLimit 是每路的候选条数（0 ⇒ 默认 MemoryRetrievalDefaultCandidateLimit）。
	CandidateLimit int
}

// MemoryRetrievalHit 是一条命中。
type MemoryRetrievalHit struct {
	EntryID     int64   `json:"entry_id"`
	TopicKey    string  `json:"topic_key"`
	Title       string  `json:"title"`
	SummaryLine string  `json:"summary_line"`
	Kind        string  `json:"kind"`
	Version     int     `json:"version"`
	WrittenBy   string  `json:"written_by"`
	Score       float64 `json:"score"`
	// TextRank 是全文通道里的名次（1 起）；0 = 没进全文候选。
	TextRank int `json:"text_rank"`
	// VectorRank 是向量通道里的名次（1 起）；0 = 没进向量候选。
	VectorRank int `json:"vector_rank"`
}

// MemoryRetrievalDegradation 是一条**显式**的降级说明。
//
// 为什么要有这个结构：pgvector 不可用时，如果只是"少了一条结果"，读代码的人
// 根本看不出来向量通道根本没跑 ⇒ 于是"混合检索"悄悄退化成"全文检索"而没人知道。
// 降级必须出现在结果里。
type MemoryRetrievalDegradation struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// MemoryRetrievalResult 是一次检索的完整结果。
type MemoryRetrievalResult struct {
	Scope MemoryScope `json:"scope"`
	// Grade 是上下文分级（complete / partial / insufficient）。
	Grade string `json:"grade"`
	// GradeReason 是分级的机器可读原因码。
	GradeReason string `json:"grade_reason"`
	// ScopeTotal 是 scope 里当前索引条目总数（分级的分母）。
	ScopeTotal int `json:"scope_total"`
	// Matched 是两路候选并集去重后的条数（融合前的命中面）。
	Matched int `json:"matched"`
	// Returned 是实际返回的条数（进上下文的条数）。
	Returned int `json:"returned"`
	// Truncated 为真表示命中面被返回上限截断了。
	Truncated bool `json:"truncated"`
	// Coverage 是返回条数 / ScopeTotal（0..1）。
	Coverage float64 `json:"coverage"`

	Hits []*MemoryRetrievalHit `json:"hits"`
	// Channels 是**实际跑过**的检索通道（text / vector）。
	Channels []string `json:"channels"`
	// Degradations 是显式的降级说明（可能为空）。
	Degradations []*MemoryRetrievalDegradation `json:"degradations"`
	// RRFFused 为真表示做了多路融合；单路时为假（那时融合退化成名次本身）。
	RRFFused bool `json:"rrf_fused"`
	// RRFConstant 是这次用的 k（写进结果 ⇒ 事后可复算）。
	RRFConstant int `json:"rrf_constant"`
	// CandidateLimit 是每路的候选条数。
	CandidateLimit int `json:"candidate_limit"`
}

// rrfList 是一路检索的结果：条目 id → 名次（1 起）。
type rrfList struct {
	name  string
	ranks map[int64]int
}

// rrfEntry 是融合后的一个条目。
type rrfEntry struct {
	EntryID    int64
	Score      float64
	TextRank   int
	VectorRank int
}

// fuseRRF 用 Reciprocal Rank Fusion 融合多路检索结果。
//
// 公式（Cormack et al. 2009）：
//
//	score(d) = Σ_i  1 / (k + rank_i(d))
//
// 其中 rank_i(d) 是条目 d 在第 i 路里的名次（1 起），k 是平滑常数
// （MemoryRRFConstant = 60）。出现在多路里的条目分高，只出现在一路里的条目
// 分低但**不会被抹掉** —— 这正是 RRF 与「加权求和」的区别：加权求和要先把两路
// 的分数归一化到同一量纲（ts_rank_cd 与余弦距离根本不可比），RRF 只用名次，
// 天然免疫量纲问题。
//
// 确定性：同分时按 entry_id 升序 ⇒ 同样的输入永远得到同样的顺序。
func fuseRRF(lists []rrfList, k int) []rrfEntry {
	if k <= 0 {
		k = MemoryRRFConstant
	}
	scores := make(map[int64]float64)
	textRanks := make(map[int64]int)
	vectorRanks := make(map[int64]int)
	for _, l := range lists {
		for id, rank := range l.ranks {
			scores[id] += 1.0 / float64(k+rank)
			switch l.name {
			case "text":
				textRanks[id] = rank
			case "vector":
				vectorRanks[id] = rank
			}
		}
	}
	out := make([]rrfEntry, 0, len(scores))
	for id, sc := range scores {
		out = append(out, rrfEntry{
			EntryID:    id,
			Score:      sc,
			TextRank:   textRanks[id],
			VectorRank: vectorRanks[id],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].EntryID < out[j].EntryID
	})
	return out
}

// RetrieveMemory 在给定 scope 内做一次混合检索。
//
// 错误语义：
//   - scope 无效 ⇒ ErrMemoryScopeRequired（未 scope 的读写一律拒绝）。
//   - 索引表读不出来 ⇒ 报错（fail-closed）：宁可不检索，也不给一份看似完整的
//     空上下文。
//   - 向量通道不可用 ⇒ **不报错**，记一条降级，继续走全文通道。
func (d *DB) RetrieveMemory(q MemoryRetrievalQuery) (*MemoryRetrievalResult, error) {
	if d == nil {
		return nil, errors.New("数据库未连接")
	}
	if !q.Scope.Valid() {
		return nil, ErrMemoryScopeRequired
	}
	if err := d.ensureMemorySchema(); err != nil {
		return nil, fmt.Errorf("应用记忆 schema 失败: %w", err)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = MemoryRetrievalDefaultLimit
	}
	if limit > MemoryRetrievalMaxLimit {
		limit = MemoryRetrievalMaxLimit
	}
	candidateLimit := q.CandidateLimit
	if candidateLimit <= 0 {
		candidateLimit = MemoryRetrievalDefaultCandidateLimit
	}

	res := &MemoryRetrievalResult{
		Scope:          q.Scope,
		RRFConstant:    MemoryRRFConstant,
		CandidateLimit: candidateLimit,
		Channels:       []string{},
		Degradations:   []*MemoryRetrievalDegradation{},
		Hits:           []*MemoryRetrievalHit{},
	}

	total, err := d.memoryScopeTotal(q.Scope)
	if err != nil {
		return nil, err
	}
	res.ScopeTotal = total

	lists := make([]rrfList, 0, 2)

	// ---- 全文通道 ----
	if strings.TrimSpace(q.Text) != "" {
		ranks, err := d.memoryTextRanks(q.Scope, q.Text, candidateLimit)
		if err != nil {
			return nil, err
		}
		if len(ranks) > 0 {
			lists = append(lists, rrfList{name: "text", ranks: ranks})
			res.Channels = append(res.Channels, "text")
		}
	}

	// ---- 向量通道（含显式降级） ----
	if len(q.Embedding) == 0 {
		res.Degradations = append(res.Degradations, &MemoryRetrievalDegradation{
			Code:    MemoryDegradationNoQueryEmbedding,
			Message: "未提供查询向量：向量通道未执行（本次是纯全文检索）",
		})
	} else {
		vectorRanks, degs, err := d.memoryVectorRanks(q.Scope, q.Embedding, candidateLimit)
		if err != nil {
			return nil, err
		}
		res.Degradations = append(res.Degradations, degs...)
		if len(vectorRanks) > 0 {
			lists = append(lists, rrfList{name: "vector", ranks: vectorRanks})
			res.Channels = append(res.Channels, "vector")
		}
	}

	// ---- 融合 ----
	fused := fuseRRF(lists, MemoryRRFConstant)
	res.RRFFused = len(lists) > 1
	res.Matched = len(fused)

	// ---- 取元数据 ----
	ids := make([]int64, 0, len(fused))
	for _, f := range fused {
		ids = append(ids, f.EntryID)
	}
	entries, err := d.memoryEntriesByIDs(ids)
	if err != nil {
		return nil, err
	}
	for _, f := range fused {
		e, ok := entries[f.EntryID]
		if !ok {
			// 索引行在检索与取元数据之间被退役了（valid_to 被闭合）⇒ 跳过它，
			// 而不是拿一份过期内容进上下文。
			continue
		}
		res.Hits = append(res.Hits, &MemoryRetrievalHit{
			EntryID:     e.id,
			TopicKey:    e.topic_key,
			Title:       e.title,
			SummaryLine: e.summary_line,
			Kind:        e.kind,
			Version:     e.version,
			WrittenBy:   e.written_by,
			Score:       f.Score,
			TextRank:    f.TextRank,
			VectorRank:  f.VectorRank,
		})
		if len(res.Hits) >= limit {
			break
		}
	}
	res.Returned = len(res.Hits)
	res.Truncated = res.Matched > res.Returned
	if res.ScopeTotal > 0 {
		res.Coverage = float64(res.Returned) / float64(res.ScopeTotal)
	}
	res.Grade, res.GradeReason = gradeMemoryRetrieval(res.ScopeTotal, res.Returned)
	return res, nil
}

// gradeMemoryRetrieval 给检索上下文定级。
//
// 规则（写死，纯函数）：
//
//	scope 为空          ⇒ INSUFFICIENT（empty_scope）—— 没有记忆可谈；
//	一条都没命中        ⇒ INSUFFICIENT（no_match）—— 拿它回答问题等于没依据；
//	返回数 ≥ scope 总数 ⇒ COMPLETE（full_coverage）—— 整个 scope 都在上下文里；
//	否则                ⇒ PARTIAL（partial_coverage）—— 命中了一部分，scope 里
//	                       还有没进上下文的东西。
//
// ⚠️ PARTIAL **绝不能**被当成 COMPLETE 交出去 —— 这是这一层的全部意义。
//
//	调用方要更严的门槛（比如「少于 30% 覆盖就不算有依据」）可以自己再判一层，
//	但拿到的 matched / scope_total / coverage 三个数字够它判。
func gradeMemoryRetrieval(scopeTotal, returned int) (string, string) {
	switch {
	case scopeTotal <= 0:
		return MemoryGradeInsufficient, "empty_scope"
	case returned <= 0:
		return MemoryGradeInsufficient, "no_match"
	case returned >= scopeTotal:
		return MemoryGradeComplete, "full_coverage"
	default:
		return MemoryGradePartial, "partial_coverage"
	}
}

// memoryScopeTotal 数 scope 里的当前条目数。
func (d *DB) memoryScopeTotal(scope MemoryScope) (int, error) {
	var n int
	if err := d.QueryRow(memoryScopeTotalSQL, scope.Type, scope.ID).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计记忆索引条目数失败: %w", err)
	}
	return n, nil
}

// memoryTextRanks 跑全文通道，返回「条目 id → 名次」。
func (d *DB) memoryTextRanks(scope MemoryScope, text string, limit int) (map[int64]int, error) {
	rows, err := d.Query(memoryTextRetrievalSQL, scope.Type, scope.ID, text, limit)
	if err != nil {
		return nil, fmt.Errorf("全文检索失败: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]int, 16)
	for rows.Next() {
		var (
			id int64
			rk int
		)
		if err := rows.Scan(&id, &rk); err != nil {
			return nil, fmt.Errorf("全文检索失败: %w", err)
		}
		out[id] = rk
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("全文检索失败: %w", err)
	}
	return out, nil
}

// memoryVectorRanks 跑向量通道，返回「条目 id → 名次」与降级说明。
//
// 降级的四种来源（每一种都**说清楚**）：
//   - pgvector 扩展没装；
//   - 向量表建不出来（运行期没有 DDL 权限）；
//   - 这个 scope 里一条向量都没有；
//   - 查询向量维度与列维度对不上。
func (d *DB) memoryVectorRanks(scope MemoryScope, embedding []float32, limit int) (map[int64]int, []*MemoryRetrievalDegradation, error) {
	degs := []*MemoryRetrievalDegradation{}
	available, err := d.memoryVectorAvailable()
	if err != nil {
		return nil, nil, err
	}
	if !available {
		degs = append(degs, &MemoryRetrievalDegradation{
			Code:    MemoryDegradationNoVectorExtension,
			Message: "目标 Postgres 没有 pgvector 扩展：向量通道未执行（本次是纯全文检索）",
		})
		return nil, degs, nil
	}
	if err := d.ensureMemoryRetrievalSchema(); err != nil {
		degs = append(degs, &MemoryRetrievalDegradation{
			Code:    MemoryDegradationNoVectorStore,
			Message: "向量索引不可用（" + err.Error() + "）：向量通道未执行（本次是纯全文检索）",
		})
		return nil, degs, nil
	}
	dim, err := d.memoryVectorDimension()
	if err != nil {
		return nil, nil, err
	}
	if dim > 0 && len(embedding) != dim {
		degs = append(degs, &MemoryRetrievalDegradation{
			Code:    MemoryDegradationVectorDimensionMismatch,
			Message: fmt.Sprintf("查询向量维度 %d 与向量列维度 %d 不一致：向量通道未执行", len(embedding), dim),
		})
		return nil, degs, nil
	}
	var n int
	if err := d.QueryRow(memoryVectorCountSQL, scope.Type, scope.ID).Scan(&n); err != nil {
		degs = append(degs, &MemoryRetrievalDegradation{
			Code:    MemoryDegradationNoVectorStore,
			Message: "读向量索引失败（" + err.Error() + "）：向量通道未执行（本次是纯全文检索）",
		})
		return nil, degs, nil
	}
	if n == 0 {
		degs = append(degs, &MemoryRetrievalDegradation{
			Code:    MemoryDegradationNoVectorEntries,
			Message: "该 scope 没有任何记忆向量：向量通道未执行（本次是纯全文检索）",
		})
		return nil, degs, nil
	}
	vec, err := encodeVectorLiteral(embedding)
	if err != nil {
		return nil, nil, err
	}
	rows, err := d.Query(memoryVectorRetrievalSQL, scope.Type, scope.ID, vec, limit)
	if err != nil {
		return nil, nil, fmt.Errorf("向量检索失败: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]int, 16)
	for rows.Next() {
		var (
			id int64
			rk int
		)
		if err := rows.Scan(&id, &rk); err != nil {
			return nil, nil, fmt.Errorf("向量检索失败: %w", err)
		}
		out[id] = rk
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("向量检索失败: %w", err)
	}
	return out, degs, nil
}

// memoryVectorAvailable 报告 pgvector 是否可用（扩展已装且类型可解析）。
func (d *DB) memoryVectorAvailable() (bool, error) {
	var ok bool
	err := d.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector')
 AND to_regtype('vector') IS NOT NULL`).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("探测 pgvector 可用性失败: %w", err)
	}
	return ok, nil
}

// memoryVectorDimension 读向量列的实际维度；读不到返回 0（表示不校验）。
func (d *DB) memoryVectorDimension() (int, error) {
	var typ string
	err := d.QueryRow(memoryVectorDimSQL).Scan(&typ)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("读向量列维度失败: %w", err)
	}
	// format_type 给的是 'vector(768)'。
	dim := 0
	fmt.Sscanf(typ, "vector(%d)", &dim)
	return dim, nil
}

// memoryEntryMeta 是取元数据时的中间形状。
type memoryEntryMeta struct {
	id           int64
	topic_key    string
	version      int
	kind         string
	title        string
	summary_line string
	written_by   string
	valid_from   time.Time
}

// memoryEntriesByIDs 按 id 取索引行（只取当前版本）。
func (d *DB) memoryEntriesByIDs(ids []int64) (map[int64]memoryEntryMeta, error) {
	out := make(map[int64]memoryEntryMeta, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := d.Query(memoryEntriesByIDsSQL, ids)
	if err != nil {
		return nil, fmt.Errorf("读记忆索引元数据失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m memoryEntryMeta
		if err := rows.Scan(&m.id, &m.topic_key, &m.version, &m.kind, &m.title,
			&m.summary_line, &m.written_by, &m.valid_from); err != nil {
			return nil, fmt.Errorf("读记忆索引元数据失败: %w", err)
		}
		out[m.id] = m
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读记忆索引元数据失败: %w", err)
	}
	return out, nil
}

// encodeVectorLiteral 把 float32 向量编码成 pgvector 的文本字面量 '[1,2,3]'。
//
// 为什么自己编码而不用 pgx 的类型映射：pgvector 的 Go 类型不在本仓库的依赖里
// （目标 Postgres 大概率也没装扩展）⇒ 不引入一个只为「也许有一天会装」的依赖。
// 编码必须精确：维度对不上由 memoryVectorDimension 校验，不是靠截断。
func encodeVectorLiteral(v []float32) (string, error) {
	if len(v) == 0 {
		return "[]", nil
	}
	parts := make([]string, 0, len(v))
	for i, f := range v {
		if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
			return "", fmt.Errorf("查询向量第 %d 位不是有限数（NaN/Inf）", i)
		}
		parts = append(parts, formatFloat32(f))
	}
	return "[" + strings.Join(parts, ",") + "]", nil
}

// formatFloat32 用最短往返精度渲染一个 float32。
func formatFloat32(f float32) string {
	b, err := json.Marshal(f)
	if err != nil {
		return "0"
	}
	return string(b)
}
