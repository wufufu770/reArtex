package credbreaker

import (
	"testing"
	"time"
)

// A-13 验收：同一 key 连续 2 次 401 即断路；第 3 次调用在窗口内直接拒绝；
// 一次 2xx 即复位；不同 host / 不同 profile 互不干扰；重启（Restore）后
// 断路态仍生效。
func TestBreakerTripsOnTwoConsecutiveAuthFailures(t *testing.T) {
	var persisted []Key
	var opened []Key
	b := New(func(k Key, _ State) { persisted = append(persisted, k) }, nil,
		func(k Key, _ State) { opened = append(opened, k) })
	k := Key{ProfileID: 7, Host: "api.example.com"}

	if !b.Allow(k) {
		t.Fatal("初始状态应放行")
	}
	if b.Record(k, 401) {
		t.Fatal("第 1 次 401 不应触发断路")
	}
	if !b.Allow(k) {
		t.Fatal("仅 1 次 401 不应断路")
	}
	if !b.Record(k, 403) {
		t.Fatal("第 2 次连续 401/403 应触发断路")
	}
	if b.Allow(k) {
		t.Fatal("断路窗口内应拒绝")
	}
	if len(opened) != 1 {
		t.Fatalf("onOpen 应恰好回调 1 次，实际 %d", len(opened))
	}
	if len(persisted) != 2 {
		t.Fatalf("401/403 各应落库一次，实际 %d", len(persisted))
	}
	until := b.OpenUntil(k)
	if until.IsZero() || time.Until(until) <= 0 || time.Until(until) > OpenDuration {
		t.Fatalf("断路截止时刻应在 (now, now+30m] 内，得到 %v", until)
	}

	// 窗口内继续吃 401 不重复断路（拒绝由 Allow 承担，计数不再推进）。
	if b.Record(k, 401) {
		t.Fatal("已断路的键不应重复触发断路")
	}
	if len(opened) != 1 {
		t.Fatalf("窗口内重复 401 不应再回调 onOpen，实际 %d", len(opened))
	}
}

func TestBreakerResetsOnSuccess(t *testing.T) {
	b := New(nil, nil, nil)
	k := Key{ProfileID: 1, Host: "h"}
	_ = b.Record(k, 401)
	// 非凭据失败不清零（429 不推进也不复位「连续」计数）。
	b.Record(k, 429)
	if !b.Record(k, 403) {
		t.Fatal("401 → 429 → 403 的连续 401/403 序列应断路")
	}
	// 手动复位后，一次 200 即彻底清除状态。
	b.Reset(k)
	b.Record(k, 200)
	if !b.Allow(k) {
		t.Fatal("200 复位后应放行")
	}
	if _, ok := b.Snapshot()[k]; ok {
		t.Fatal("复位+成功后不应残留状态行")
	}

	// 纯 2xx 复位路径：401 一次后紧跟 200，计数清零。
	_ = b.Record(k, 401)
	b.Record(k, 200)
	if b.Record(k, 401) {
		t.Fatal("200 复位后单次 401 不应直接断路")
	}
}

func TestBreakerKeysAreIndependent(t *testing.T) {
	b := New(nil, nil, nil)
	a := Key{ProfileID: 1, Host: "a.example.com"}
	bk := Key{ProfileID: 1, Host: "b.example.com"}
	c := Key{ProfileID: 2, Host: "a.example.com"}
	_ = b.Record(a, 401)
	_ = b.Record(a, 401)
	if !b.Allow(bk) || !b.Allow(c) {
		t.Fatal("不同 host / 不同 profile 的键不应被连带断路")
	}
	if b.Allow(a) {
		t.Fatal("被断路的键应保持拒绝")
	}
	// 大小写归一：同一 host 不同写法是同一个键。
	if b.Allow(Key{ProfileID: 1, Host: "A.EXAMPLE.COM"}) {
		t.Fatal("host 归一化后应命中同一断路键")
	}
}

// 重启恢复：persist 回调捕到的状态在「新进程」里 Restore 后仍拒绝，
// 窗口过期后自动回到放行。
func TestBreakerRestoreSurvivesRestart(t *testing.T) {
	var saved map[Key]State
	b := New(func(k Key, st State) {
		if saved == nil {
			saved = map[Key]State{}
		}
		saved[k] = st
	}, nil, nil)
	k := Key{ProfileID: 9, Host: "db.internal"}
	_ = b.Record(k, 401)
	if !b.Record(k, 403) {
		t.Fatal("用例前提不成立：应已断路")
	}

	// 新进程：从 DB 播种（测试直接用捕获到的持久化快照）。
	reborn := New(nil, nil, nil)
	for kk, st := range saved {
		reborn.Restore(kk, st)
	}
	if reborn.Allow(k) {
		t.Fatal("重启后的断路态应仍然生效")
	}

	// 窗口过期：把 OpenUntil 拨回过去再 Restore，应放行。
	expired := saved[k]
	expired.OpenUntil = time.Now().Add(-time.Minute)
	reborn.Restore(k, expired)
	if !reborn.Allow(k) {
		t.Fatal("过期的断路窗口应放行")
	}
}
