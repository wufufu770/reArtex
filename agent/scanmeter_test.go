package agent

// P0 扫描构成埋点的纯函数验收（agent/scanmeter.go）。

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestScanBucketOf(t *testing.T) {
	cases := map[string]string{
		"recent_facts": "facts", "facts": "facts",
		"open_intents": "intents", "running_intents": "intents", "recent_done_intents": "intents",
		"frontier_open": "intents", "done_intents_total": "intents",
		"goals": "goals_hints", "hints": "goals_hints",
		"finding_list": "other", "task": "other", "assets": "other",
	}
	for key, want := range cases {
		if got := scanBucketOf(key); got != want {
			t.Fatalf("scanBucketOf(%q)=%q, want %q", key, got, want)
		}
	}
}

func TestMeterPlannerScanEmitsBuckets(t *testing.T) {
	data := map[string]any{
		"recent_facts":       []map[string]any{{"id": 1, "summary": "abc"}},
		"open_intents":       []map[string]any{{"id": 2}},
		"recent_done_intents": []map[string]any{},
		"goals":              []map[string]any{{"id": 3}},
		"finding_list":       []map[string]any{},
	}
	var got db.Activity
	emit := func(a db.Activity) { got = a }
	MeterPlannerScan(emit, data, 1234)
	if got.Kind != "meter" || got.Worker != "planner" {
		t.Fatalf("meter 活动形状不对: %+v", got)
	}
	buckets := map[string]float64{}
	if err := json.Unmarshal(got.Metadata, &buckets); err != nil {
		t.Fatalf("metadata 不是分桶 JSON: %v", err)
	}
	for _, bucket := range []string{"facts", "intents", "goals_hints", "other", "system"} {
		if _, ok := buckets[bucket]; !ok {
			t.Fatalf("缺桶 %s: %v", bucket, buckets)
		}
	}
	if buckets["system"] != 1234 {
		t.Fatalf("system 桶应为 1234: %v", buckets)
	}
	if buckets["facts"] <= 0 || buckets["goals_hints"] <= 0 {
		t.Fatalf("facts/goals_hints 桶应有字节: %v", buckets)
	}
	if strings.Contains(string(got.Metadata), "recent_facts") {
		t.Fatalf("metadata 应只含桶名，不应泄漏原文")
	}
}

func TestMeterPlannerScanNilSafe(t *testing.T) {
	MeterPlannerScan(nil, nil, 0)       // 不 panic
	MeterPlannerScan(func(db.Activity) {}, nil, 0) // 不 panic
}
