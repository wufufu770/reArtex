package sod

import (
	"os"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/testdb"
)

// sodTestDB 是本包的测试库连接。
var sodTestDB *db.DB

// sodE2EChildEnv 是「特权对照子进程」的标记。
//
// 为什么需要它：子进程不能再跑一遍 testdb.Setup —— Setup 会把派生库重建
// （drop + create），把父进程正在用的库删掉 ⇒ 父进程的断言会在子进程跑到
// 一半时炸掉。所以子进程走「复用已存在的派生库」的分支：连接串由父进程通过
// ARTEX_PG_DSN 传下来（TestMain 已经把它设成派生库的名字）。
const sodE2EChildEnv = "ARTEX_SOD_E2E_CHILD"

// TestMain 给本包准备一个独立的测试库。
//
// 为什么需要库：审计器要往决策账本与哈希链写东西 —— 那是本包唯一需要真库的
// 部分（闸门与凭证都是纯内存的）。派生独立库（artex_test_sod）后本包与其它包
// 互不可见，用例不会互相污染。
func TestMain(m *testing.M) {
	if os.Getenv(sodE2EChildEnv) == "1" {
		// 特权对照子进程：复用父进程已经建好的派生库，**绝不重建**。
		dsn := strings.TrimSpace(os.Getenv("ARTEX_PG_DSN"))
		if dsn != "" {
			if d, err := db.Open(dsn); err == nil {
				sodTestDB = d
			}
		}
		code := m.Run()
		if sodTestDB != nil {
			sodTestDB.Close()
		}
		os.Exit(code)
	}

	dsn, release, err := testdb.Setup("sod")
	if err != nil {
		os.Exit(m.Run())
	}
	_ = os.Setenv("ARTEX_PG_DSN", dsn)
	d, err := db.Open(dsn)
	if err == nil {
		sodTestDB = d
	}
	code := m.Run()
	// os.Exit 不执行 defer ⇒ 收尾必须显式做完。
	if sodTestDB != nil {
		sodTestDB.Close()
	}
	release()
	os.Exit(code)
}

// auditorTestDB 取测试库；没有可用数据库时跳过用例。
func auditorTestDB(t *testing.T) *db.DB {
	t.Helper()
	if sodTestDB == nil {
		t.Skip("没有可用的测试库 —— 跳过")
	}
	return sodTestDB
}
