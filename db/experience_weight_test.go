package db

import (
	"strings"
	"testing"
)

// 经验权重卡（X1，方案 v10 §6 T1）的验收用例 —— 钉住验收清单的四例：
//
//	① 命中升：词集因果校验通过的事件把 prior 抬上去；
//	② 未命中降：无交集（或光标记）事件把 prior 拉下来；
//	③ 同日第 4 次不计胜：cap=3，第 4 次 win 候选只记 hit；
//	④ 无交集只记 hit：归因串与结论零交集 ⇒ wins 不动、hits 只增。
//
// 连库方式照 db 包现有派生库范式（memoryRetrievalOpen 同款；无库时跳过）。

func experienceWeightOpen(t *testing.T) *DB {
	t.Helper()
	d, err := Open(memoryTestDSN(t))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.EnsureExperienceWeightSchema(); err != nil {
		t.Fatalf("应用经验权重 schema 失败: %v", err)
	}
	return d
}

// experienceWinAttribution 构造一段「能过因果校验」的归因行 + 结论。
// 归因行的实质词（nginx/指纹）与结论共词 ⇒ 交集 ≥1。
func experienceWinAttribution(patternKey string) (attribution, conclusion string) {
	attribution = "used_experience:" + patternKey + " 本条经验的 nginx 指纹判读适用本目标"
	conclusion = "结论：目标站点的 nginx 指纹与经验一致，采用同款探测路径完成验证"
	return attribution, conclusion
}

func TestExperienceWeightWinRaisesPrior(t *testing.T) {
	d := experienceWeightOpen(t)
	key := ExperiencePatternKeyForLegacy(910001)

	// 无卡 ⇒ 回落默认 prior（同一公式的 fresh 值）。
	fresh, exists, err := d.ExperienceWeightStats(key)
	if err != nil {
		t.Fatalf("读空卡失败: %v", err)
	}
	if exists {
		t.Fatalf("不该有权重卡：%v", fresh)
	}
	def := ExperienceDefaultPrior()

	attr, conclusion := experienceWinAttribution(key)
	win, prior, err := d.RecordExperienceOutcome(key, attr, conclusion)
	if err != nil {
		t.Fatalf("记账失败: %v", err)
	}
	if !win {
		t.Fatalf("共词归因应当计胜")
	}
	if prior <= def {
		t.Fatalf("命中应升：prior=%f，默认=%f", prior, def)
	}

	// 与公式对账（事件前 wins=0,hits=0,dw=1 ⇒ 2/3）。唯一公式之外的
	// 数字只出现在测试锚点里，实现侧没有第二份算式。
	if want := 2.0 / 3.0; prior < want-1e-9 || prior > want+1e-9 {
		t.Errorf("prior = %f，期望 2/3", prior)
	}

	stats, exists, err := d.ExperienceWeightStats(key)
	if err != nil || !exists {
		t.Fatalf("读卡失败: %v（exists=%v）", err, exists)
	}
	if stats.Wins != 1 || stats.Hits != 1 {
		t.Errorf("wins/hits = %d/%d，期望 1/1", stats.Wins, stats.Hits)
	}
	if stats.LastWinAt == nil {
		t.Error("计胜必须刷新 last_win_at")
	}
}

func TestExperienceWeightMissLowersPrior(t *testing.T) {
	d := experienceWeightOpen(t)
	key := ExperiencePatternKeyForLegacy(910002)

	// 先记一次胜（升到 2/3），再记一次未命中（dw=0 ⇒ (1+0+1)/(0+3)=2/3？
	// 注意：hits_old 是事件前的 0 ⇒ 未命中 prior=(1+1)/3=2/3 —— 同值。
	// 所以这里先记一次胜、再记一次胜之后的无交集未命中：
	// 事件序列 win,win,miss ⇒ (0,0)→win prior=2/3 wins=1 hits=1
	//                        →win prior=(1+1+1)/(1+3)=3/4 wins=2 hits=2
	//                        →miss prior=(2+0+1)/(2+3)=3/5 —— 降。
	attr, conclusion := experienceWinAttribution(key)
	_, prior1, err := d.RecordExperienceOutcome(key, attr, conclusion)
	if err != nil {
		t.Fatalf("第一次记账失败: %v", err)
	}
	_ = prior1
	_, prior2, err := d.RecordExperienceOutcome(key, attr, conclusion)
	if err != nil {
		t.Fatalf("第二次记账失败: %v", err)
	}
	_, prior3, err := d.RecordExperienceOutcome(key, "", "")
	if err != nil {
		t.Fatalf("未命中记账失败: %v", err)
	}
	if prior3 >= prior2 {
		t.Fatalf("未命中应降：prior %f 未低于 %f", prior3, prior2)
	}
	stats, _, err := d.ExperienceWeightStats(key)
	if err != nil {
		t.Fatalf("读卡失败: %v", err)
	}
	if stats.Wins != 2 || stats.Hits != 3 {
		t.Errorf("wins/hits = %d/%d，期望 2/3（hits 只增、未命中不计胜）", stats.Wins, stats.Hits)
	}
}

