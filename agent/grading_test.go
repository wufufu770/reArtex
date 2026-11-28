package agent

import (
	"math"
	"os"
	"strings"
	"testing"
)

func TestComputeGradeFullMarksIsS(t *testing.T) {
	got := ComputeGrade(GradeInput{
		GoalsMet: 4, GoalsTotal: 4,
		VerifiedFindings: 12, TotalFindings: 12,
		CoveredAssets: 3, ScopedAssets: 3,
		EvidenceCompleteFindings: 12,
	})
	if got.Grade != GradeS || got.Score != 100 {
		t.Fatalf("全满应是 100 S，得到 %v %v", got.Score, got.Grade)
	}
	want := "4/4 目标达成，12/12 条发现已验证，覆盖率 100%，证据完整率 100%"
	if got.Summary != want {
		t.Fatalf("摘要\n got %q\nwant %q", got.Summary, want)
	}
}

func TestComputeGradeAcceptanceExampleIsA(t *testing.T) {
	// 3/4、9/12、82/100、9/12 → 0.75×40 + 0.75×30 + 0.82×20 + 0.75×10 = 76.4 → A
	got := ComputeGrade(GradeInput{
		GoalsMet: 3, GoalsTotal: 4,
		VerifiedFindings: 9, TotalFindings: 12,
		CoveredAssets: 82, ScopedAssets: 100,
		EvidenceCompleteFindings: 9,
	})
	if got.Grade != GradeA {
		t.Fatalf("76.4 应是 A，得到 %v %v", got.Score, got.Grade)
	}
	if math.Abs(got.Score-76.4) > 1e-9 {
		t.Fatalf("分数应是 76.4，得到 %v", got.Score)
	}
	want := "3/4 目标达成，9/12 条发现已验证，覆盖率 82%，证据完整率 75%"
	if got.Summary != want {
		t.Fatalf("摘要\n got %q\nwant %q", got.Summary, want)
	}
}

func TestComputeGradeThresholds(t *testing.T) {
	cases := []struct {
		name  string
		in    GradeInput
		grade string
		score float64
	}{
		{
			name: "exactly 90 is S",
			in: GradeInput{
				GoalsMet: 4, GoalsTotal: 4,
				VerifiedFindings: 12, TotalFindings: 12,
				CoveredAssets: 100, ScopedAssets: 100,
				EvidenceCompleteFindings: 0,
			},
			grade: GradeS, score: 90,
		},
		{
			name: "exactly 75 is A",
			in: GradeInput{
				GoalsMet: 4, GoalsTotal: 4,
				VerifiedFindings: 10, TotalFindings: 10,
				CoveredAssets: 1, ScopedAssets: 4,
				EvidenceCompleteFindings: 0,
			},
			grade: GradeA, score: 75,
		},
		{
			name: "exactly 60 is B",
			in: GradeInput{
				GoalsMet: 1, GoalsTotal: 2,
				VerifiedFindings: 10, TotalFindings: 10,
				CoveredAssets: 1, ScopedAssets: 2,
				EvidenceCompleteFindings: 0,
			},
			grade: GradeB, score: 60,
		},
		{
			name: "below 60 is C",
			in: GradeInput{
				GoalsMet: 1, GoalsTotal: 2,
				VerifiedFindings: 0, TotalFindings: 10,
				CoveredAssets: 1, ScopedAssets: 2,
				EvidenceCompleteFindings: 0,
			},
			grade: GradeC, score: 30,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeGrade(tc.in)
			if got.Grade != tc.grade || math.Abs(got.Score-tc.score) > 1e-9 {
				t.Fatalf("got %v %s, want %v %s", got.Score, got.Grade, tc.score, tc.grade)
			}
		})
	}
}

