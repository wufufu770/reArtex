package agent_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
)

// 本文件是【Tier 1 启用的唯一前置验收测试】。
//
// Tier 1 = 「安全只读工具自动通过」。它只有在读面【真的】按任务范围隔离时才成立：
// 若 list_assets / get_finding_traffic / 图读这类工具还能把范围外的内容端出来，
// 「Tier 1 自动通过」就等于「自动放行这次泄露」——模型不用申请任何权限就把别人任务
// 的侦察结论读走了。
//
// 为什么测试清单【从 agent.Tier1ReadOnlyTools 派生】而不是在本文件里再抄一份：
// 抄一份就等于出现第二个真相源，此后「清单说安全」与「读面真的隔离」可以各说各话，
// 而没有任何东西会变红。派生之后，【任何人往 Tier 1 里加一个还没做范围隔离的工具，
// 这道测试立刻失败】⇒ Tier 1 拿不到「可以启用」的结论。这正是
// /home/analyst/.omo/plans/2026-10-29-auto-permission-tiers.md §1 里
// 「Tier 1 的启用绑定读面范围隔离验收测试」这条硬要求的落地方式。
//
// 为什么用 package agent_test（外部测试包）：
// get_finding_traffic 这个工具由 **server** 包注册（server/finding_traffic.go 的
// (*Server).toolGetFindingTraffic），它的范围判定也发生在 server 包的
// (*Server).agentFindingTrafficAccess 里。server → agent 是既有依赖方向，反过来会成
// 导入环 —— agent 的内部测试包（package agent）因此【够不到】这个工具。
// 外部测试包（package agent_test）是 Go 为「打破这种环」留的口子：它可以 import
// server，而 server 不会 import agent_test。
//
// 为什么这道测试【不是】只测 db 层：db 层的范围判定（AssetStore.ScopeFragmentFor、
// ExplorationStore.ListByKindScoped）已经被 db/scope_test.go、db/exploration_scope_test.go
// 钉住了；这道门要钉的是【工具面真的用上了那些判定】——判定做对了、工具没接上，
// 对 Tier 1 来说同样等于泄露。所以这里一律在【工具的真实输出】上断言。

// tier1ScopeProbe 是一个 Tier 1 工具的范围隔离用例。
type tier1ScopeProbe struct {
	// outOfScope 用【范围外】的对象调用该工具，返回工具的原始输出。
	outOfScope func(t *testing.T, fx *tier1Fixture) string
	// inScope 用【范围内】的对象调用同一个工具，返回工具的原始输出。
	inScope func(t *testing.T, fx *tier1Fixture) string
	// outMarker 是范围外对象的唯一标识：它绝不能出现在任何输出里。
	outMarker string
	// inMarker 是范围内对象的唯一标识：它必须出现在输出里。
	inMarker string
}

