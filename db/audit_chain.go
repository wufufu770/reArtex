package db

import (
	"crypto/hmac"
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
//
// A-05（0.15.89）起：digest 从无密钥 SHA-256 升级为 **HMAC-SHA256**——无密钥
// 哈希链挡得住「改一条」，挡不住「持 DB 写权限者从断点起整条重算」；加了只有
// 平台侧有的密钥（主密钥派生，见 DeriveAuditKey），重算整条链需要连密钥文件
// 一起拿走，而密钥文件在数据目录之外（MasterKeyOutsideDir）。存量行保持旧
// SHA-256 并按 key_id 区分：新行 HMAC（source 纳入计算——来源标记也是审计
// 事实），旧行原样可验，链在边界处自然衔接（第一条 HMAC 行的 prev 就是旧链尾
// digest，见 MigrateAuditChainToHMAC）。配套外部锚点（audit_anchors 表 +
// scripts/audit-anchor.sh 写第二介质）：链尾被整体截断（删行重写）也能与锚点
// 对不上而现形——VerifyAuditChain 报断裂位置与锚点不一致两种告警。

// appendAuditMu 串行化审计写入。
//
// 链的每条都引用上一条的 digest，写入必须严格有序：两个 goroutine 同时追加会
// 算出两条引用同一个 prev_digest 的记录，链当场断掉。用进程内的锁把写入排成
// 单写者（对应审计要求里的「单写协程串行化」）。
var appendAuditMu sync.Mutex

// auditKeyID 标记 HMAC 行（key_id 列）。空串 = 旧 SHA-256 行（存量/未配密钥）。
const auditKeyID = "hmac-v1"

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
	// Source 是来源标记（M1，方案 v10 §6：actor/时间/来源三要素）。A-05 起
	// **纳入 HMAC 计算**——来源标记也是审计事实，不该有「换了来源摘要不变」
	// 的重写空间；旧 SHA-256 行保持五段口径（存量行回填 'legacy' 不破链）。
	Source    string
	CreatedAt time.Time
}

// auditDigest 是旧口径的无密钥 SHA-256 摘要（存量链逐条可验；新写入只在
// 未配置审计密钥的部署里沿用——告警见 server 侧装配日志）。
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

// auditDigestHMAC 是 A-05 的 HMAC-SHA256 摘要：密钥（主密钥派生值） +
// 前一条 digest + 本条内容（source 纳入）。
func auditDigestHMAC(key []byte, prev string, seq int64, actor, tool, action, reason, source string, payload []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(prev))
	mac.Write([]byte{0})
	fmt.Fprintf(mac, "%d\x00%s\x00%s\x00%s\x00%s\x00%s\x00", seq, actor, tool, action, reason, source)
	if len(payload) > 0 {
		mac.Write(payload)
	}
	return hex.EncodeToString(mac.Sum(nil))
}

// DeriveAuditKey 从主密钥派生审计链 HMAC 密钥。域分离派生（常量后缀 + SHA-256）：
// 同一把主密钥还加密 LLM key（AES），一把钥开两把锁不是加固——审计域拿自己的
// 派生值，泄露互不连带。
func DeriveAuditKey(master []byte) []byte {
	h := sha256.Sum256(append(append([]byte{}, master...), []byte("artex-audit-hmac-v1")...))
	return h[:]
}

// WithAuditKey 注入审计链 HMAC 密钥（A-05）：主密钥的派生值（DeriveAuditKey）。
// 只进进程内存，不落库、不进 settings、不进日志。nil = 未配置，新写入沿用旧
// 无密钥口径（server 装配时告警）。启动装配一次，进程生命周期内不变。
func (d *DB) WithAuditKey(key []byte) *DB {
	d.auditKey = key
	return d
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
// best-effort 由调用方决定：这里返回错误，不吞。已配置审计密钥 ⇒ HMAC 行
// （source 纳入摘要，key_id='hmac-v1'）；未配置 ⇒ 旧无密钥口径（存量部署过渡态，
// server 装配时告警）。
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
	keyID := ""
	digest := auditDigest(prevDigest, seq, ev.Actor, ev.ToolName, ev.Action, ev.Reason, payload)
	if len(d.auditKey) > 0 {
		keyID = auditKeyID
		digest = auditDigestHMAC(d.auditKey, prevDigest, seq, ev.Actor, ev.ToolName, ev.Action, ev.Reason, ev.Source, payload)
	}
	_, err := d.Exec(`
INSERT INTO audit_events(seq, actor, tool_name, action, reason, payload, digest, prev_digest, source, key_id)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		seq, ev.Actor, ev.ToolName, ev.Action, ev.Reason, payload, digest, prevDigest, ev.Source, keyID)
	if err != nil {
		return "", err
	}
	return digest, nil
}

// AuditChainTail 返回链上最后一条的 seq（空链为 0）。导出：锚点接口（server）
// 也要用它对账。
func (d *DB) AuditChainTail() (int64, error) {
	var seq int64
	err := d.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM audit_events`).Scan(&seq)
	return seq, err
}

