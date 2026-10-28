package db

import (
	"os"
	"os/exec"
	"testing"
)

// 进程环境里留着明文 DSN 时，agent 敲一条 env 就能读出数据库口令——因为 norma 的
// Bash 子进程无条件继承 os.Environ()（norma/tool/task.go:144）。
// 故 DSN 必须在读入时从进程环境抹掉。这条用例用真实的子进程验证抹除确实生效。
func TestDSNUnsetsProcessEnv(t *testing.T) {
	dsnCached, dsnSource = "", ""
	t.Cleanup(func() { dsnCached, dsnSource = "", "" })

	t.Setenv("ARTEX_PG_DSN", "postgres://u:sup3rs3cret@postgres:5432/artex")
	dsn, _, err := DSN()
	if err != nil {
		t.Fatal(err)
	}
	if dsn == "" {
		t.Fatal("DSN 不应为空")
	}
	if os.Getenv("ARTEX_PG_DSN") != "" {
		t.Fatal("读入后 ARTEX_PG_DSN 仍留在进程环境里")
	}
	// 缓存生效：再次调用仍能拿到值，且不会因环境已抹除而失败。
	again, _, err := DSN()
	if err != nil || again != dsn {
		t.Fatalf("缓存未生效: %q %v", again, err)
	}
	// 子进程视角：这是 agent 实际能触达的那条路径。
	out, err := exec.Command("sh", "-c", "printf '%s' \"${ARTEX_PG_DSN:-EMPTY}\"").Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "EMPTY" {
		t.Fatalf("子进程仍能看到 DSN: %q", out)
	}
}
