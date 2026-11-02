package server

// 本文件是【审计链校验】的对外通道。
//
// 审计链（db/audit_chain.go）每条记录的 digest 都依赖上一条 ⇒ 链被改过一定
// 能验出来。追加有入口（db.AppendAuditEvent，由各处裁决写调用），校验却一直
// 没有对外通道 —— 操作者没法自查「这条链还完整吗」。
//
// 【只读】校验不改链：一个 GET 绝不能顺手把断链「修好」。

import (
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
