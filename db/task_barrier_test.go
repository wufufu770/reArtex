package db

import (
	"errors"
	"strings"
	"testing"
)

// 工具表不可读且无快照时必须拒绝创建新 run。
//
// 关键是闸门在事务之前：这里故意用零值 DB（没有 *sql.DB），若闸门没先生效，
// d.Begin() 会空指针 panic 而不是返回错误 —— 断言本身就是对「先闸门后事务」的检验。
func TestCreateTaskRefusedWhenToolTableUnavailable(t *testing.T) {
	orig := ToolTableUnavailable
	defer func() { ToolTableUnavailable = orig }()

	ToolTableUnavailable = func() error { return errors.New("工具表不可读且无可用快照") }

	d := &DB{}
	task, err := d.CreateTaskWithOptions("desc", "goal", TaskCreateOptions{})
	if err == nil {
		t.Fatalf("工具表不可用时应拒绝创建任务，却创建成功: %+v", task)
	}
	if !strings.Contains(err.Error(), "工具表不可读") {
		t.Fatalf("错误应说明拒绝原因，得到 %q", err.Error())
	}
	if task != nil {
		t.Fatalf("拒绝创建时不应返回任务，得到 %+v", task)
	}
}

// 闸门不设置时（无 server 装配的纯 db 场景）不应拦路。
// 用零值 DB 时会因缺 *sql.DB 而报错 —— 只要错误不是来自闸门即可。
func TestCreateTaskWithoutBarrierIsNotBlocked(t *testing.T) {
	orig := ToolTableUnavailable
	defer func() { ToolTableUnavailable = orig }()

	ToolTableUnavailable = nil

	defer func() {
		// 零值 DB 会在 d.Begin() 处 panic（缺 *sql.DB），那说明流程确实走到了
		// 闸门之后 —— 即闸门没有拦路。panic 本身不是失败。
		if r := recover(); r != nil {
			return
		}
		t.Fatal("闸门未设置时不应在创建前被拦下")
	}()

	d := &DB{}
	_, _ = d.CreateTaskWithOptions("desc", "goal", TaskCreateOptions{})
}
