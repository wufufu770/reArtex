package guard

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/hook"
)

// openTestDB 打开本包的派生测试库；连不上就跳过需要库的用例。
func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("无测试库（ARTEX_PG_DSN 未设置）")
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Skipf("打开测试库失败: %v", err)
	}
	return d
}

// allow 档必须留痕：平台工具调用量小，进审计链无压力；
// 没有留痕就回答不了「谁在何时建过什么」。
func TestAllowTierIsAudited(t *testing.T) {
	d := openTestDB(t)
	ic := intercept.New(d)
	g := NewWithInterceptor(ic)

	ev := hook.Event{ToolName: "create_skill", Input: json.RawMessage(`{"name":"x"}`)}
	res := g.applyIntercept(context.Background(), ev)
	if res.Decision == "block" {
		t.Fatalf("create_skill 不应被拦，得到 %+v", res)
	}

	var action, tool string
	err := d.QueryRow(`SELECT action, tool_name FROM audit_events WHERE tool_name='create_skill' ORDER BY seq DESC LIMIT 1`).
		Scan(&action, &tool)
	if err != nil {
		t.Fatalf("allow 档未落审计链: %v", err)
	}
	if action != "allow" || tool != "create_skill" {
		t.Fatalf("审计行不对：action=%q tool=%q", action, tool)
	}
}

// 显式放行同样要能在审批记录页看到（decision_source 标为代码常量短路表）。
func TestAllowTierIsVisibleInInterceptHistory(t *testing.T) {
	d := openTestDB(t)
	ic := intercept.New(d)
	g := NewWithInterceptor(ic)

	ev := hook.Event{ToolName: "create_skill", Input: json.RawMessage(`{"name":"y"}`)}
	g.applyIntercept(context.Background(), ev)

	var source string
	err := d.QueryRow(`SELECT decision_source FROM intercept_pending WHERE tool_name='create_skill' ORDER BY id DESC LIMIT 1`).
		Scan(&source)
	if err != nil {
		t.Fatalf("allow 档未落 intercept_pending: %v", err)
	}
	if source != "shortcircuit" {
		t.Fatalf("来源应为 shortcircuit，得到 %q", source)
	}
}
