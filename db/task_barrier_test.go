package db

import (
	"errors"
	"strings"
	"testing"
)

// 工具表不可读且无快照时必须拒绝创建新 run —— 这是「白名单未知时不许开工」的最后一道闸。
func TestToolTableUnavailableBlocks(t *testing.T) {
	readErr := func() ([]*Tool, error) { return nil, errors.New("连接失败") }

	// 读不到、也没有快照 → 拒绝
	err := toolTableUnavailable(readErr, func() bool { return false })
	if err == nil {
		t.Fatal("工具表不可读且无快照时应拒绝")
	}
	if !strings.Contains(err.Error(), "工具表不可读") {
		t.Fatalf("错误应说明拒绝原因，得到 %q", err.Error())
	}

	// 读不到、但有上一份可用快照 → 放行（仍可按快照划边界）
	if err := toolTableUnavailable(readErr, func() bool { return true }); err != nil {
		t.Fatalf("有可用快照时应放行，得到 %v", err)
	}

	// 没有注册 fallback（无 server 装配）→ 读不到即拒绝
	if err := toolTableUnavailable(readErr, nil); err == nil {
		t.Fatal("未注册快照判定时，读不到工具表应拒绝")
	}

	// 工具表可读 → 放行
	ok := func() ([]*Tool, error) { return nil, nil }
	if err := toolTableUnavailable(ok, nil); err != nil {
		t.Fatalf("工具表可读时应放行，得到 %v", err)
	}
}

// 闸门必须落在事务之前：拒绝创建不该留下半截的 exploration 行。
//
// 这里用零值 DB（没有 *sql.DB）并注入一个「读不到工具表」的 listTools ——
// 若闸门没先生效，d.Begin() 会空指针 panic 而不是返回错误；断言本身就是对
// 「先闸门后事务」的检验。
func TestCreateTaskRefusedBeforeTransaction(t *testing.T) {
	d := &DB{}
	d.listTools = func() ([]*Tool, error) { return nil, errors.New("连接失败") }
	d.SetToolTableFallback(func() bool { return false })

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

// 参数校验的报错不能被闸门盖住：参数错误就应当以参数错误的形式返回。
func TestTaskArgumentValidationPrecedesBarrier(t *testing.T) {
	d := &DB{}
	d.listTools = func() ([]*Tool, error) { return nil, errors.New("连接失败") }
	d.SetToolTableFallback(func() bool { return false })

	_, err := d.CreateTaskWithOptions("child", "goal", TaskCreateOptions{
		SourceTaskIDs: []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17},
	})
	if err == nil {
		t.Fatal("源任务过多时应报错")
	}
	if !strings.Contains(err.Error(), "too many source tasks") {
		t.Fatalf("应是参数校验的错误而不是闸门的错误，得到 %q", err.Error())
	}
}
