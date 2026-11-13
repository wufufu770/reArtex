package server

// 小程序渗透工具面（0.12.0 施工 §四-B3/B-5）：token 池的作用域工具。
//
// 与 biz/edge 工具同一通道（server 侧参数化 + hostTools 装配 + tools 表绑定 +
// roledispatch 双闸）。三条纪律：
//   - token 明文只在「使用时刻」经显式 reveal 进入工具结果（构造 Burp 重放
//     replacement）；保存/清单面只见标签与指纹；
//   - task_id 必填（池按任务作用域绑定，无全局池——跨任务凭据走人工通道）；
//   - 保存动作走审计链（source=miniprogram-token），凭据值不进审计载荷。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	actool "github.com/Autumn-27/norma/tool"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// mpTools 返回小程序渗透工具全集。
func (s *Server) mpTools() []actool.CoreTool {
	return []actool.CoreTool{
		s.toolMpTokenSave(),
		s.toolMpTokenList(),
		s.toolMpTokenGet(),
	}
}

// seedMpTools 播种小程序工具 + 默认绑定（pentest/worker/auto/mainagent——阶段
// 执行在任务路径（worker）与会话驱动（pentest/auto/mainagent）两条线都要用）。
func (s *Server) seedMpTools() {
	const refreshFlag = "mp_tools_seed_v1"
	bindings, _ := json.Marshal([]string{"pentest", "worker", "auto", "mainagent", "miniprogram"})
	for _, t := range s.mpTools() {
		schema, _ := json.Marshal(t.InputSchema())
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, bindings)
	}
	if v, _, _ := s.m.pg.GetSetting(refreshFlag); v != "true" {
		_ = s.m.pg.SetSetting(refreshFlag, "true")
	}
	agent.InvalidateAssembly()
}

// toolMpTokenSave 保存/轮换 token（加密落库，transcript 只见指纹）。
func (s *Server) toolMpTokenSave() actool.CoreTool {
	return wrTool("mp_token_save",
		"保存/轮换一条小程序凭据到 token 池（按任务+应用+角色标签作用域绑定，落库即加密）。凭据值不进会话消息——直接把值作为参数传给本工具，返回的是指纹（sha256 前 16 位）；后续用 mp_token_list 看清单、mp_token_get(reveal=true) 在构造 Burp 重放 replacement 的使用时刻取明文。",
		objSchema(map[string]any{
			"task_id": map[string]any{"type": "integer", "description": "任务 id（必填——池按任务作用域绑定）"},
			"app_id":  strParam("小程序 AppID（必填，如 wx1a2b3c4d）"),
			"label":   strParam("角色标签（必填，如 admin / userA / userB / anonymous）"),
			"value":   strParam("凭据值（必填；落库即加密，审计面只有指纹）"),
			"scope":   strParam("作用域：task（默认）| company"),
		}, "task_id", "app_id", "label", "value"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID int64  `json:"task_id"`
				AppID  string `json:"app_id"`
				Label  string `json:"label"`
				Value  string `json:"value"`
				Scope  string `json:"scope"`
			}
			_ = json.Unmarshal(in, &a)
			fp, err := s.m.pg.UpsertMiniprogramToken(a.TaskID, a.AppID, a.Label, a.Value, a.Scope)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			s.appendMpAudit("mp-token-save", fmt.Sprintf("task=%d app=%s label=%s", a.TaskID, a.AppID, a.Label),
				map[string]any{"task_id": a.TaskID, "app_id": a.AppID, "label": a.Label, "fingerprint": fp})
			return actool.Text(fmt.Sprintf("token 已保存（task=%d app=%s label=%s fingerprint=%s）。凭据值未进本会话消息与审计载荷。", a.TaskID, a.AppID, a.Label, fp)), nil
		})
}

// toolMpTokenList 清单面（标签 + 指纹，无凭据内容）。
func (s *Server) toolMpTokenList() actool.CoreTool {
	return roTool("mp_token_list",
		"列出 token 池清单（按任务+应用过滤）：角色标签、指纹、作用域、更新时间。不含凭据内容——取明文用 mp_token_get(reveal=true)，仅在构造 Burp 重放 replacement 的使用时刻。",
		objSchema(map[string]any{
			"task_id": map[string]any{"type": "integer", "description": "任务 id（必填）"},
			"app_id":  strParam("应用过滤（可选）"),
		}, "task_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID int64  `json:"task_id"`
				AppID  string `json:"app_id"`
			}
			_ = json.Unmarshal(in, &a)
			tokens, err := s.m.pg.ListMiniprogramTokens(a.TaskID, a.AppID)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(map[string]any{"tokens": tokens, "count": len(tokens)})
		})
}

// toolMpTokenGet 取 token：默认指纹面，reveal=true 为使用时刻的明文取用。
func (s *Server) toolMpTokenGet() actool.CoreTool {
	return wrTool("mp_token_get",
		"取 token：默认（reveal=false）返回标签与指纹（清单核对用）；reveal=true 返回明文——【仅】在构造 Burp resend 的 replacement / 运行时注入的使用时刻调用，取到的明文不要再写进会话消息或 hints。每次 reveal 走审计（凭据值不在审计载荷里）。",
		objSchema(map[string]any{
			"id":     map[string]any{"type": "integer", "description": "token id（mp_token_list 返回）"},
			"reveal": map[string]any{"type": "boolean", "description": "是否取明文（默认 false）"},
		}, "id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				ID     int64 `json:"id"`
				Reveal bool  `json:"reveal"`
			}
			_ = json.Unmarshal(in, &a)
			meta, err := s.m.pg.MiniprogramTokenMeta(a.ID)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if meta == nil {
				return actool.Errorf(fmt.Sprintf("token #%d 不存在", a.ID)), nil
			}
			if !a.Reveal {
				return jsonResult(map[string]any{"id": a.ID, "app_id": meta.AppID, "label": meta.Label,
					"fingerprint": meta.Fingerprint, "scope": meta.Scope})
			}
			appID, label, value, err := s.m.pg.RevealMiniprogramToken(a.ID)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			fp := db.TokenFingerprint(value)
			s.appendMpAudit("mp-token-reveal", fmt.Sprintf("id=%d app=%s label=%s fingerprint=%s", a.ID, appID, label, fp),
				map[string]any{"id": a.ID, "app_id": appID, "label": label, "fingerprint": fp})
			return actool.Text(fmt.Sprintf("app=%s label=%s fingerprint=%s\nvalue=%s", appID, label, fp, value)), nil
		})
}

// appendMpAudit 是小程序侧动作的审计链双写（与 appendBizAudit 同款纪律）。
func (s *Server) appendMpAudit(action, reason string, body map[string]any) {
	b, _ := json.Marshal(body)
	if _, err := s.m.pg.AppendAuditEvent(db.AuditEvent{
		Actor:    "miniprogram",
		ToolName: "mp",
		Action:   action,
		Reason:   reason,
		Payload:  b,
		Source:   "miniprogram",
	}); err != nil {
		log.Printf("[mp] 审计链写入失败（忽略）: %v", err)
	}
}