func TestComputeGradeZeroDenominatorIsNotFullMarks(t *testing.T) {
	got := ComputeGrade(GradeInput{})
	if got.Grade != GradeC || got.Score != 0 {
		t.Fatalf("分母全 0 应是 0 分 C，得到 %v %s", got.Score, got.Grade)
	}
	want := "0/0 目标达成，0/0 条发现已验证，覆盖率 0%，证据完整率 0%（无目标按 0 计；无发现按 0 计；无范围资产按 0 计）"
	if got.Summary != want {
		t.Fatalf("摘要\n got %q\nwant %q", got.Summary, want)
	}
	// 负分母同样按 0，不 panic。
	neg := ComputeGrade(GradeInput{GoalsMet: 1, GoalsTotal: -1, VerifiedFindings: 1, TotalFindings: -3, CoveredAssets: 1, ScopedAssets: -2})
	if neg.Score != 0 || neg.Grade != GradeC {
		t.Fatalf("负分母应是 0 分 C，得到 %v %s", neg.Score, neg.Grade)
	}
}

func TestComputeGradeIsDeterministic(t *testing.T) {
	in := GradeInput{GoalsMet: 3, GoalsTotal: 4, VerifiedFindings: 9, TotalFindings: 12, CoveredAssets: 82, ScopedAssets: 100, EvidenceCompleteFindings: 9}
	a := ComputeGrade(in)
	b := ComputeGrade(in)
	if a != b {
		t.Fatalf("同一输入两次结果不同：%+v vs %+v", a, b)
	}
}

func TestComputeGradeDoesNotClampNumerator(t *testing.T) {
	// 公式按卡面不封顶。分子大于分母时分数可以超过 100，等级仍是 S。
	got := ComputeGrade(GradeInput{GoalsMet: 2, GoalsTotal: 1, VerifiedFindings: 1, TotalFindings: 1, CoveredAssets: 1, ScopedAssets: 1, EvidenceCompleteFindings: 1})
	if got.Score != 140 || got.Grade != GradeS {
		t.Fatalf("2/1 目标应把分数推到 140 S，得到 %v %s", got.Score, got.Grade)
	}
}

func TestCountEvidenceCompleteUsesFindingGate(t *testing.T) {
	// 没填自报等级（0）的调用按存量：仍只看 finding_gate。
	findings := []GradeFinding{
		{Summary: "SQL 注入可读取他人数据", Evidence: "GET /api?id=1 HTTP/1.1\n200 OK"},
		{Summary: "SQL 注入可读取他人数据", Evidence: "   "},
		{Summary: "这不是漏洞", Evidence: "GET /api?id=1 HTTP/1.1\n200 OK"},
		{Summary: "未验证的注入点", Evidence: "GET /api?id=1 HTTP/1.1\n200 OK"},
	}
	if n := CountEvidenceComplete(findings); n != 1 {
		t.Fatalf("只有第一条过门，得到 %d", n)
	}
	if n := CountEvidenceComplete(nil); n != 0 {
		t.Fatalf("空列表应是 0，得到 %d", n)
	}
}

func TestCountEvidenceCompleteLegacyKeepsGate(t *testing.T) {
	good := "GET /api?id=1 HTTP/1.1\n200 OK"
	legacy := func(summary, evidence string, level int) GradeFinding {
		return GradeFinding{
			Summary: summary, Evidence: evidence,
			EvidenceLevel: level, EvidenceLevelRequested: 1,
		}
	}
	cases := []struct {
		name string
		f    GradeFinding
		want bool
	}{
		{"默认 L1 且过门", legacy("SQL 注入可读取他人数据", good, 1), true},
		{"空白降级原因仍按存量", GradeFinding{Summary: "SQL 注入可读取他人数据", Evidence: good, EvidenceLevel: 1, EvidenceLevelRequested: 1, EvidenceLevelReason: "  "}, true},
		{"默认 L1 但证据为空", legacy("SQL 注入可读取他人数据", "  ", 1), false},
		{"默认 L1 但结论自否", legacy("这不是漏洞", good, 1), false},
		// 只改了落库等级、自报仍是默认且没有原因：不是 C-04 写入标记，仍过门。
		{"自报仍是 L1 时不看落库等级", legacy("这不是漏洞", good, 4), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CountEvidenceComplete([]GradeFinding{tc.f})
			want := 0
			if tc.want {
				want = 1
			}
			if got != want {
				t.Fatalf("got %d want %d", got, want)
			}
		})
	}
}

