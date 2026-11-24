package db

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TI-12 事件 payload 消毒测试（§0.12）：凭据类键值改写、超限/非 JSON 拒绝、
// EnqueueEvent 存储单点强制（消毒后落库可查、无 _sanitized 误标）。

func TestSanitizeEventPayloadRedactsCredentials(t *testing.T) {
	in := []byte(`{"headers":{"Authorization":"Bearer abc","Content-Type":"application/json"},
"cookies":{"session_cookie":"xyz123"},"body":{"user":"u","password":"p@ss"},"note":"正常内容"}`)
	clean, redacted, err := SanitizeEventPayload(in)
	if err != nil {
		t.Fatalf("消毒不应失败: %v", err)
	}
	s := string(clean)
	// 值被改写：凭据值不再出现。
	if strings.Contains(s, "Bearer abc") || strings.Contains(s, "xyz123") || strings.Contains(s, "p@ss") {
		t.Fatalf("凭据值应被改写: %s", s)
	}
	// 占位符在位 + 键名保留（事件语义需要知道哪个 header 被记录了）。
	for _, want := range []string{redactedPlaceholder, `"Authorization"`, `"session_cookie"`, `"password"`, "正常内容"} {
		if !strings.Contains(s, want) {
			t.Fatalf("消毒结果缺 %s: %s", want, s)
		}
	}
	// 改写路径可观测。
	if len(redacted) != 3 {
		t.Fatalf("应记录 3 处改写, got %v", redacted)
	}
	// _sanitized 标记块在位。
	var out map[string]any
	if err := json.Unmarshal(clean, &out); err != nil {
		t.Fatal(err)
	}
	if out["sanitized"] != true {
		t.Fatalf("应带 sanitized 标记: %v", out)
	}
}

func TestSanitizeEventPayloadCleanPass(t *testing.T) {
	in := []byte(`{"statement":"s","refs":[1,2],"nested":{"deep":{"ok":true}}}`)
	clean, redacted, err := SanitizeEventPayload(in)
	if err != nil {
		t.Fatalf("干净 payload 不应失败: %v", err)
	}
	if len(redacted) != 0 {
		t.Fatalf("干净 payload 不应有改写: %v", redacted)
	}
	// 无改写时不包 _sanitized 外壳，原语义原样。
	if bytes.Contains(clean, []byte("_sanitized")) {
		t.Fatalf("无改写不应加标记块: %s", clean)
	}
	// 数组内嵌凭据键同样命中。
	inArr := []byte(`{"items":[{"token":"secret-val"},{"token":"another"}]}`)
	clean, redacted, err = SanitizeEventPayload(inArr)
	if err != nil || len(redacted) != 2 {
		t.Fatalf("数组内凭据键应命中: redacted=%v err=%v", redacted, err)
	}
	if strings.Contains(string(clean), "secret-val") {
		t.Fatalf("数组内凭据值应被改写: %s", clean)
	}
}

func TestSanitizeEventPayloadRejects(t *testing.T) {
	// 非 JSON 拒绝。
	if _, _, err := SanitizeEventPayload([]byte("raw text, not json")); err == nil {
		t.Fatal("非 JSON 应拒绝")
	}
	// 超限拒绝（大内容走 evidence_refs）。
	big := []byte(`{"blob":"` + strings.Repeat("x", MaxEventPayloadBytes) + `"}`)
	if _, _, err := SanitizeEventPayload(big); err == nil {
		t.Fatal("超限 payload 应拒绝")
	}
}

// 存储单点强制：带凭据的 payload 经 EnqueueEvent 落库后值已被改写且带标记；
// 非 JSON payload 在入队口被拒。
func TestEnqueueEventSanitizes(t *testing.T) {
	d := tiSetup(t)
	rec := &DomainEventRecord{EventID: "evt-san-1", Type: "traffic.exchange.recorded",
		Payload: []byte(`{"headers":{"Cookie":"SESS=abc"},"exchange":"x1"}`)}
	if _, dedup, err := d.EnqueueEvent(rec); err != nil || dedup {
		t.Fatalf("入队: dedup=%v err=%v", dedup, err)
	}
	var stored string
	if err := d.QueryRow(`SELECT payload::text FROM domain_events WHERE event_id='evt-san-1'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "SESS=abc") {
		t.Fatalf("落库 payload 不应含凭据值: %s", stored)
	}
	if !strings.Contains(stored, "_sanitized") || !strings.Contains(stored, redactedPlaceholder) {
		t.Fatalf("落库 payload 应带消毒标记与占位符: %s", stored)
	}

	if _, _, err := d.EnqueueEvent(&DomainEventRecord{EventID: "evt-san-2", Type: "x",
		Payload: []byte("not-json")}); err == nil {
		t.Fatal("非 JSON payload 应在入队口被拒")
	}
}
