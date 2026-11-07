package server

// 方案 v3 缺陷 C2：预算僵尸闸的单测。
//
// 预算拒绝是确定性失败，旧行为下 planner 每心跳重试、任务永不收敛。闸的契约：
// 连续 quotaRefusalTerminal 次群组预算拒绝 ⇒ 任务落终态（failed）且只触发一次；
// 成功调用清零计数；非预算错误不影响计数。

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmrec"
)

func TestBudgetZombieGateConsecutiveRefusalsTerminateTask(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{m: m, engine: NewEngine(m), ctx: ctx}

	t0 := time.Now().UnixNano()
	desc := fmt.Sprintf("预算僵尸闸任务 %d", t0)
	created, err := m.CreateTaskWithOptions(desc, "测试目标", db.TaskCreateOptions{
		Name: fmt.Sprintf("zombie-%d", t0),
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	taskID := created.ID
	defer func() { _, _ = m.DeleteTask(taskID, DeleteTaskOptions{}) }()

	refusal := llmrec.GroupQuotaRefusal(db.ErrQuotaBudgetExceeded)
	for i := 1; i < quotaRefusalTerminal; i++ {
		if s.noteTaskQuotaOutcome(taskID, refusal) {
			t.Fatalf("第 %d 次拒绝不应触发终态", i)
		}
	}
	if !s.noteTaskQuotaOutcome(taskID, refusal) {
		t.Fatal("连续拒绝达到阈值必须触发终态")
	}
	// 终态已落：failed。
	status := m.TaskStatus(taskID)
	if status != "failed" {
		t.Fatalf("任务终态 = %q, want failed", status)
	}
	// 后续拒绝（终态后不再计数）不再重复触发。
	if s.noteTaskQuotaOutcome(taskID, refusal) {
		t.Fatal("终态后的拒绝不应重复触发")
	}
}

func TestBudgetZombieGateSuccessResetsCounter(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{m: m, engine: NewEngine(m), ctx: ctx}

	t0 := time.Now().UnixNano()
	created, err := m.CreateTaskWithOptions(fmt.Sprintf("预算计数清零 %d", t0), "测试目标", db.TaskCreateOptions{
		Name: fmt.Sprintf("zombie-reset-%d", t0),
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	taskID := created.ID
	defer func() { _, _ = m.DeleteTask(taskID, DeleteTaskOptions{}) }()

	refusal := llmrec.GroupQuotaRefusal(db.ErrQuotaBudgetExceeded)
	for i := 0; i < quotaRefusalTerminal-1; i++ {
		s.noteTaskQuotaOutcome(taskID, refusal)
	}
	// 一次成功调用清零：之后的拒绝序列要从头数。
	s.noteTaskQuotaOutcome(taskID, nil)
	for i := 1; i < quotaRefusalTerminal; i++ {
		if s.noteTaskQuotaOutcome(taskID, refusal) {
			t.Fatalf("清零后第 %d 次拒绝不应触发终态", i)
		}
	}
	if !s.noteTaskQuotaOutcome(taskID, refusal) {
		t.Fatal("清零后连续达到阈值应触发终态")
	}
	if got := m.TaskStatus(taskID); got != "failed" {
		t.Fatalf("任务终态 = %q, want failed", got)
	}
}

func TestBudgetZombieGateIgnoresOtherErrors(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{m: m, engine: NewEngine(m), ctx: ctx}

	t0 := time.Now().UnixNano()
	created, err := m.CreateTaskWithOptions(fmt.Sprintf("非预算错误 %d", t0), "测试目标", db.TaskCreateOptions{
		Name: fmt.Sprintf("zombie-other-%d", t0),
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	taskID := created.ID
	defer func() { _, _ = m.DeleteTask(taskID, DeleteTaskOptions{}) }()

	// 供应商额度耗尽/网络错误等其它错误不进预算计数：连续多个也不触发终态。
	for i := 0; i < quotaRefusalTerminal+2; i++ {
		if s.noteTaskQuotaOutcome(taskID, errors.New("insufficient_quota on provider")) {
			t.Fatal("供应商额度错误不应触发预算僵尸闸")
		}
	}
	if got := m.TaskStatus(taskID); got == "failed" {
		t.Fatal("非预算错误不应把任务置为 failed")
	}
}