// tier1ScopeProbes 是每个 Tier 1 工具的范围隔离用例。
//
// 它是一张【按工具名索引的用例表】，不是第二份 Tier 1 清单：清单的唯一真相在
// agent/approval_policy.go 的 Tier1ReadOnlyTools。两边的关系由下面两条断言钉死：
// 清单里有而这张表里没有 ⇒ 失败（有人加了工具却没做隔离）；这张表里有而清单里没有
// ⇒ 失败（用例表退化成了第二份清单）。
func tier1ScopeProbes(fx *tier1Fixture) map[string]tier1ScopeProbe {
	return map[string]tier1ScopeProbe{
		// list_assets：按 id 直取。范围外的 id 必须【取不到】，且不能靠报错文案
		// 反推出那个资产存在。
		"list_assets": {
			outOfScope: func(t *testing.T, fx *tier1Fixture) string {
				return tier1Call(t, tier1Tool(t, fx.agentTools, "list_assets"), context.Background(),
					map[string]any{"id": fx.outAsset})
			},
			inScope: func(t *testing.T, fx *tier1Fixture) string {
				return tier1Call(t, tier1Tool(t, fx.agentTools, "list_assets"), context.Background(),
					map[string]any{"id": fx.inAsset})
			},
			outMarker: fx.outHost,
			inMarker:  fx.inHost,
		},

		// list_untested_assets（覆盖度/待测清单读面）：返回的是一整份清单，所以
		// 「范围外不可达」表现为清单里没有范围外的资产；「范围内可达」表现为清单里
		// 有范围内的资产（未被任何事实覆盖 ⇒ 仍在待测清单里）。
		"list_untested_assets": {
			outOfScope: func(t *testing.T, fx *tier1Fixture) string {
				return tier1Call(t, tier1Tool(t, fx.agentTools, "list_untested_assets"), context.Background(),
					map[string]any{"page_size": 100})
			},
			inScope: func(t *testing.T, fx *tier1Fixture) string {
				return tier1Call(t, tier1Tool(t, fx.agentTools, "list_untested_assets"), context.Background(),
					map[string]any{"page_size": 100})
			},
			outMarker: fx.outHost,
			inMarker:  fx.inHost,
		},

		// graph_overview（图读）：范围外的资产经由它【不可达】。
		//
		// 夹具走真实写路径：agent 自己就能用 record_fact 把一枚范围外的资产写进事实的
		// 锚点 —— agent/tools.go 的 recordOneFact 直接把 asset_ids 交给
		// db.ExplorationStore.AddNode 建锚点，没有范围判定（db 里为此备好的
		// AnchorInScope 还没被任何 agent 写路径用上）。于是「范围外的结论」真的会进到
		// 探索图里 —— 图读必须把它挡住：agent/tools.go 的 graphOverviewData 目前读事实
		// 用的是【无范围约束】的 ListByKind，db 里备好的 ListByKindScoped 同样还没被
		// agent 读路径用上。
		//
		// 这里【不】断言 record_fact 是否写成功：写侧将来若补上范围判定，这枚事实
		// 根本不会被建出来，图读自然也就无从泄露 —— 两种修法（写侧挡住 / 读侧过滤）
		// 都能让本用例变绿，这正是我们要的：门禁钉的是【范围外的资产不可达】这个
		// 结果，而不是钉某一种实现。
		"graph_overview": {
			outOfScope: func(t *testing.T, fx *tier1Fixture) string {
				tier1Call(t, tier1Tool(t, fx.agentTools, "record_fact"), context.Background(), map[string]any{
					"summary":   "范围外资产 " + fx.graphOutHost + " 上的结论",
					"asset_ids": []int64{fx.graphOutAsset},
				})
				return tier1Call(t, tier1Tool(t, fx.agentTools, "graph_overview"), context.Background(), map[string]any{})
			},
			inScope: func(t *testing.T, fx *tier1Fixture) string {
				tier1Call(t, tier1Tool(t, fx.agentTools, "record_fact"), context.Background(), map[string]any{
					"summary":   "范围内资产 " + fx.graphInHost + " 上的结论",
					"asset_ids": []int64{fx.graphInAsset},
				})
				return tier1Call(t, tier1Tool(t, fx.agentTools, "graph_overview"), context.Background(), map[string]any{})
			},
			outMarker: fx.graphOutHost,
			inMarker:  fx.graphInHost,
		},

		// list_findings（漏洞清单读面）：范围外的资产经由它【不可达】。
		//
		// 夹具里 B 名下的漏洞锚在 B 自己的范围资产上，它的内容对 A 是范围外的
		// 侦察结论；A 的漏洞清单不得把它带出来。A 名下那条锚在范围内资产上的漏洞
		// 必须照常出现 —— 不能把正常路径一起堵死。
		"list_findings": {
			outOfScope: func(t *testing.T, fx *tier1Fixture) string {
				return tier1Call(t, tier1Tool(t, fx.agentTools, "list_findings"), context.Background(), map[string]any{})
			},
			inScope: func(t *testing.T, fx *tier1Fixture) string {
				return tier1Call(t, tier1Tool(t, fx.agentTools, "list_findings"), context.Background(), map[string]any{})
			},
			outMarker: fx.outHost,
			inMarker:  fx.inHost,
		},

		// get_finding_traffic（漏洞流量证据读面）：范围判定在 server 包的
		// (*Server).agentFindingTrafficAccess —— 按【漏洞归属】判（findings.task_id），
		// 并在没有任务上下文时直接拒绝。
		//
		// 所以这里的「范围外对象」是 B 名下的漏洞：拿 A 的任务上下文去读 B 的漏洞必须
		// 被拒，且拒绝文案不得泄露该漏洞的任何细节（文案里也不能出现它的摘要）。
		"get_finding_traffic": {
			outOfScope: func(t *testing.T, fx *tier1Fixture) string {
				ctx := agent.WithRunInfo(context.Background(), agent.RunInfo{TaskID: fx.taskA})
				return tier1Call(t, tier1Tool(t, fx.serverTools, "get_finding_traffic"), ctx,
					map[string]any{"finding_id": fx.outFindingID})
			},
			inScope: func(t *testing.T, fx *tier1Fixture) string {
				ctx := agent.WithRunInfo(context.Background(), agent.RunInfo{TaskID: fx.taskA})
				return tier1Call(t, tier1Tool(t, fx.serverTools, "get_finding_traffic"), ctx,
					map[string]any{"finding_id": fx.inFindingID})
			},
			outMarker: fx.outHost,
			inMarker:  fmt.Sprintf(`"finding_id":"%d"`, fx.inFindingID),
		},
	}
}