func TestExperienceWeightDailyCapBlocksFourthWin(t *testing.T) {
	d := experienceWeightOpen(t)
	key := ExperiencePatternKeyForMemory(910003)
	attr, conclusion := experienceWinAttribution(key)

	// 同日三连胜：cap=3。
	for i := 0; i < ExperienceDailyWinCap; i++ {
		win, _, err := d.RecordExperienceOutcome(key, attr, conclusion)
		if err != nil {
			t.Fatalf("第 %d 次记账失败: %v", i+1, err)
		}
		if !win {
			t.Fatalf("第 %d 次应当计胜（cap=%d）", i+1, ExperienceDailyWinCap)
		}
	}
	stats, _, err := d.ExperienceWeightStats(key)
	if err != nil {
		t.Fatalf("读卡失败: %v", err)
	}
	if stats.Wins != ExperienceDailyWinCap || stats.DayWins != ExperienceDailyWinCap {
		t.Fatalf("三次后 wins/day_wins = %d/%d，期望 %d/%d",
			stats.Wins, stats.DayWins, ExperienceDailyWinCap, ExperienceDailyWinCap)
	}

	// 同日第 4 次：候选仍过因果校验，但只记 hit 不计胜。
	win, _, err := d.RecordExperienceOutcome(key, attr, conclusion)
	if err != nil {
		t.Fatalf("第 4 次记账失败: %v", err)
	}
	if win {
		t.Error("同日第 4 次不该计胜（cap=3）")
	}
	stats, _, _ = d.ExperienceWeightStats(key)
	if stats.Wins != ExperienceDailyWinCap {
		t.Errorf("第 4 次后 wins = %d，期望仍为 %d（只记 hit）", stats.Wins, ExperienceDailyWinCap)
	}
	if stats.Hits != 4 {
		t.Errorf("第 4 次后 hits = %d，期望 4（hits 只增）", stats.Hits)
	}

	// 跨日窗口重置：把 day_key 拨回昨天 ⇒ 下一次事件重新计胜。
	if _, err := d.Exec(`UPDATE experience_weight SET day_key='2000-01-01', day_wins=3 WHERE pattern_key=$1`, key); err != nil {
		t.Fatalf("拨 day_key 失败: %v", err)
	}
	win, _, err = d.RecordExperienceOutcome(key, attr, conclusion)
	if err != nil {
		t.Fatalf("跨日记账失败: %v", err)
	}
	if !win {
		t.Error("跨日窗口应重置并重新计胜")
	}
}

func TestExperienceWeightNoIntersectionRecordsHitOnly(t *testing.T) {
	d := experienceWeightOpen(t)
	key := ExperiencePatternKeyForLegacy(910004)

	// 光标记：归因行除样板词外没有任何实质词 ⇒ 与结论零交集 ⇒ 只记 hit。
	win, prior, err := d.RecordExperienceOutcome(key, "used_experience:"+key, "结论：识别了目标站点的技术栈与响应特征")
	if err != nil {
		t.Fatalf("记账失败: %v", err)
	}
	if win {
		t.Error("光标记归因不该计胜（无交集）")
	}
	if prior != ExperienceDefaultPrior() {
		t.Errorf("fresh 行未命中 prior = %f，期望回落默认 %f", prior, ExperienceDefaultPrior())
	}
	stats, exists, err := d.ExperienceWeightStats(key)
	if err != nil || !exists {
		t.Fatalf("读卡失败: %v（exists=%v）", err, exists)
	}
	if stats.Wins != 0 || stats.Hits != 1 {
		t.Errorf("wins/hits = %d/%d，期望 0/1（无交集只记 hit）", stats.Wins, stats.Hits)
	}
	if stats.LastWinAt != nil {
		t.Error("无交集不该刷新 last_win_at")
	}
}

// TestExperienceAttributionValidation 纯函数侧的因果校验用例（不连库）。
func TestExperienceAttributionValidation(t *testing.T) {
	key := "legacy_42"
	if !ValidateExperienceAttribution(key,
		"used_experience:legacy_42 经验里的 nginx 指纹判断适用",
		"结论：nginx 指纹与经验一致") {
		t.Error("共词归因应当通过")
	}
	if ValidateExperienceAttribution(key, "used_experience:legacy_42", "结论：nginx 指纹一致") {
		t.Error("光标记不该通过（样板词剔除后无交集）")
	}
	if ValidateExperienceAttribution(key,
		"used_experience:legacy_42 完全无关的描述",
		"结论：nginx 指纹一致") {
		t.Error("无共词不该通过")
	}
	if ValidateExperienceAttribution(key, "", "结论：nginx") {
		t.Error("空归因行不该通过")
	}
	if ValidateExperienceAttribution("", "used_experience: nginx", "nginx") {
		t.Error("空 pattern_key 不该通过")
	}
	got := ExtractExperienceAttributions("先 used_experience:legacy_7 后 used_experience:memory_9 再 used_experience:legacy_7")
	if len(got) != 2 || got[0] != "legacy_7" || got[1] != "memory_9" {
		t.Errorf("归因提取去重保序失败：%v", got)
	}
	if len(ExtractExperienceAttributions("没有归因串的结论")) != 0 {
		t.Error("无归因串应返回空")
	}
	if !strings.Contains("used_experience:legacy_1", ExperienceAttributionMarker) {
		t.Error("标记常量漂移")
	}
}
