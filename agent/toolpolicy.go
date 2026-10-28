package agent

import (
	"context"
	"encoding/json"

	"github.com/Autumn-27/artex/pathguard"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// policyTool 在工具执行前施加平台侧安全策略。与 guard/shortcircuit.go 的分工：
// 短路表按工具名与命令字面量判「该不该做」，本装饰器按解析后的真实参数判「能不能
// 够得着」。前者拦已知命令，后者补上真实路径解析（软链、.. 展开）这一层。
type policyTool struct{ actool.CoreTool }

// CheckPermissions 命中平台控制面秘密时返回 Denied。
//
// 必须硬 Deny 而非 AskUser：ARTEX 六个 agent 会话全部跑 permission.ModeBypass
// （chat/goals/mainagent/planner/provider/worker），而 norma 的 Evaluate 在 Stage 2
// 对 ModeBypass 直接 return Allowed()，AskUser 在该模式下等于放行。只有 Stage 1
// 生效，而 Deny 恰好落在 Stage 1（工具自身决策优先于 mode 短路），任何权限模式都
// 绕不过。
func (p *policyTool) CheckPermissions(ctx context.Context, input json.RawMessage, pc acperm.Context) acperm.Decision {
	if err := pathguard.Check(p.Name(), input); err != nil {
		return acperm.Denied(err.Error())
	}
	// 入参路径片段：相对工具工作目录解析真实路径后判定。短路表是字符串预筛，
	// 挡不住 `cat $(realpath ../../../jwt.key)` 这类经 shell 展开才现形的路径。
	if err := pathguard.CheckInputPaths(p.Name(), input, pc.WorkingDir); err != nil {
		return acperm.Denied(err.Error())
	}
	return p.CoreTool.CheckPermissions(ctx, input, pc)
}

func (p *policyTool) policyApplied() {}

type policyApplied interface{ policyApplied() }

// IsPolicyWrapped 供装配完整性测试断言用：每个 agent 的最终工具表里，
// 每个工具都应实现该标记接口。
func IsPolicyWrapped(t actool.CoreTool) bool {
	_, ok := t.(policyApplied)
	return ok
}

func enforceToolPolicy(t actool.CoreTool) actool.CoreTool { return &policyTool{CoreTool: t} }
