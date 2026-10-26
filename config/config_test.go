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
