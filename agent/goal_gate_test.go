package agent

import (
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestGateTaskCompletion(t *testing.T) {
	one := []db.UnmetGoal{{NodeID: 7, Text: "拿下旗标", Criteria: "读到 flag", Reason: db.UnmetGoalReason}}
	cases := []struct {
		name      string
		unmet     []db.UnmetGoal
		want      string
		got       string
		keepUnmet bool
	}{
		{name: "没有目标可记 done", want: "done", got: "done"},
		{name: "空清单可记 done", unmet: []db.UnmetGoal{}, want: "done", got: "done"},
		{name: "未达成不许记 done", unmet: one, want: "done", got: "timeout", keepUnmet: true},
		{name: "到点仍是 timeout", unmet: one, want: "timeout", got: "timeout", keepUnmet: true},
		{name: "失败不被改成 timeout", unmet: one, want: "failed", got: "failed", keepUnmet: true},
		{name: "暂停不被改写", unmet: one, want: "paused", got: "paused", keepUnmet: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, unmet := GateTaskCompletion(tc.unmet, tc.want)
			if status != tc.got {
				t.Fatalf("状态 %s，想要 %s", status, tc.got)
			}
			if tc.keepUnmet {
				if len(unmet) != 1 || unmet[0].NodeID != 7 || unmet[0].Criteria != "读到 flag" {
					t.Fatalf("未达成清单 = %+v", unmet)
				}
				return
			}
			if unmet != nil {
				t.Fatalf("没有未达成目标时不应带回清单：%+v", unmet)
			}
		})
	}
}
