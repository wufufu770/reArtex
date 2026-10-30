package sod

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// CanonicalDigest 返回入参的规范化摘要。
//
// 规范化口径与 db/audit_chain.go 的 canonicalJSON 一致：能解成 JSON 就先
// Unmarshal 再 Marshal（encoding/json 的 map 键序是稳定的）⇒ 同一份语义内容
// 永远得到同一个摘要；解不动（不是 JSON、或空载荷）就原样取字节。
//
// 为什么必须是「规范化后」的摘要：执行凭证绑定的是**将要执行的那份字节**。
// 审批时看到的是 JSON 文本，真正跑起来的可能是键序不同、空白不同的同一份
// 内容 —— 摘要若不规范化，一次正常的改写就会被判成「载荷被换过」而误拒。
//
// ⚠️ 这个摘要同时是审计里 payload_digest 的来源：**原始载荷只以摘要 + 脱敏
// 预览入库，绝不存原文**（见 redact.go）。
func CanonicalDigest(payload []byte) string {
	var v any
	if err := json.Unmarshal(payload, &v); err == nil {
		if canonical, err := json.Marshal(v); err == nil {
			payload = canonical
		}
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
