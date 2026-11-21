package server

// amber_curator.go：Amber 记忆 curator——**观察模式**（0.15.40，批次 J2）。
//
// 规则（B 方案 3.2 口径，v1 落地观察态）：定时扫 Amber 隔离区 → 按晋升规则
// 评估每条待审记忆 → 产出「拟晋升清单」写调度状态（sched_state）——**只记录
// 不落写**（不调 review 接口）。跑出观察数据、并由人工确认规则行为后切
// enforce（调用 /api/memory/review approve——J3 期）。
//
// 晋升规则（观察态评估）：
//   - specialize:*（目标本地事实）→ 拟自动晋升（14 天 TTL 语义由 Amber 侧管理）；
//   - generalize:*（跨目标泛化）→ 拟晋升条件 = 该 scope 下 active 记录 ≥2 或
//     任务 SoD 账本零 deny（Veto verified 的 v1 简化代理判定——完整 Veto 回执
//     关联在 J3 打通）；
//   - 冲突（同 scope 同 key 多条）→ 拟交人工，不自动 supersede。

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// schedKeyAmberCurator 是 curator 观察模式的水位（unix 秒）。
const schedKeyAmberCurator = "amber_curator_last_run"

// amberCuratorInterval 是观察模式的运行间隔（每小时，与策略 curator 同节奏）。
const amberCuratorInterval = time.Hour

// AmberCuratorObserve 跑一轮观察（scheduler 异步调用；amber 关闭时直接返回）。
func (s *Server) AmberCuratorObserve() {
	if !s.amberEnabled() {
		return
	}
	// 水位闸：距上次运行不足间隔则跳过。
	if raw, err := s.m.pg.GetSchedState(schedKeyAmberCurator); err == nil && raw != "" {
		if last, perr := strconv.ParseInt(raw, 10, 64); perr == nil {
			if time.Since(time.Unix(last, 0)) < amberCuratorInterval {
				return
			}
		}
	}
	_ = s.m.pg.SetSchedState(schedKeyAmberCurator, strconv.FormatInt(time.Now().Unix(), 10))

	// 拉隔离区（curator 用 owner 视角——review_queue 只列元数据）。
	quarantine, err := s.amberQuarantine()
	if err != nil {
		log.Printf("[amber-curator] 隔离区读取失败（跳过本轮）: %v", err)
		return
	}
	var promote, review []string
	for _, item := range quarantine {
		scope, _ := item["scope"].(string)
		itemID, _ := item["item_id"].(string)
		if itemID == "" {
			continue
		}
		switch {
		case stringHasPrefix(scope, "reartex:specialize:"):
			promote = append(promote, itemID)
		case stringHasPrefix(scope, "reartex:generalize"):
			// v1 简化代理：同 scope 已有 active 记录数 ≥2 才拟晋升。
			n := s.amberActiveCount(scope)
			if n >= 2 {
				promote = append(promote, itemID)
			} else {
				review = append(review, itemID)
			}
		default:
			review = append(review, itemID)
		}
	}
	observation := map[string]any{
		"observed_at":      time.Now().UTC().Format(time.RFC3339),
		"quarantine_total": len(quarantine),
		"propose_promote":  promote,
		"propose_review":   review,
		"mode":             "observe",
	}
	body, _ := json.Marshal(observation)
	if err := s.m.pg.SetSchedState(schedKeyAmberCurator+"_observation", string(body)); err != nil {
		log.Printf("[amber-curator] 观察结果落库失败: %v", err)
		return
	}
	log.Printf("[amber-curator] 观察完成：隔离区 %d 条，拟晋升 %d、拟人工 %d（观察态不落写）",
		len(quarantine), len(promote), len(review))
}

// amberQuarantine 拉隔离区元数据（owner token；无 token 时匿名 worker 视角读
// 不到——功能要求配 AMBER_OWNER_TOKEN）。
func (s *Server) amberQuarantine() ([]map[string]any, error) {
	body, status, err := s.amberGet("/api/memory/quarantine?limit=100")
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("amber quarantine HTTP %d", status)
	}
	var out struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// amberActiveCount 数某 scope 的 active 记录（简化代理判定）。
func (s *Server) amberActiveCount(scope string) int {
	body, status, err := s.amberGet("/api/memory/items?limit=200")
	if err != nil || status != 200 {
		return 0
	}
	var out struct {
		Items []struct {
			Scope  string `json:"scope"`
			Status string `json:"status"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &out) != nil {
		return 0
	}
	n := 0
	for _, it := range out.Items {
		if it.Scope == scope && it.Status == "active" {
			n++
		}
	}
	return n
}

// amberGet 是 GET 的公共形态（owner token 优先）。
func (s *Server) amberGet(path string) ([]byte, int, error) {
	req, _ := http.NewRequest(http.MethodGet, s.amberBaseURL()+path, nil)
	if tok := s.amberOwnerToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	buf := make([]byte, 1024*1024)
	n, _ := resp.Body.Read(buf)
	return buf[:n], resp.StatusCode, nil
}

// amberOwnerToken 读 owner 令牌（晋升裁决面；worker 令牌无 review 权限）。
func (s *Server) amberOwnerToken() string {
	return strings.TrimSpace(osGetenv("AMBER_OWNER_TOKEN"))
}

// stringHasPrefix 是 strings.HasPrefix 的局部别名（避免 import 噪声）。
func stringHasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }
