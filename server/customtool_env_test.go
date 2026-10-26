package server

import (
	"os"
	"strings"
	"testing"
)

// 自定义脚本继承的环境必须是白名单：黑名单追不上将来新增的凭据变量。
func TestToolScriptBaseEnvAllowlist(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOME", "/root")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:8788")
	t.Setenv("ARTEX_PG_DSN", "postgres://u:secret@h/db")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-secret")
	t.Setenv("SOME_FUTURE_TOKEN", "whatever")

	got := strings.Join(toolScriptBaseEnv(), "\n")
	for _, banned := range []string{"ARTEX_PG_DSN", "ANTHROPIC_API_KEY", "SOME_FUTURE_TOKEN"} {
		if strings.Contains(got, banned) {
			t.Fatalf("白名单外变量 %s 泄漏进了子进程环境", banned)
		}
	}
	for _, want := range []string{"PATH=", "HOME=", "HTTP_PROXY="} {
		if !strings.Contains(got, want) {
			t.Fatalf("必要变量 %s 缺失", want)
		}
	}
}

func TestToolScriptBaseEnvDropsEmptyValues(t *testing.T) {
	os.Setenv("PATH", "/usr/bin")
	defer os.Unsetenv("PATH")
	for _, kv := range toolScriptBaseEnv() {
		if strings.HasSuffix(kv, "=") {
			t.Fatalf("空值变量不应保留: %q", kv)
		}
	}
}