func TestCountEvidenceCompleteNewUsesLevel(t *testing.T) {
	good := "GET /api?id=1 HTTP/1.1\n200 OK"
	cases := []struct {
		name string
		f    GradeFinding
		want bool
	}{
		{
			"自报 L3 且落到 L3，结论自否也算完整",
			GradeFinding{Summary: "这不是漏洞", Evidence: good, EvidenceLevel: 3, EvidenceLevelRequested: 3},
			true,
		},
		{
			"自报 L2 落到 L2，过门也不算完整",
			GradeFinding{Summary: "SQL 注入可读取他人数据", Evidence: good, EvidenceLevel: 2, EvidenceLevelRequested: 2},
			false,
		},
		{
			"有降级原因且落到 L2",
			GradeFinding{Summary: "SQL 注入可读取他人数据", Evidence: good, EvidenceLevel: 2, EvidenceLevelRequested: 4, EvidenceLevelReason: "自报证据等级 L4，可验证上限为 L2"},
			false,
		},
		{
			"有降级原因且落到 L3，证据正文为空也算完整",
			GradeFinding{Summary: "SQL 注入可读取他人数据", Evidence: "", EvidenceLevel: 3, EvidenceLevelRequested: 4, EvidenceLevelReason: "自报证据等级 L4，可验证上限为 L3"},
			true,
		},
		{
			"自报 L4 落到 L4",
			GradeFinding{Summary: "SQL 注入可读取他人数据", Evidence: good, EvidenceLevel: 4, EvidenceLevelRequested: 4},
			true,
		},
		{
			"自报仍是 L1 但写了降级原因，落到 L3",
			GradeFinding{Summary: "SQL 注入可读取他人数据", Evidence: "", EvidenceLevel: 3, EvidenceLevelRequested: 1, EvidenceLevelReason: "走过上限"},
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CountEvidenceComplete([]GradeFinding{tc.f})
			want := 0
			if tc.want {
				want = 1
			}
			if got != want {
				t.Fatalf("got %d want %d", got, want)
			}
		})
	}
}

func TestCountEvidenceCompleteMixed(t *testing.T) {
	good := "GET /api?id=1 HTTP/1.1\n200 OK"
	findings := []GradeFinding{
		{Summary: "SQL 注入可读取他人数据", Evidence: good, EvidenceLevel: 1, EvidenceLevelRequested: 1},
		{Summary: "待补证据", Evidence: "", EvidenceLevel: 1, EvidenceLevelRequested: 1},
		{Summary: "这不是漏洞", Evidence: good, EvidenceLevel: 3, EvidenceLevelRequested: 3},
		{Summary: "SQL 注入可读取他人数据", Evidence: good, EvidenceLevel: 2, EvidenceLevelRequested: 4, EvidenceLevelReason: "降到 L2"},
		{Summary: "SQL 注入可读取他人数据", Evidence: good, EvidenceLevel: 2, EvidenceLevelRequested: 2},
		{Summary: "未授权访问可导出用户列表", Evidence: good, EvidenceLevel: 4, EvidenceLevelRequested: 4},
	}
	// 新口径 3 条：存量过门、自报 L3、落到 L4。两条 L2 不算。旧门会把两条 L2 算进去、把自否的 L3 排除，得到 4。
	if n := CountEvidenceComplete(findings); n != 3 {
		t.Fatalf("混合任务应有 3 条完整，得到 %d", n)
	}
}

func TestGradingSourceDoesNotCallLLM(t *testing.T) {
	src, err := os.ReadFile("grading.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	// 只查导入和调用形态。函数名 CountEvidenceComplete 含 Complete，不能拿子串误伤。
	for _, bad := range []string{`"github.com/Autumn-27/artex/llm"`, "llm.", "openai", "anthropic", "net/http", ".Complete("} {
		if strings.Contains(text, bad) {
			t.Fatalf("grading.go 不应出现 %q", bad)
		}
	}
}

func TestValidGrade(t *testing.T) {
	for _, ok := range []string{GradeS, GradeA, GradeB, GradeC} {
		if !ValidGrade(ok) {
			t.Fatalf("%s 应合法", ok)
		}
	}
	for _, bad := range []string{"", "s", "D", "medium", "auto"} {
		if ValidGrade(bad) {
			t.Fatalf("%q 不应合法", bad)
		}
	}
}