// VerifyAuditChain 逐条重算链上的 digest（按行 key_id 选口径），并核对最近
// 一次锚点（A-05），返回第一处断裂的 seq。
//
// 返回 (0, nil) 表示链完整且锚点一致。两类告警：
//   - 链断裂：某行 digest/prev 对不上（改行、插行、删行都现形），报断裂 seq；
//   - 锚点不一致：链尾 seq 小于锚点 seq（尾部被截断）或锚点位 digest 对不上
//     （锚点之后整段被重写），报锚点 seq。
//
// 两种同时发生时链断裂优先（先报改写、再报截断）。链为空且无锚点也算完整。
// 本实例未配置密钥而链上有 HMAC 行 ⇒ 无法复算，如实报错（不是跳过）。
func (d *DB) VerifyAuditChain() (int64, error) {
	if seq, err := d.verifyChainRows(); err != nil {
		return seq, err
	}
	// 锚点核对：最近一次锚点必须在链上且 digest 一致，链尾不得早于锚点。
	anchor, ok, err := d.LatestAuditAnchor()
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, nil
	}
	tail, err := d.AuditChainTail()
	if err != nil {
		return 0, err
	}
	if tail < anchor.Seq {
		return anchor.Seq, fmt.Errorf("审计锚点与链不一致：链尾 seq=%d 早于锚点 seq=%d（尾部被截断）", tail, anchor.Seq)
	}
	if err := d.VerifyAuditAnchor(anchor); err != nil {
		return anchor.Seq, err
	}
	return 0, nil
}

