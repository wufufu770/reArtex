package sod

import (
	"os"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/testdb"
)

// sodTestDB 是本包的测试库连接。
var sodTestDB *db.DB

// TestMain 给本包准备一个独立的测试库。
//
// 为什么需要库：审计器要往决策账本与哈希链写东西 —— 那是本包唯一需要真库的
// 部分（闸门与凭证都是纯内存的）。派生独立库（artex_test_sod）后本包与其它包
// 互不可见，用例不会互相污染。
func TestMain(m *testing.M) {
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
