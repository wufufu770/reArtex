-- A-10（0.15.76）：拦截规则按工具限定作用域。
--
-- tool_scope：规则只对列出的工具生效；空数组 = 全局（存量规则语义不变，
-- 不静默收窄——这是 A-10 的硬口径：升级不改变既有规则行为）。
-- 组匹配用前缀约定（"shell:*"），不依赖工具命名是否存在分组前缀。
--
-- 为什么不在 db/schema.sql 里改：schema.sql 是保留文件（go:embed 基线），
-- 存量表的演进一律走本文件的幂等 ALTER（IF NOT EXISTS），启动期由
-- EnsureInterceptToolScopeSchema 用迁移连接应用（受限运行角色无 DDL 权限，
-- 见 db/db.go 迁移块与 db/role.go markRuntimeSchemasReady 的 v0.9.16 事故记录）。

ALTER TABLE intercept_rules ADD COLUMN IF NOT EXISTS tool_scope TEXT[] NOT NULL DEFAULT '{}';

CREATE INDEX IF NOT EXISTS idx_intercept_rules_tool_scope ON intercept_rules USING GIN (tool_scope);
