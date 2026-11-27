package agent

// 任务难度建议（C-01B）。
//
// 这是任务难度 easy/medium/hard/expert，不是本文件同包的执行档 TaskDifficulty
// （auto/simple/medium/hard，0.15.17，只调算力）。两套枚举的常量名不能重用：
// DifficultyMedium / DifficultyHard 已经被执行档占用，所以这里的档位用字符串
// 常量 Level*，函数名仍按卡面叫 SuggestDifficulty / ValidDifficulty。
//
// 规则是确定性关键词，不调 LLM。建议只回给调用方，不自动落库——用户不传时
// 存储层默认 medium，用户显式覆盖以用户值为准。
//
// DefaultExecutionMode 在 execution_profile.go（C-02A）：把本文件的任务难度
// 映射成资源模式 fast/balanced/super_heavy。这里只建议难度，不决定预算。

import "strings"

const (
	LevelEasy   = "easy"
	LevelMedium = "medium"
	LevelHard   = "hard"
	LevelExpert = "expert"
)

// 命中顺序就是理由里的列举顺序。expert 先于 hard，hard 先于 easy。
var (
	levelExpertKeywords = []string{"内网", "横向", "提权", "多阶段"}
	levelHardKeywords   = []string{"认证", "登录态", "支付", "多域名", "WAF", "绕过"}
	levelEasyKeywords   = []string{"收集", "枚举", "指纹", "梳理"}
	// 卡面：纯信息收集还要求「无认证/支付词」。登录态/WAF/绕过本身已在 hard 列表里。
	levelEasyBlockedBy = []string{"认证", "支付"}
)

// ValidDifficulty 报告 s 是否为任务难度四值。大小写敏感，不接受执行档的 auto/simple。
// 不 TrimSpace——调用方先规范化。必须与 db.ValidTaskDifficulty 的四值一致。
func ValidDifficulty(s string) bool {
	switch s {
	case LevelEasy, LevelMedium, LevelHard, LevelExpert:
		return true
	}
	return false
}

// SuggestDifficulty 按目标与描述给出建议难度和一句话理由。两侧都会看。
// 多类关键词同时出现时取更高档：expert > hard > easy > medium。
func SuggestDifficulty(goal, description string) (string, string) {
	text := goal + "\n" + description
	if hit := matchedKeywords(text, levelExpertKeywords); len(hit) > 0 {
		return LevelExpert, "目标含" + strings.Join(hit, "、") + "，需多阶段推进"
	}
	if hit := matchedKeywords(text, levelHardKeywords); len(hit) > 0 {
		return LevelHard, "目标含" + strings.Join(hit, "、") + "，需绕过鉴权或额外防护"
	}
	if hit := matchedKeywords(text, levelEasyKeywords); len(hit) > 0 && len(matchedKeywords(text, levelEasyBlockedBy)) == 0 {
		return LevelEasy, "目标为纯信息收集（" + strings.Join(hit, "、") + "），无认证或支付要求"
	}
	return LevelMedium, "未命中高难度或纯信息收集特征，按中等难度建议"
}

func matchedKeywords(text string, keywords []string) []string {
	var hit []string
	folded := strings.ToLower(text)
	for _, kw := range keywords {
		if strings.Contains(text, kw) || (kw == "WAF" && strings.Contains(folded, "waf")) {
			hit = append(hit, kw)
		}
	}
	return hit
}