// verifyChainRows 只做逐行重算（不含锚点核对）：VerifyAuditChain 的链部分，
// 也是迁移判断存量链是否完整的入口。
func (d *DB) verifyChainRows() (int64, error) {
	rows, err := d.Query(`
SELECT seq, actor, tool_name, action, reason, payload, digest, prev_digest, key_id, source
FROM audit_events ORDER BY seq ASC`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	prev := ""
	for rows.Next() {
		var seq int64
		var actor, tool, action, reason, digest, prevDigest, keyID, source string
		var payload []byte
		if err := rows.Scan(&seq, &actor, &tool, &action, &reason, &payload, &digest, &prevDigest, &keyID, &source); err != nil {
			return 0, err
		}
		var want string
		switch {
		case keyID == "":
			want = auditDigest(prev, seq, actor, tool, action, reason, payload)
		case len(d.auditKey) == 0:
			return seq, fmt.Errorf("审计链 seq=%d 为 HMAC 行，但本实例未配置审计密钥，无法复算", seq)
		default:
			want = auditDigestHMAC(d.auditKey, prev, seq, actor, tool, action, reason, source, payload)
		}
		if want != digest || prev != prevDigest {
			return seq, fmt.Errorf("审计链在 seq=%d 处断裂", seq)
		}
		prev = digest
	}
	return 0, rows.Err()
}

// AuditAnchor 是审计链的一次外部锚定记录（A-05）。
type AuditAnchor struct {
	Seq        int64     `json:"seq"`         // 锚定时链尾 seq
	Digest     string    `json:"digest"`      // 该 seq 行的 digest
	KeyID      string    `json:"key_id"`      // 锚点位的行口径（轮换预留；''=旧 SHA-256）
	AnchoredAt time.Time `json:"anchored_at"` // 锚定时间
	Note       string    `json:"note"`        // 操作备注（genesis/脚本名等）
}

// LatestAuditAnchor 返回最近一次锚点（按 seq 降序）。没有锚点时 ok=false。
func (d *DB) LatestAuditAnchor() (AuditAnchor, bool, error) {
	var a AuditAnchor
	var at time.Time
	row := d.QueryRow(`
SELECT seq, digest, key_id, anchored_at, note FROM audit_anchors ORDER BY seq DESC LIMIT 1`)
	switch err := row.Scan(&a.Seq, &a.Digest, &a.KeyID, &at, &a.Note); err {
	case nil:
		a.AnchoredAt = at
		return a, true, nil
	case sql.ErrNoRows:
		return AuditAnchor{}, false, nil
	default:
		return AuditAnchor{}, false, err
	}
}

// VerifyAuditAnchor 比对锚点与链：锚点 seq 处的行必须存在且 digest 一致。
// 链上找不到该行 = 锚点之后的内容被删除（比断裂更严重的重写痕迹）。
func (d *DB) VerifyAuditAnchor(a AuditAnchor) error {
	var digest string
	err := d.QueryRow(`SELECT digest FROM audit_events WHERE seq = $1`, a.Seq).Scan(&digest)
	if err == sql.ErrNoRows {
		return fmt.Errorf("审计锚点与链不一致：锚点 seq=%d 在链上已不存在（行被删除）", a.Seq)
	}
	if err != nil {
		return err
	}
	if digest != a.Digest {
		return fmt.Errorf("审计锚点与链不一致：锚点 seq=%d 的 digest 对不上（该行或其后整段被重写）", a.Seq)
	}
	return nil
}

// RecordAuditAnchor 在当前链尾打一次锚点（A-05）。主要调用方是迁移（genesis）
// 与运维脚本经 psql 直插；进程内方法供测试与未来定时锚定使用。
func (d *DB) RecordAuditAnchor(note string) (AuditAnchor, error) {
	var seq int64
	var digest, keyID string
	row := d.QueryRow(`SELECT seq, digest, key_id FROM audit_events ORDER BY seq DESC LIMIT 1`)
	switch err := row.Scan(&seq, &digest, &keyID); err {
	case nil:
	case sql.ErrNoRows:
		return AuditAnchor{}, fmt.Errorf("链为空，没有可锚定的链尾")
	default:
		return AuditAnchor{}, err
	}
	_, err := d.Exec(`
INSERT INTO audit_anchors(seq, digest, key_id, note) VALUES ($1,$2,$3,$4)`, seq, digest, keyID, note)
	if err != nil {
		return AuditAnchor{}, err
	}
	return AuditAnchor{Seq: seq, Digest: digest, KeyID: keyID, Note: note, AnchoredAt: time.Now()}, nil
}

// MigrateAuditChainToHMAC 是启用 HMAC 后的启动期一次性重锚（A-05 第 4 步）：
// 旧 SHA-256 链验证通过 ⇒ 把链尾 digest 写成创世锚点（key_id=” 标记边界；
// 此后第一条 HMAC 行的 prev 就是这个 digest，链自然衔接，无需搬数据）。
//   - 链为空 ⇒ no-op（首条 HMAC 行就是链头）；
//   - 已有锚点 ⇒ no-op（幂等，重复启动零开销）；
//   - 旧链已断 ⇒ 不写锚点并返回断裂位置——断裂是必须人工看的结论；新行照常
//     追加（链的增长不被历史损坏阻塞），verify 接口会持续报出断裂。
func (d *DB) MigrateAuditChainToHMAC() error {
	if len(d.auditKey) == 0 {
		return nil // 未启用 HMAC，没有「迁移到 HMAC」可言
	}
	if _, ok, err := d.LatestAuditAnchor(); err != nil {
		return err
	} else if ok {
		return nil
	}
	tail, err := d.AuditChainTail()
	if err != nil {
		return err
	}
	if tail == 0 {
		return nil
	}
	// 存量链完整性是写创世锚点的前提：链本来就断着，锚点位选在哪都掩盖不了。
	if seq, err := d.verifyChainRows(); err != nil {
		return fmt.Errorf("存量审计链验证未通过（seq=%d），不写创世锚点: %w", seq, err)
	}
	_, err = d.RecordAuditAnchor("genesis: legacy sha256 tail → hmac-v1 (A-05)")
	return err
}
