package db

import "testing"

// TestDSNUserPassword 冷启动自举的凭据解析（URL 形态为主，key=value 兜底）。
func TestDSNUserPassword(t *testing.T) {
	u, p, err := dsnUserPassword("postgres://artex_app:se%2Cret@postgres:5432/artex?sslmode=disable")
	if err != nil || u != "artex_app" || p != "se,ret" {
		t.Fatalf("url 形态: u=%q p=%q err=%v", u, p, err)
	}
	u, p, err = dsnUserPassword("host=postgres user=artex_app password=abc123 port=5432")
	if err != nil || u != "artex_app" || p != "abc123" {
		t.Fatalf("kv 形态: u=%q p=%q err=%v", u, p, err)
	}
	// 无口令（trust/peer）不报错，口令为空 ⇒ 跳过对齐。
	u, p, err = dsnUserPassword("postgres://artex_app@postgres/artex")
	if err != nil || u != "artex_app" || p != "" {
		t.Fatalf("无口令形态: u=%q p=%q err=%v", u, p, err)
	}
}
