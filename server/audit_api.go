package server

// 本文件是【审计链校验】的对外通道。
//
// 审计链（db/audit_chain.go）每条记录的 digest 都依赖上一条 ⇒ 链被改过一定
// 能验出来。追加有入口（db.AppendAuditEvent，由各处裁决写调用），校验却一直
// 没有对外通道 —— 操作者没法自查「这条链还完整吗」。
//
// 【只读】校验不改链：一个 GET 绝不能顺手把断链「修好」。

import (
	"fmt"
	"net/http"
)

// auditChainVerify 校验审计链的完整性。
//
// 返回 200 + intact=true 表示链完整；链断了同样返回 200，但 intact=false 且
// broken_at_seq 指出第一处断裂的位置。
//
// ⚠️ 为什么断链不是 500：断链是一个**结论**（链被改过），不是接口坏了。
// 500 会让调用方以为「校验没跑成」⇒ 反而看不出链断了。校验本身跑不动（读库
// 失败）才是 500。
func (s *Server) auditChainVerify(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	brokenAt, err := pg.VerifyAuditChain()
	if err != nil {
		// 链断了：VerifyAuditChain 返回 (断裂处的 seq, 错误)。
		writeJSON(w, 200, map[string]any{
			"intact":        false,
			"broken_at_seq": brokenAt,
			"error":         err.Error(),
		})
		return
	}
	writeJSON(w, 200, map[string]any{
		"intact":        true,
		"broken_at_seq": int64(0),
	})
}

// auditAnchor 返回最近的审计链锚点与核对结论（A-05，0.15.89）。
//
// 锚点是「某时刻链尾 seq+digest」的独立记录（audit_anchors 表 + 运维脚本把链头
// 同步写到备份目录之外的第二介质）：链尾被整段删除/重写时，链本身仍可能逐行
// 验通，但与锚点对不上——本接口把这一对比暴露给操作者与外部对账脚本。
//
// 【只读】与 chain 校验同一纪律：GET 绝不写锚点、绝不「修复」任何东西。
// consistent 语义：有锚点 ⇒ 锚点 seq 在链上、digest 一致且链尾不早于锚点；
// 无锚点 ⇒ null（不算不一致，但部署应尽快配置锚点脚本）。
func (s *Server) auditAnchor(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	anchor, ok, err := pg.LatestAuditAnchor()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	tail, err := pg.AuditChainTail()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	resp := map[string]any{
		"chain_tail_seq": tail,
		"anchor":         nil,
		"consistent":     true,
	}
	if ok {
		verr := pg.VerifyAuditAnchor(anchor)
		consistent := verr == nil && tail >= anchor.Seq
		resp["anchor"] = anchor
		resp["consistent"] = consistent
		if !consistent {
			if verr == nil {
				verr = fmt.Errorf("链尾 seq=%d 早于锚点 seq=%d（尾部被截断）", tail, anchor.Seq)
			}
			resp["error"] = verr.Error()
		}
	}
	writeJSON(w, 200, resp)
}
