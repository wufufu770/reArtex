package agent

import (
	"os"
	"os/exec"
	"testing"
)

// 与 config 的 DSN 同理：进程环境里留着明文 API key，agent 一条 env / printenv
// 就能读走。读入时抹除，缓存保重复调用可用。
func TestEnvAPIKeysUnsetProcessEnv(t *testing.T) {
	envKeysRead = false
	anthKeyCached, oaiKeyCached = "", ""
	t.Cleanup(func() {
		envKeysRead = false
		anthKeyCached, oaiKeyCached = "", ""
	})

	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-secret-value")
	t.Setenv("OPENAI_API_KEY", "sk-oai-secret-value")

	anth, oai := envAPIKeys()
	if anth != "sk-ant-secret-value" || oai != "sk-oai-secret-value" {
		t.Fatalf("首次读取应拿到值，得到 %q %q", anth, oai)
	}
	if os.Getenv("ANTHROPIC_API_KEY") != "" || os.Getenv("OPENAI_API_KEY") != "" {
		t.Fatal("读入后 API key 仍留在进程环境里")
	}
	anth2, oai2 := envAPIKeys()
	if anth2 != anth || oai2 != oai {
		t.Fatalf("缓存未生效: %q %q", anth2, oai2)
	}

	out, err := exec.Command("sh", "-c", "printf '%s|%s' \"${ANTHROPIC_API_KEY:-EMPTY}\" \"${OPENAI_API_KEY:-EMPTY}\"").Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "EMPTY|EMPTY" {
		t.Fatalf("子进程仍能看到 API key: %q", out)
	}
}
