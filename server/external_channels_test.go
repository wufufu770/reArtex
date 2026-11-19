package server

// 0.15.19/20 外部通道广播回归：settings 闸（空=关闭）+ http(s) scheme 校验 +
// 双目标扇出（飞书桥 JSON 要素 / 企微群 markdown 卡）+ 共享口令正确送达。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordedHit struct {
	path, secret, body string
}

func TestExternalChannelsListener(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]recordedHit{}
	received := make(chan string, 4)
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		hits[r.URL.Path] = recordedHit{r.URL.Path, r.Header.Get("X-Bridge-Secret"), string(raw)}
		mu.Unlock()
		w.WriteHeader(200)
		received <- r.URL.Path
	}))
	defer bridge.Close()

	s, _ := taskHallTestServer(t)
	defer func() { s.m.interceptor.SetAskListener(nil) }()

	// ① 未配置任何目标：静默返回，广播零发出。
	s.externalChannelListener(1, 0, "7", "worker", "Bash", []byte(`{"command":"ls"}`), "测试原因")
	select {
	case <-received:
		t.Fatal("未配置目标不应广播")
	case <-time.After(300 * time.Millisecond):
	}

	// ② 配置双目标：同一次扇出两路都到。
	if err := s.m.pg.SetSetting(settingFeishuBridgeURL, bridge.URL+"/internal/approval"); err != nil {
		t.Fatal(err)
	}
	if err := s.m.pg.SetSetting(settingFeishuBridgeSecret, "test-secret"); err != nil {
		t.Fatal(err)
	}
	if err := s.m.pg.SetSetting(settingWeComWebhookURL, bridge.URL+"/wecom-hook"); err != nil {
		t.Fatal(err)
	}
	s.externalChannelListener(7, 0, "7", "worker", "Bash", []byte(`{"command":"ls /etc"}`), "高危命令待批")
	deadline := time.Now().Add(3 * time.Second)
	got := map[string]bool{}
	for len(got) < 2 && time.Now().Before(deadline) {
		select {
		case p := <-received:
			got[p] = true
		case <-time.After(500 * time.Millisecond):
		}
	}
	if !got["/internal/approval"] || !got["/wecom-hook"] {
		t.Fatalf("双目标扇出不完整: %v", got)
	}
	mu.Lock()
	feishu, fOK := hits["/internal/approval"]
	wecom, wOK := hits["/wecom-hook"]
	mu.Unlock()
	if !fOK || !wOK {
		t.Fatalf("命中记录缺失: %v", hits)
	}
	if feishu.secret != "test-secret" {
		t.Fatalf("飞书共享口令不符: %q", feishu.secret)
	}
	var feishuBody map[string]any
	if err := json.Unmarshal([]byte(feishu.body), &feishuBody); err != nil {
		t.Fatal(err)
	}
	if feishuBody["pending_id"].(float64) != 7 || feishuBody["tool"] != "Bash" || feishuBody["agent"] != "worker" {
		t.Fatalf("飞书载荷字段不符: %v", feishuBody)
	}
	var wecomBody struct {
		MsgType  string `json:"msgtype"`
		Markdown struct {
			Content string `json:"content"`
		} `json:"markdown"`
	}
	if err := json.Unmarshal([]byte(wecom.body), &wecomBody); err != nil {
		t.Fatal(err)
	}
	if wecomBody.MsgType != "markdown" || !strings.Contains(wecomBody.Markdown.Content, "#7") || !strings.Contains(wecomBody.Markdown.Content, "Bash") {
		t.Fatalf("企微卡内容不符: %+v", wecomBody)
	}

	// ③ 非法 scheme：该目标拒绝广播（防误配成非 http 面），另一目标不受影响。
	if err := s.m.pg.SetSetting(settingFeishuBridgeURL, "file:///tmp/x"); err != nil {
		t.Fatal(err)
	}
	s.externalChannelListener(8, 0, "7", "worker", "Bash", []byte(`{}`), "x")
	deadline = time.Now().Add(2 * time.Second)
	sawWecom, sawFeishu := false, false
	for time.Now().Before(deadline) {
		select {
		case p := <-received:
			if p == "/wecom-hook" {
				sawWecom = true
			}
			if p == "/internal/approval" {
				sawFeishu = true
			}
		case <-time.After(300 * time.Millisecond):
			goto done
		}
	}
done:
	if sawFeishu {
		t.Fatal("非法 scheme 的飞书目标不应广播")
	}
	if !sawWecom {
		t.Fatal("企微目标应不受飞书非法配置影响")
	}
}
