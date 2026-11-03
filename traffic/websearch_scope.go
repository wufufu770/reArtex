package traffic

// web_search 的出网目的地治理（方案书 §9.1 第 9 条 / §6.5 第 1 条）。
//
// 事实：web_search 走自己的出口代理（socks5 / 全局代理），不是录制代理 ⇒
// 请求既不装 netguard 的拨号守卫，也不带任务身份 ⇒ 完全不受任务范围校验。
//
// 治理办法不是「让它受任务范围校验」：搜索服务商的域名不在任务的资产范围里，
// 范围判定会把它拒掉 ⇒ 工具直接不可用（§6.3 已确认这条路走不通）。而是引入
// 【范围豁免清单】（internal/sodpolicy 的代码常量，不是 DB 行、不是 settings 项）：
//
//	目的地在清单内   ⇒ 平台基础设施（搜索服务商 / 模型 API / 镜像仓库），
//	                   不受任务范围管辖（exempt）；
//	目的地不在清单内 ⇒ 仍然放行（工具不能坏），但按【未归属出网】留痕；
//	判不出目的地     ⇒ fail-closed（不放行）：不知道请求要去哪，就不能放行。
//
// 清单本身住在 internal/sodpolicy 是有意的：豁免清单是安全边界的一部分，必须与
// 判定代码一起改、一起审、一起发布；放进可热改的 DB/settings 就等于「由被它
// 约束的人来调边界」。

import (
	"context"
	"encoding/json"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/sodpolicy"
	actool "github.com/Autumn-27/norma/tool"
)

// webSearchAuditActor 是 web_search 出网留痕的署名（与录制代理的
// "traffic-proxy" 区分开：这条出网压根没经过录制代理）。
const webSearchAuditActor = "web_search"

// EgressAuditWriter 写出网治理的判定留痕。
//
// *db.DB 满足它（db.AuditEvent + AppendAuditEvent）；留成接口是为了让判定器
// 能在没有数据库的测试里被穷举（判定的分支不该依赖库）。
type EgressAuditWriter interface {
	AppendAuditEvent(ev db.AuditEvent) (string, error)
}

// WebSearchScope 判定并留痕 web_search 的出网目的地。
//
// 它是纯判定 + 留痕，不发请求：真正的拦截点在 vendored SDK 的 web_search
// 工具里（patches/norma-websearch-egress-scope.patch 加的钩子），钩子在请求
// 发出【之前】调用本类型。
type WebSearchScope struct {
	writer EgressAuditWriter
}

// NewWebSearchScope 造一个 web_search 目的地判定器。writer 为 nil 时仍然判定，
// 只是不留痕（用于没有数据库的场合）。
func NewWebSearchScope(writer EgressAuditWriter) *WebSearchScope {
	return &WebSearchScope{writer: writer}
}

// Classify 判定一次 web_search 目的地：返回（是否放行, 原因码）。
//
// backend 是后端名（ddgs / brave-free / tavily / deepseek），只进留痕载荷。
//
// ctx 目前不用：留痕写面（db.DB.AppendAuditEvent）不接 ctx。参数留着是为了与
// SDK 钩子的签名一致（钩子只能拿到 ctx），将来换带 ctx 的写面时不必改签名。
func (s *WebSearchScope) Classify(ctx context.Context, host, backend string) (bool, string) {
	_ = ctx
	verdict := sodpolicy.ClassifyEgressScopeHost(host)
	switch verdict.Reason {
	case sodpolicy.ReasonEgressScopeExempt:
		// 平台基础设施目的地：预期内的出网，不留痕。
		//
		// 为什么 exempt 不留痕：审计链只记「判定动作」（见 db/audit_chain.go 的
		// 包注释）；而每次搜索都记一条会把链淹掉，真正需要追责的判定反而被埋掉。
		return true, verdict.Reason
	case sodpolicy.ReasonEgressScopeUnattributed:
		// 清单外：放行（工具不能坏），但按「未归属出网」留痕 —— 这条出网不受
		// 任务范围校验，代价写进审计链而不是藏起来。
		s.record(verdict, backend)
		return true, verdict.Reason
	default:
		// 判不出目的地主机 ⇒ fail-closed。
		s.record(verdict, backend)
		return false, verdict.Reason
	}
}

// record 把一次判定写进平台的审计链。
func (s *WebSearchScope) record(verdict sodpolicy.EgressScopeVerdict, backend string) {
	if s == nil || s.writer == nil {
		return
	}
	action := "allow_unattributed"
	if verdict.Reason == sodpolicy.ReasonEgressScopeUnresolved {
		action = "deny"
	}
	payload, err := json.Marshal(map[string]any{
		"host":       verdict.Host,
		"backend":    backend,
		"reason":     verdict.Reason,
		"attributed": false,
	})
	if err != nil {
		payload = []byte("{}")
	}
	// 留痕失败不改变判定结论：这条链记的是「发生过一次未归属出网」，写不进去
	// 是审计面的问题，不能反过来把搜索工具弄坏（工具坏了会掩盖这次出网本身）。
	_, _ = s.writer.AppendAuditEvent(db.AuditEvent{
		Actor:    webSearchAuditActor,
		ToolName: "egress",
		Action:   action,
		Reason:   verdict.Reason,
		Payload:  payload,
	})
}

// InstallWebSearchScope 把 web_search 的目的地判定装进 vendored SDK。
//
// 装的是**进程级**钩子（norma/tool.SetWebSearchEgressScope）：web_search 工具
// 是 SDK 在 session 装配时创建的，宿主若要逐 session 传回调，就得改四个 agent
// 角色的 Options —— 而判据是全局的（豁免清单是代码常量），逐 session 传没有
// 意义，反而多出四处可能漏接的调用点。
//
// 传 nil 表示卸下钩子（回到「不做目的地判定」）。
func InstallWebSearchScope(scope *WebSearchScope) {
	if scope == nil {
		actool.SetWebSearchEgressScope(nil)
		return
	}
	actool.SetWebSearchEgressScope(scope.Classify)
}
