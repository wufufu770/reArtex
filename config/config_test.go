package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPostgresDSNPrecedence(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	os.WriteFile(cfgPath, []byte(`{"database":{"host":"10.1.2.3","port":6000,"user":"u","password":"p","dbname":"d","sslmode":"require"}}`), 0o644)
	t.Setenv("ARTEX_CONFIG", cfgPath)

	// no env DSN → built from config file fields
	t.Setenv("ARTEX_PG_DSN", "")
	got, _, err := PostgresDSN()
	want := "postgres://u:p@10.1.2.3:6000/d?sslmode=require"
	if err != nil || got != want {
		t.Fatalf("from file: got %q err %v want %q", got, err, want)
	}

	// env wins over config file
	t.Setenv("ARTEX_PG_DSN", "postgres://envwins/x")
	if got, _, err := PostgresDSN(); err != nil || got != "postgres://envwins/x" {
		t.Fatalf("env should win, got %q err %v", got, err)
	}

	// no env, no file → error (no built-in default)
	t.Setenv("ARTEX_PG_DSN", "")
	t.Setenv("ARTEX_CONFIG", filepath.Join(dir, "nope.json"))
	if got, _, err := PostgresDSN(); err == nil {
		t.Fatalf("missing config should error, got %q", got)
	}

	// full dsn in config file is used verbatim
	os.WriteFile(cfgPath, []byte(`{"database":{"dsn":"postgres://full/dsn"}}`), 0o644)
	t.Setenv("ARTEX_CONFIG", cfgPath)
	if got, _, err := PostgresDSN(); err != nil || got != "postgres://full/dsn" {
		t.Fatalf("file dsn verbatim, got %q err %v", got, err)
	}
}

func TestPostgresDSNFromFile(t *testing.T) {
	dir := t.TempDir()
	dsnPath := filepath.Join(dir, "pg_dsn")
	const want = "postgres://u:p@db:5432/artex?sslmode=disable"
	if err := os.WriteFile(dsnPath, []byte(want), 0o600); err != nil {
		t.Fatalf("写 DSN 文件失败: %v", err)
	}

	t.Setenv("ARTEX_PG_DSN", "")
	t.Setenv("ARTEX_PG_DSN_FILE", dsnPath)

	got, source, err := PostgresDSN()
	if err != nil {
		t.Fatalf("从文件读 DSN 应成功: %v", err)
	}
	if got != want {
		t.Fatalf("从文件读 DSN: got %q want %q", got, want)
	}
	if !strings.Contains(source, dsnPath) {
		t.Fatalf("来源说明应含文件路径，得到 %q", source)
	}

	// 读入后文件必须被删除：否则秘密留在盘上等 agent 来捡（agent 与 artex 同 UID）。
	if _, err := os.Stat(dsnPath); !os.IsNotExist(err) {
		t.Fatalf("DSN 文件读入后应被删除，stat err=%v", err)
	}
}

func TestPostgresDSNFileSourceErrors(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ARTEX_PG_DSN", "")

	// 文件不存在 → 明确报错，不静默 fallback 到配置文件
	t.Setenv("ARTEX_PG_DSN_FILE", filepath.Join(dir, "missing"))
	if _, _, err := PostgresDSN(); err == nil {
		t.Fatal("DSN 文件不存在时应报错")
	}

	// 文件为空 → 明确报错
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("   \n"), 0o600); err != nil {
		t.Fatalf("写空 DSN 文件失败: %v", err)
	}
	t.Setenv("ARTEX_PG_DSN_FILE", empty)
	if _, _, err := PostgresDSN(); err == nil {
		t.Fatal("DSN 文件为空时应报错")
	}
}

