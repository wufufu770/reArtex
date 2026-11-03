package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCompactionTriggerRatioEnv pins the parsing of ARTEX_COMPACT_TRIGGER_RATIO.
//
// A malformed or out-of-range value must be IGNORED, not applied: a typo must not
// silently change when the engine decides to compress the conversation.
func TestCompactionTriggerRatioEnv(t *testing.T) {
	const env = "ARTEX_COMPACT_TRIGGER_RATIO"

	cases := []struct {
		name string
		val  string
		want float64
	}{
		{"unset keeps the SDK default", "", 0},
		{"blank keeps the SDK default", "   ", 0},
		{"a valid ratio is applied", "0.6", 0.6},
		{"surrounding spaces are tolerated", " 0.75 ", 0.75},
		{"a ratio of 1 is out of range", "1", 0},
		{"a ratio above 1 is out of range", "1.5", 0},
		{"a ratio of 0 keeps the SDK default", "0", 0},
		{"a negative ratio is out of range", "-0.5", 0},
		{"garbage is ignored", "early", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.val != "" {
				t.Setenv(env, tc.val)
			} else {
				os.Unsetenv(env)
			}
			cfg := compactionConfig(200000)
			if cfg.AutoCompactRatio != tc.want {
				t.Fatalf("AutoCompactRatio = %v, want %v", cfg.AutoCompactRatio, tc.want)
			}
		})
	}
}

// TestCompactionTriggerOnlyMovesEarlier guards the vendored-SDK contract that the
// AutoCompact trigger can be moved EARLIER but never LATER.
//
// The SDK is vendored under third_party/norma, so every change to it lives as a
// patch under patches/. A patch can be lost or rewritten without any compile
// error, so the invariant itself has to be asserted against the generated tree.
func TestCompactionTriggerOnlyMovesEarlier(t *testing.T) {
	path := filepath.Join("..", "third_party", "norma", "compaction", "compaction.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读不到 %s（先跑 scripts/prepare-norma.sh）: %v", path, err)
	}
	src := string(raw)

	for _, want := range []string{
		"AutoCompactRatio", // the knob exists
		"t < base",         // the ratio may only lower the threshold
		"return base",      // and otherwise the default is kept
	} {
		if !strings.Contains(src, want) {
			t.Errorf("%s 里找不到 %q：vendored SDK 的压缩触发补丁丢了？", path, want)
		}
	}
}
