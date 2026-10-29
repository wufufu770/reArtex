package agent

// cold-digest §6: graph_overview folding + the restore tools.
//
//	coldDigestsRecent — builds the folded cold region for graph_overview:
//	  cold_digests (flat {id, body, member_count}), newest-member first, capped.
//	expand_digest(id)  — level-1 restore: a digest's member compact list.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// digestMemberEntry builds the compact per-member view expand_digest returns —
// same shape as recent_facts / recent_done_intents (§6.1 middle level). store is
// the digest's OWNING store (the current task, or a read-only source task §2).
func (t *ToolSet) digestMemberEntry(store *db.ExplorationStore, id int64) map[string]any {
	n, _ := store.GetNode(id)
	if n == nil {
		return map[string]any{"id": id, "missing": true}
	}
	m := compactNode(n)
	m["state"] = n.State
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if c, ok := p["confidence"].(string); ok && c != "" {
			m["confidence"] = c
		}
	}
	return m
}

// coldDigest 是折叠冷区的一个 digest：正文 + 它折叠的成员 + 新鲜度。
type coldDigest struct {
	id        int64
	entry     map[string]any
	freshness int64 // max member id (ids are monotonic ≈ creation time)
	members   []int64
}

// collectColdDigests 收集一个 store 的活跃 digest（正文 + 成员 + 新鲜度）。
func collectColdDigests(store *db.ExplorationStore) []coldDigest {
	ads, err := store.ActiveDigests()
	if err != nil || len(ads) == 0 {
		return nil
	}
	items := make([]coldDigest, 0, len(ads))
	for _, d := range ads {
		var p struct {
			Body string `json:"body"`
		}
		_ = json.Unmarshal(d.Payload, &p)
		ms, _ := store.DigestMembers(d.ID) // sorted asc → last = freshest
		var fresh int64
		if len(ms) > 0 {
			fresh = ms[len(ms)-1]
		}
		items = append(items, coldDigest{
			id:        d.ID,
			entry:     map[string]any{"id": d.ID, "body": p.Body, "member_count": len(ms)},
			freshness: fresh,
			members:   ms,
		})
	}
	return items
}

// coldDigestsRecentVisible 返回一个 store 的活跃 digest（正文），按其最新成员的时间
// 降序排列，最多取 `cap` 个；被截的更旧 digest 只给裸 id（仍可 expand_digest 展开），
// 避免冷区唯一出口被无限拉长。
//
// 它【整体挡掉】那些覆盖了范围外成员的 digest。
//
// 为什么必须整块挡：digest 的正文不是成员的原文，而是模型把它所有成员的 summary
// 合并写成的一段叙述（agent/compaction.go 的 digestPayload）。所以「只留下范围内
// 那部分的正文」根本做不到 —— 正文已经混在一起了。半截保留就是泄露，所以要么整块
// 给，要么整块不给。
//
// scopeID<=0（没有任务上下文）⇒ 没有授权边界 ⇒ 一个 digest 都不给。
func coldDigestsRecentVisible(store *db.ExplorationStore, scopeID int64, cap int) (shown []map[string]any, moreIDs []int64) {
	if scopeID <= 0 {
		return nil, nil
	}
	items := collectColdDigests(store)
	if len(items) == 0 {
		return nil, nil
	}
	allMembers := []int64{}
	for _, it := range items {
		allMembers = append(allMembers, it.members...)
	}
	blocked, err := store.OutOfScopeAnchoredNodeIDs(scopeID, allMembers)
	if err != nil {
		return nil, nil
	}
	items = slices.DeleteFunc(items, func(it coldDigest) bool {
		for _, m := range it.members {
			if blocked[m] {
				return true
			}
		}
		return false
	})
	sort.Slice(items, func(i, j int) bool { return items[i].freshness > items[j].freshness })
	for i, it := range items {
		if i < cap {
			shown = append(shown, it.entry)
		} else {
			moreIDs = append(moreIDs, it.id)
		}
	}
	return shown, moreIDs
}

