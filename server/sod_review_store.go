package server

import (
	"errors"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// errSodReviewStoreUnavailable 是存储未就绪时的错误。
//
// 为什么要有它而不是让 nil 指针自然崩：这两个工具是被模型调用的，崩掉会带走
// 整个 agent 进程；返回一句中文错误则模型能读懂并换条路。
var errSodReviewStoreUnavailable = errors.New("对抗复核队列存储未就绪")

// sodReviewStore 把 db.DB 的对抗复核队列能力适配成 agent.SodReviewStoreAPI。
//
// 为什么需要适配器而不是直接注入 *db.DB：agent 包刻意只认「读队列 / 结案」
// 两个动作（方法名是 ListPending / Resolve），不认表名、列名与状态机实现
// ——那样 db 改一次表形状就要动 agent 包，而 agent 包的依赖方向本该是单向的
// 且不绑定存储细节。
//
// 为什么这个适配器住在 server 而不是 agent：agent 不能 import db 的实现细节，
// 而 server 同时认识两边（它本来就要给 agent 装存储）⇒ 桥放在 server 是唯一
// 不产生新依赖方向的位置。
type sodReviewStore struct {
	pg *db.DB
}

// ListPending 列出待复核项。limit <= 0 时由 db 层取默认值（上限 200）。
func (s sodReviewStore) ListPending(taskID int64, limit int) ([]*db.AdversarialReviewItem, error) {
	if s.pg == nil {
		return nil, errSodReviewStoreUnavailable
	}
	return s.pg.ListAdversarialReviewPending(taskID, limit)
}

// Resolve 结案一条复核项。状态白名单由 agent 层把关（只允许 resolved /
// dismissed 两个终态），db 层另有自己的宽松校验（允许 pending / claimed）——
// 两层都在，是有意的：agent 层拦掉模型能说的话，db 层拦掉绕过 agent 的写入。
func (s sodReviewStore) Resolve(id int64, by, resolution, status string) error {
	if s.pg == nil {
		return errSodReviewStoreUnavailable
	}
	return s.pg.ResolveAdversarialReview(id, by, resolution, status)
}

// 断言适配器真的满足接口（编译期检查；这一行若不匹配会直接编译失败）。
var _ agent.SodReviewStoreAPI = sodReviewStore{}
