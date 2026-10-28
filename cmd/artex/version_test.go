package main

import (
	"bytes"
	"testing"
)

// printVersion 必须原样输出编译期注入的版本号，供镜像构建期校验二进制身份。
func TestPrintVersionWritesEmbeddedVersion(t *testing.T) {
	old := version
	version = "v-test-1.2.3"
	t.Cleanup(func() { version = old })

	var buf bytes.Buffer
	printVersion(&buf)
	if got, want := buf.String(), "v-test-1.2.3\n"; got != want {
		t.Fatalf("printVersion 输出 %q，期望 %q", got, want)
	}
}
