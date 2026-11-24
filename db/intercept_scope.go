package db

// A-10（0.15.76）：拦截规则的 tool_scope 列（按工具限定规则作用域）。
//
// intercept_rules 是 schema.sql 基线表，列演进走独立幂等 DDL 文件 +
// EnsureInterceptToolScopeSchema（模式同 db/reports.go）。启动迁移块应用一次、
// markRuntimeSchemasReady 打就位标记，运行期懒执行变空操作（受限角色无 DDL
// 权限——v0.9.16 事故的既定修法）。

import (
	_ "embed"
	"fmt"
	"sync"
)

//go:embed schema_intercept_scope.sql
var interceptScopeSchemaSQL string

// interceptScopeSchemaReady 记录某个连接已经应用过 tool_scope DDL。
var interceptScopeSchemaReady sync.Map

// EnsureInterceptToolScopeSchema 应用 tool_scope 列 DDL（幂等）。
func (d *DB) EnsureInterceptToolScopeSchema() error {
	if _, ok := interceptScopeSchemaReady.Load(d.DB); ok {
		return nil
	}
	if _, err := d.Exec(interceptScopeSchemaSQL); err != nil {
		return fmt.Errorf("应用拦截规则 tool_scope schema 失败: %w", err)
	}
	interceptScopeSchemaReady.Store(d.DB, struct{}{})
	return nil
}
