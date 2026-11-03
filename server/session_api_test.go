package server

// 会话 API 冒烟文件（验证完即删）：把会话路由挂到独立 mux 上，跑一遍端到端形状。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func tmpSuffix() int64 { return time.Now().UnixNano() }

func tmpDo(t *testing.T, h http.Handler, tok, method, path string, body any) (int, string) {
	t.Helper()
	var r *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, bytes.NewReader(b))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code, rec.Body.String()
}

func TestSessionAPISmoke(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { m.Close() })
	if m.pg == nil {
		t.Skip("postgres unavailable")
	}
	td := t.TempDir()
	s := New(testServerCtx(t), m, td, td, td)
	tok, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	mux := http.NewServeMux()
	s.RegisterSessionRoutes(mux)
	h := cors(s.requireAuth(mux))

	name := "冒烟-" + strconv.FormatInt(tmpSuffix(), 10)

	// 建会话
	code, body := tmpDo(t, h, tok, "POST", "/api/sessions", map[string]any{"name": name})
	t.Logf("POST /api/sessions -> %d %s", code, body)
	if code != 201 {
		t.Fatalf("建会话失败: %d %s", code, body)
	}
	var created struct {
		Session struct {
			ID        int64  `json:"id"`
			Name      string `json:"name"`
			Kind      string `json:"kind"`
			Members   []any  `json:"members"`
			AgentCnt  int    `json:"agent_count"`
			Attachmnt []any  `json:"attachments"`
		} `json:"session"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("解响应失败: %v", err)
	}
	sid := created.Session.ID
	if sid == 0 || created.Session.Name != name {
		t.Fatalf("建出来的会话不对: %+v", created.Session)
	}

	// 列表
	code, body = tmpDo(t, h, tok, "GET", "/api/sessions", nil)
	t.Logf("GET /api/sessions -> %d %s", code, body)
	if code != 200 {
		t.Fatalf("列会话失败: %d %s", code, body)
	}

	// 详情
	code, body = tmpDo(t, h, tok, "GET", "/api/sessions/"+strconv.FormatInt(sid, 10), nil)
	t.Logf("GET /api/sessions/{id} -> %d %s", code, body)

	// 改名
	code, body = tmpDo(t, h, tok, "PATCH", "/api/sessions/"+strconv.FormatInt(sid, 10),
		map[string]any{"title": "冒烟标题", "pinned": true})
	t.Logf("PATCH /api/sessions/{id} -> %d %s", code, body)
	if code != 200 {
		t.Fatalf("改会话失败: %d %s", code, body)
	}

	// 发消息（@auto ⇒ 唤醒集合）
	code, body = tmpDo(t, h, tok, "POST", "/api/sessions/"+strconv.FormatInt(sid, 10)+"/messages",
		map[string]any{"body": "@auto 看一下这个"})
	t.Logf("POST messages -> %d %s", code, body)
	if code != 201 {
		t.Fatalf("发消息失败: %d %s", code, body)
	}
	var msgResp struct {
		Mentions []string `json:"mentions"`
		Wake     []string `json:"wake_agents"`
	}
	if err := json.Unmarshal([]byte(body), &msgResp); err != nil {
		t.Fatalf("解消息响应失败: %v", err)
	}
	if len(msgResp.Mentions) != 1 || msgResp.Mentions[0] != "auto" {
		t.Fatalf("mentions 不对: %v", msgResp.Mentions)
	}

	// 读消息
	code, body = tmpDo(t, h, tok, "GET", "/api/sessions/"+strconv.FormatInt(sid, 10)+"/messages", nil)
	t.Logf("GET messages -> %d %s", code, body)
	if code != 200 {
		t.Fatalf("读消息失败: %d %s", code, body)
	}

	// 在场状态
	code, body = tmpDo(t, h, tok, "GET", "/api/sessions/"+strconv.FormatInt(sid, 10)+"/presence", nil)
	t.Logf("GET presence -> %d %s", code, body)

	// 热加成员
	code, body = tmpDo(t, h, tok, "POST", "/api/sessions/"+strconv.FormatInt(sid, 10)+"/members",
		map[string]any{"agent_key": "auto"})
	t.Logf("POST members -> %d %s", code, body)

	// 反查（没有关联 ⇒ 空列表）
	code, body = tmpDo(t, h, tok, "GET", "/api/sessions/attachments/task/42/sessions", nil)
	t.Logf("GET attachments reverse -> %d %s", code, body)
	if code != 200 {
		t.Fatalf("反查失败: %d %s", code, body)
	}

	// 删会话
	code, body = tmpDo(t, h, tok, "DELETE", "/api/sessions/"+strconv.FormatInt(sid, 10), nil)
	t.Logf("DELETE /api/sessions/{id} -> %d %s", code, body)
	if code != 200 {
		t.Fatalf("删会话失败: %d %s", code, body)
	}
}
