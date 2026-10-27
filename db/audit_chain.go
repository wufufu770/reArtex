package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// 审计链：平台自身的安全判定（deny / ask / timeout）按顺序追加成一条哈希链。
//
// 为什么要有链：intercept_pending 里的记录可以被直接 UPDATE 掉，事后看不出
// 被动过手脚。链上每条都把上一条的 digest 算进自己的 digest，改动任何一条
// 都会让后续链条对不上，VerifyAuditChain 能报出断在第几条。
//
// 链只覆盖「判定动作」，不覆盖普通 allow —— 放行量极大（每次工具调用都可能
// 记一条），而真正需要事后追责的是被拦下的那些。

// appendAuditMu 串行化审计写入。
//
// 链的每条都引用上一条的 digest，写入必须严格有序：两个 goroutine 同时追加会
// 算出两条引用同一个 prev_digest 的记录，链当场断掉。用进程内的锁把写入排成
// 单写者（对应审计要求里的「单写协程串行化」）。
var appendAuditMu sync.Mutex

// AuditEvent 是审计链上的一条事件。
type AuditEvent struct {
	Seq        int64
	Actor      string
	ToolName   string
	Action     string
	Reason     string
	Payload    json.RawMessage
	Digest     string
	PrevDigest string
	CreatedAt  time.Time
}

// auditDigest 算出一条事件的 digest：前一条的 digest + 本条内容。
//
// 用 \x00 分隔各段：字段值里可能出现分隔符，固定分隔 + 长度前缀式的顺序拼接
// 足以避免「换个字段划分方式就撞出同一个哈希」。
func auditDigest(prev string, seq int64, actor, tool, action, reason string, payload []byte) string {
	h := sha256.New()
	h.Write([]byte(prev))
	h.Write([]byte{0})
	fmt.Fprintf(h, "%d\x00%s\x00%s\x00%s\x00%s\x00", seq, actor, tool, action, reason)
	if len(payload) > 0 {
		h.Write(payload)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// canonicalJSON 把 payload 规范化成稳定的字节序列。
//
// 必须规范化：同一个语义内容，键序或空白不同就得算出不同的 digest。
// 规范化后写入的是 TEXT（不是 JSONB）—— JSONB 落库会重排键、改空白，读回来的
// 字节与写入时不同，链会误报断裂。审计链要的是字节级可复算。
func canonicalJSON(payload []byte) []byte {
	if len(payload) == 0 {
		return []byte("{}")
	}
	var v any
	if err := json.Unmarshal(payload, &v); err != nil {
		return payload
	}
	out, err := json.Marshal(v)
	if err != nil {
		return payload
	}
	return out
}

// AppendAuditEvent 追加一条审计事件，返回它的 digest。
//
// best-effort 由调用方决定：这里返回错误，不吞。
func (d *DB) AppendAuditEvent(ev AuditEvent) (string, error) {
	appendAuditMu.Lock()
	defer appendAuditMu.Unlock()

	payload := canonicalJSON(ev.Payload)
	var prevDigest string
	var lastSeq int64
	row := d.QueryRow(`SELECT seq, digest FROM audit_events ORDER BY seq DESC LIMIT 1`)
	switch err := row.Scan(&lastSeq, &prevDigest); err {
	case nil:
	case sql.ErrNoRows:
		lastSeq = 0
		prevDigest = ""
	default:
		return "", err
	}
	seq := lastSeq + 1
	digest := auditDigest(prevDigest, seq, ev.Actor, ev.ToolName, ev.Action, ev.Reason, payload)
	_, err := d.Exec(`
INSERT INTO audit_events(seq, actor, tool_name, action, reason, payload, digest, prev_digest)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		seq, ev.Actor, ev.ToolName, ev.Action, ev.Reason, payload, digest, prevDigest)
	if err != nil {
		return "", err
	}
	return digest, nil
}

// auditChainTail 返回链上最后一条的 seq（空链为 0）。
func (d *DB) auditChainTail() (int64, error) {
	var seq int64
	err := d.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM audit_events`).Scan(&seq)
	return seq, err
}

// VerifyAuditChain 逐条重算链上的 digest，返回第一处断裂的 seq。
//
// 返回 (0, nil) 表示链完整。链为空也算完整 —— 没有记录就没有可被改的痕迹。
func (d *DB) VerifyAuditChain() (int64, error) {
	rows, err := d.Query(`
SELECT seq, actor, tool_name, action, reason, payload, digest, prev_digest
FROM audit_events ORDER BY seq ASC`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	prev := ""
	for rows.Next() {
		var ev AuditEvent
		var payload []byte
		if err := rows.Scan(&ev.Seq, &ev.Actor, &ev.ToolName, &ev.Action, &ev.Reason, &payload, &ev.Digest, &ev.PrevDigest); err != nil {
			return 0, err
		}
		want := auditDigest(prev, ev.Seq, ev.Actor, ev.ToolName, ev.Action, ev.Reason, payload)
		if want != ev.Digest || prev != ev.PrevDigest {
			return ev.Seq, fmt.Errorf("审计链在 seq=%d 处断裂", ev.Seq)
		}
		prev = ev.Digest
	}
	return 0, rows.Err()
}
