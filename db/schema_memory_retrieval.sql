-- 记忆检索（方案 B 的混合检索面）—— **与 db/schema.sql 并行应用**的幂等 DDL。
--
-- 为什么单独成文件：db/schema.sql 由别的单元在同一 worktree 内并行开发，本单元
-- 不改它。本文件由 db/memory_retrieval.go 的 EnsureMemoryRetrievalSchema 幂等
-- 补齐（见该文件注释），schema 属主可以随时把内容并进 db/schema.sql —— 两边都是
-- 幂等的，重复应用无副作用。
--
-- 设计依据（session-groupchat-proposal.md §10.4 方案 B / §10.5 第 7、9 条）：
--
--   ① **全文通道不需要这张表**：tsvector 由 memory_index 的行内文本**现算**
--      （db/memory_retrieval.go 的 SQL）。索引常驻上下文，不需要第二份拷贝 ⇒
--      没有「索引与检索结果不一致」这个新的失效面。
--   ② **向量通道需要一张表**：embedding 与索引行分开存（向量是稠密的、维度固定、
--      要能被 ANN 索引），且**只在 pgvector 可用时**才存在。
--   ③ **pgvector 不可用不是错误**：整张表连同它的索引一起跳过（DO 块里 RETURN），
--      检索显式降级为纯全文并在结果里**说清楚降级了**（§10.5 第 7 条：混合检索 +
--      确定性融合；降级必须可观测，不能假装两路都跑了）。
--
-- ⚠️ 本文件**不含任何阈值列**：候选条数、融合常数、返回条数全部是
-- db/memory_retrieval.go 的代码常量（§8 红线第 3 条）。

-- ---------- memory_embedding：向量通道的存储 ----------
--
-- 为什么表名带前缀：它是 memory_index 的**派生**数据（稠密向量 + ANN 索引），
-- 删掉 memory_index 的行会级联删掉它（ON DELETE CASCADE）⇒ 不会留下孤儿向量。
--
-- ⚠️ vector(768) 的维度是**代码常量** db.MemoryEmbeddingDim 的镜像。
--    改维度要同时改两处（这里与 Go 常量）—— 写入侧会按实际列维度校验长度，
--    对不上直接报错，不会静默截断。
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector') THEN
        RAISE NOTICE 'pgvector 扩展不可用：跳过记忆向量表（检索降级为纯全文通道）';
        RETURN;
    END IF;

    EXECUTE 'CREATE TABLE IF NOT EXISTS memory_embedding (
        entry_id   BIGINT PRIMARY KEY REFERENCES memory_index(id) ON DELETE CASCADE,
        scope_type TEXT NOT NULL,
        scope_id   BIGINT NOT NULL,
        embedding  vector(768) NOT NULL,
        updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
    )';

    EXECUTE 'CREATE INDEX IF NOT EXISTS idx_memory_embedding_scope
        ON memory_embedding(scope_type, scope_id)';

    -- ANN 索引：HNSW（pgvector 0.5+）。建不出来不致命（只是退化成顺序扫描），
    -- 所以用 EXCEPTION 兜住，不让「没有 ANN 索引」变成「整个检索不可用」。
    BEGIN
        EXECUTE 'CREATE INDEX IF NOT EXISTS idx_memory_embedding_hnsw
            ON memory_embedding USING hnsw (embedding vector_cosine_ops)';
    EXCEPTION WHEN OTHERS THEN
        RAISE NOTICE '建 HNSW 向量索引失败（不影响正确性，只是检索变慢）：%', SQLERRM;
    END;
END $$;
