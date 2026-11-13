package intercept

// 延迟工具的治理解包（0.11.9，桌面控制施工 §四-G）。
//
// 背景：skill 门控的延迟 MCP 工具经 ExecuteExtraTool 转发——模型调用的是外层的
// ExecuteExtraTool，其载荷携带内层工具名与参数；内层工具的 Permissions 无条件
// 放行（third_party/norma/tool/deferred.go），且外层名不含 mcp__ 前缀。不解包的话，
// 延迟调用在拦截系统里整体失明：启用名单按外层名查不到、规则按外层名匹配不上。
//
// 解包的保真性：ExecuteExtraTool.Run 执行的就是载荷 tool_name 指向的工具（空名与
// 未解锁由它自己拒绝）⇒ 解包得到的内层名就是【实际执行的工具】，治理看到的与
// 执行的恒等，无旁路。载荷畸形（解析失败/空名）时回退外层名——那类载荷在转发器
// 里同样会被拒绝，治理放行与执行拒绝殊途同归，不会出现「治理放过、执行也放过
// 未登记工具」的窗口。

import (
	"encoding/json"
	"strings"
)

// DeferredExecutorTool 是延迟工具转发器的注册名（norma tool.DeferredExecutorToolName）。
// 常量在本包重复一份：intercept 是叶子包，不 import norma。
const DeferredExecutorTool = "ExecuteExtraTool"

// UnwrapDeferredName 返回规则求值应使用的【有效工具名与有效入参】。
//   - 非转发器调用：原样返回（直连工具与今天完全一致）；
//   - 转发器调用：取载荷 tool_name 为有效名、params 为有效入参；
//   - 载荷畸形/空名：回退原值（执行侧会拒绝同一载荷，见上）。
//
// 调用方（guard 的 applyIntercept）把返回值用于 IsToolEnabled / Match / Judge /
// Log / HandleAsk 全链——审计与审批记录里看到的是【实际执行的工具名与参数】。
func UnwrapDeferredName(tool string, input json.RawMessage) (string, json.RawMessage) {
	if !strings.EqualFold(tool, DeferredExecutorTool) {
		return tool, input
	}
	var probe struct {
		ToolName string          `json:"tool_name"`
		Params   json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(input, &probe); err != nil || strings.TrimSpace(probe.ToolName) == "" {
		return tool, input
	}
	params := probe.Params
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}
	return probe.ToolName, params
}