// hiddenMembersFor returns a predicate telling whether a member is hidden (folded
// into an active digest AND still cold) in the given store — so a source task's
// overview folds exactly the way that task folds itself (§2 cross-task: "当前任务
// 什么展示逻辑，关联任务就什么逻辑"). A revived (now hot) covered member is NOT
// hidden (§6 render-time revival check). Returns a never-hidden predicate when the
// store has no digests.
func hiddenMembersFor(store *db.ExplorationStore) func(int64) bool {
	covered, err := store.CoveredMembers()
	if err != nil || len(covered) == 0 {
		return func(int64) bool { return false }
	}
	var hot map[int64]bool
	if cg, _, err := loadColdGraph(store); err == nil {
		hot = cg.hotSet()
	}
	return func(id int64) bool { _, c := covered[id]; return c && !hot[id] }
}

// resolveDigest finds a digest node by id in the current task, else in a direct
// source task (read-only, §2). Returns the node, its owning store, and the source
// task id (0 = current task).
func (t *ToolSet) resolveDigest(id int64) (*db.Node, *db.ExplorationStore, int64) {
	if n, _ := t.ts.GetNode(id); n != nil && n.Kind == db.KindDigest {
		return n, t.ts, 0
	}
	srcs, _ := t.ts.DirectSourceStores()
	for _, s := range srcs {
		if n, _ := s.Store.GetNode(id); n != nil && n.Kind == db.KindDigest {
			return n, s.Store, s.Task.TaskID
		}
	}
	return nil, nil, 0
}

// expandDigest returns a digest's covered members as a compact list (§6.1). It is
// a distinct tool from node_detail because it returns a LIST of members, not one
// node's full detail.
func (t *ToolSet) expandDigest() actool.CoreTool {
	return t.writeExpTool("expand_digest",
		"展开一个 cold digest：返回它折叠的成员紧凑列表（id/summary/state/confidence），与概览 recent_facts/recent_done_intents 同形状。要某条完整细节/证据用 node_detail(member_id)。",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "integer", "description": "digest 节点 id（来自概览 cold_digests）"},
			},
			"required": []any{"id"},
		},
		func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
			var in struct {
				ID int64 `json:"id"`
			}
			_ = json.Unmarshal(raw, &in)
			n, store, srcTaskID := t.resolveDigest(in.ID)
			if n == nil {
				return jsonResult(map[string]any{"error": fmt.Sprintf("#%d 不是 digest 节点（本任务或直接关联任务里都没找到）", in.ID)})
			}
			var p struct {
				Body string `json:"body"`
			}
			_ = json.Unmarshal(n.Payload, &p)
			members, _ := store.DigestMembers(in.ID)
			// ⚠️ digest 的正文是所有成员 summary 的合并叙述，成员里若有一个锚在范围外
			// 资产上，正文就已经混进了范围外的结论 —— 所以整个 digest 都不给，而不是
			// 只挑出范围内的成员（那样正文还是混着的）。
			scopeID := t.scopeTaskID()
			if scopeID <= 0 {
				return jsonResult(map[string]any{"error": fmt.Sprintf("#%d 需要任务上下文才能展开 digest：没有任务上下文就没有范围边界，digest 的成员里可能混着范围外的结论", in.ID)})
			}
			blocked, err := store.OutOfScopeAnchoredNodeIDs(scopeID, members)
			if err != nil {
				return jsonResult(map[string]any{"error": fmt.Sprintf("展开 digest #%d 失败：%v", in.ID, err)})
			}
			for _, m := range members {
				if blocked[m] {
					return jsonResult(map[string]any{"error": fmt.Sprintf("#%d 折叠的成员里有范围外资产的结论，整个 digest 不展开：正文是所有成员摘要的合并叙述，半截保留就是泄露", in.ID)})
				}
			}
			list := make([]map[string]any, 0, len(members))
			for _, m := range members {
				entry := t.digestMemberEntry(store, m)
				if srcTaskID > 0 { // 关联任务的成员：只读，带继承标记（§2）
					entry["inherited"] = true
					entry["source_task_id"] = srcTaskID
				}
				list = append(list, entry)
			}
			out := map[string]any{
				"id":      in.ID,
				"state":   n.State, // active / superseded
				"body":    p.Body,
				"members": list,
			}
			if srcTaskID > 0 {
				out["inherited"] = true
				out["source_task_id"] = srcTaskID
			}
			return jsonResult(out)
		})
}
