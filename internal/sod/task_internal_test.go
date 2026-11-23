package sod

import "testing"

// TestTaskInternalWriteUnwrapsDeferred 钉住 0.15.50 空目标误拒修复的识别面：
// 延迟工具（ExecuteExtraTool 外层）按解包后的内层名查任务内表；平台内部
// 数据面工具（目标是任务/图/账本引用而非主机）命中；扩权/以主机为真实目标的
// 工具绝不命中（照旧进规整过 scope）。
func TestTaskInternalWriteUnwrapsDeferred(t *testing.T) {
	// 内建等待/跨任务读：直接名命中。
	for _, name := range []string{"sleep", "TaskOutput", "record_fact", "add_hint", "report_finding"} {
		if !TaskInternalWrite(name) {
			t.Errorf("%q 应在任务内表（空目标 fail-closed 会误拒它）", name)
		}
	}
	// 0.15.52 偏锋数据面 + finding 报告写侧（生产实锤被误拒类：edge_consult
	// 只带 task_id 被拒 ×2、update_finding_report 靠载荷里恰好有主机名才过）。
	for _, name := range []string{"edge_consult", "edge_coverage", "edge_methodologies", "edge_init_cells", "edge_dispatch", "edge_switch", "update_finding_report"} {
		if !TaskInternalWrite(name) {
			t.Errorf("%q 应在任务内表（目标是任务/图/账本引用而非主机）", name)
		}
		p := []byte(`{"tool_name": "` + name + `", "params": {}}`)
		if !TaskInternalWrite(deferredInnerName("ExecuteExtraTool", p)) {
			t.Errorf("延迟形态内层 %q 应命中任务内表", name)
		}
	}
	// 延迟形态：外层名不命中，解包后的内层名命中（0.15.50 实测被拒的三件）。
	payload := []byte(`{"tool_name": "record_cell", "params": {}}`)
	if TaskInternalWrite("ExecuteExtraTool") {
		t.Fatal("外层名 ExecuteExtraTool 本身不得命中（内层可能是网络工具）")
	}
	inner := deferredInnerName("ExecuteExtraTool", payload)
	if inner != "record_cell" {
		t.Fatalf("解包内层名 = %q，期望 record_cell", inner)
	}
	if !TaskInternalWrite(inner) {
		t.Fatal("ExecuteExtraTool 内层 record_cell 应命中任务内表")
	}
	for _, n := range []string{"graph_overview", "list_facts", "upsert_biz_node", "biz_adjudicate", "list_assets"} {
		p := []byte(`{"tool_name": "` + n + `", "params": {}}`)
		if !TaskInternalWrite(deferredInnerName("ExecuteExtraTool", p)) {
			t.Errorf("内层 %q 应命中任务内表", n)
		}
	}
	// 扩权/主机目标工具绝不命中。
	for _, name := range []string{"add_task_scope", "add_company_scope", "insert_assets", "create_mcp", "create_custom_tool", "create_skill", "delete_assets_by_host", "Bash", "WebFetch"} {
		if TaskInternalWrite(name) {
			t.Errorf("%q 不得在任务内表（扩权/主机目标工具照旧过 scope）", name)
		}
	}
	// 偏锋生命周期工具绝不命中（0.15.52）：收尾/建任务/切模式照旧进规整过闸
	// ——worker 不得经延迟转发直收运行、建任务。
	for _, name := range []string{"edge_conclude", "edge_run", "edge_activate"} {
		if TaskInternalWrite(name) {
			t.Errorf("%q 不得在任务内表（生命周期动作照旧过 scope）", name)
		}
	}
	// 内层是网络工具的延迟调用：解包后也不命中。
	bad := []byte(`{"tool_name": "browser_navigate", "params": {}}`)
	if TaskInternalWrite(deferredInnerName("ExecuteExtraTool", bad)) {
		t.Fatal("内层网络工具不得命中任务内表")
	}
}