func TestPostgresDSNEnvWinsOverFile(t *testing.T) {
	dir := t.TempDir()
	dsnPath := filepath.Join(dir, "pg_dsn")
	if err := os.WriteFile(dsnPath, []byte("postgres://fromfile/x"), 0o600); err != nil {
		t.Fatalf("写 DSN 文件失败: %v", err)
	}
	t.Setenv("ARTEX_PG_DSN", "postgres://fromenv/x")
	t.Setenv("ARTEX_PG_DSN_FILE", dsnPath)

	got, _, err := PostgresDSN()
	if err != nil || got != "postgres://fromenv/x" {
		t.Fatalf("env 应优先于文件，got %q err %v", got, err)
	}
	// env 生效时文件不该被动：它属于下一次启动的输入
	if _, err := os.Stat(dsnPath); err != nil {
		t.Fatalf("env 优先时不应动文件：%v", err)
	}
}

func TestLeakSecretEnvKeys(t *testing.T) {
	block := []byte("PATH=/usr/bin\x00ARTEX_PG_DSN=postgres://u:p@db/x\x00ANTHROPIC_API_KEY=\x00OPENAI_API_KEY=sk-xxx\x00HOME=/root\x00")
	got := leakSecretEnvKeys(block)
	want := []string{"ARTEX_PG_DSN", "OPENAI_API_KEY"}
	if len(got) != len(want) {
		t.Fatalf("泄露键: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("泄露键: got %v want %v", got, want)
		}
	}
}

func TestSecretEnvPolicy(t *testing.T) {
	// 无残留 → 不致命、无理由
	if fatal, reason := secretEnvPolicy(nil, "1"); fatal || reason != "" {
		t.Fatalf("无残留时应不致命且无理由，得到 fatal=%v reason=%q", fatal, reason)
	}
	// 有残留但未开启 → 只告警
	fatal, reason := secretEnvPolicy([]string{"ARTEX_PG_DSN"}, "")
	if fatal {
		t.Fatal("未开启 ARTEX_REQUIRE_ENV_SCRUB 时不应致命")
	}
	if !strings.Contains(reason, "ARTEX_PG_DSN") {
		t.Fatalf("理由应含泄露键名，得到 %q", reason)
	}
	// 有残留且开启 → 致命
	if fatal, _ := secretEnvPolicy([]string{"ARTEX_PG_DSN"}, "1"); !fatal {
		t.Fatal("开启 ARTEX_REQUIRE_ENV_SCRUB=1 后应致命")
	}
}

// docker secret 是只读挂载：既不能清零也不能删。但它落在 /run/secrets，
// 不在 agent 的工作区（/app/data）里，agent 够不着 —— 故容忍不烧毁。
//
// 否则 compose 用 secret 供给 DSN 的部署根本起不来（删除失败即报错）。
func TestPostgresDSNFileReadOnlyMountIsTolerated(t *testing.T) {
	dir := t.TempDir()
	orig := readOnlySecretDir
	readOnlySecretDir = dir
	t.Cleanup(func() { readOnlySecretDir = orig })

	p := filepath.Join(dir, "pg_dsn")
	const want = "postgres://u:p@h:5432/d"
	if err := os.WriteFile(p, []byte(want), 0o400); err != nil {
		t.Fatalf("写 secret 文件失败: %v", err)
	}
	t.Setenv("ARTEX_PG_DSN", "")
	t.Setenv("ARTEX_PG_DSN_FILE", p)

	got, source, err := PostgresDSN()
	if err != nil {
		t.Fatalf("只读 secret 应能读出 DSN: %v", err)
	}
	if got != want {
		t.Fatalf("DSN 不对: %q", got)
	}
	if !strings.Contains(source, p) {
		t.Fatalf("来源说明应含文件路径，得到 %q", source)
	}
	// 可写路径仍必须被烧毁（既有行为不能被这次改动放松）
	dir2 := t.TempDir()
	p2 := filepath.Join(dir2, "pg_dsn")
	if err := os.WriteFile(p2, []byte(want), 0o600); err != nil {
		t.Fatalf("写 DSN 文件失败: %v", err)
	}
	t.Setenv("ARTEX_PG_DSN_FILE", p2)
	if _, _, err := PostgresDSN(); err != nil {
		t.Fatalf("可写 DSN 文件应能读出: %v", err)
	}
	if _, err := os.Stat(p2); !os.IsNotExist(err) {
		t.Fatalf("可写 DSN 文件读入后应被删除，stat err=%v", err)
	}
}
