package server

// 0.15.19 飞书桥广播回归：settings 闸（空 URL=通道关闭）+ http(s) scheme 校验 +
// 广播载荷与共享口令正确送达（httptest 桥接收端）。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestFeishuBridgeListener(t *testing.T) {
	var mu sync.Mutex
	var gotPath, gotSecret string
	var gotBody map[string]any
	received := make(chan struct{}, 1)
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath, gotSecret = r.URL.Path, r.Header.Get("X-Bridge-Secret")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		mu.Unlock()
		w.WriteHeader(200)
		select {
		case received <- struct{}{}:
		default:
		}
	}))
	defer bridge.Close()

	s, _ := taskHallTestServer(t)
	defer func() { s.m.interceptor.SetAskListener(nil) }()

	// ① 未配置 URL：监听器直呼应静默返回（通道关闭），广播零发出。
	s.feishuBridgeListener(1, 0, "7", "worker", "Bash", []byte(`{"command":"ls"}`), "测试原因")
	select {
	case <-received:
		t.Fatal("未配置 URL 不应广播")
	default:
	}

	// ② 配置桥 URL + 口令：广播送达，载荷与口令匹配。
	if err := s.m.pg.SetSetting(settingFeishuBridgeURL, bridge.URL+"/internal/approval"); err != nil {
		t.Fatal(err)
	}
	if err := s.m.pg.SetSetting(settingFeishuBridgeSecret, "test-secret"); err != nil {
		t.Fatal(err)
	}
	s.feishuBridgeListener(7, 0, "7", "worker", "Bash", []byte(`{"command":"ls /etc"}`), "高危命令待批")
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("广播未送达")
	}
	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/internal/approval" {
		t.Fatalf("桥收到的路径=%q", gotPath)
	}
	if gotSecret != "test-secret" {
		t.Fatalf("共享口令不符: %q", gotSecret)
	}
	if gotBody["pending_id"].(float64) != 7 || gotBody["tool"] != "Bash" || gotBody["agent"] != "worker" {
		t.Fatalf("载荷字段不符: %v", gotBody)
	}

	// ③ 非法 scheme：拒绝广播（防误配成非 http 面）。
	if err := s.m.pg.SetSetting(settingFeishuBridgeURL, "file:///tmp/x"); err != nil {
		t.Fatal(err)
	}
	s.feishuBridgeListener(8, 0, "7", "worker", "Bash", []byte(`{}`), "x")
	select {
	case <-received:
		t.Fatal("非法 scheme 不应广播")
	default:
	}
}