// TestTier1ReadOnlyToolsNoScopeLeak 是 Tier 1 启用的唯一前置验收测试。
//
// 判定规则（两半都要成立）：
//  1. 范围外的对象经由每个 Tier 1 工具【不可达】——既不出现在结果里，也不出现在
//     错误文案里（拒绝本身不能变成存在性探测器）。
//  2. 范围内的对象经由同一个工具【仍可达】——范围隔离不能变成一刀切，否则正常的
//     任务会被自己的门禁锁死。
//
// 工具清单从 agent.Tier1ReadOnlyTools 派生（不是在本文件里抄一份）：清单里多一个
// 没有范围隔离证据的工具 ⇒ 本测试立刻失败 ⇒ Tier 1 拿不到「可以启用」的结论。
func TestTier1ReadOnlyToolsNoScopeLeak(t *testing.T) {
	fx := newTier1Fixture(t)
	probes := tier1ScopeProbes(fx)

	// ① 清单里的每个工具都必须有范围隔离用例。
	for _, key := range agent.Tier1ReadOnlyTools {
		if _, ok := probes[key]; !ok {
			t.Fatalf("Tier 1 工具 %q 没有范围隔离用例：把工具加进 Tier 1（agent/approval_policy.go 的 Tier1ReadOnlyTools）之前，"+
				"必须先证明它的读面范围隔离 —— 否则 Tier 1 等于自动放行这次泄露", key)
		}
	}
	// ② 反向：用例表不得变成第二份 Tier 1 清单。
	for key := range probes {
		if !agent.IsTier1ReadOnlyTool(key) {
			t.Fatalf("范围隔离用例 %q 不在 Tier 1 清单里：清单只有一处真相（agent/approval_policy.go 的 Tier1ReadOnlyTools），"+
				"用例表不能变成第二份", key)
		}
	}

	for _, key := range agent.Tier1ReadOnlyTools {
		key := key
		t.Run(key, func(t *testing.T) {
			probe := probes[key]

			out := probe.outOfScope(t, fx)
			if strings.Contains(out, probe.outMarker) {
				t.Fatalf("工具 %s 把范围外的内容（%s）带进了输出：范围隔离失效 ⇒ Tier 1 不能启用。输出：%s",
					key, probe.outMarker, out)
			}

			in := probe.inScope(t, fx)
			if !strings.Contains(in, probe.inMarker) {
				t.Fatalf("工具 %s 把范围内的内容（%s）也一起挡掉了：范围隔离不能变成一刀切，否则任务会被自己的门禁锁死。输出：%s",
					key, probe.inMarker, in)
			}
		})
	}
}
