package agent

import (
	"embed"
	"io/fs"
	"log"
)

// 本文件把「顾问型 agent」的提示词正文从磁盘文件装载进内置提示词目录。
//
// 为什么单独成文件：这五个 agent（curator / verifier / redteam / creative /
// intent-normalizer）的提示词是【长篇规约】——身份、铁律、输入、输出 JSON、
// 禁止事项、失败处理六段齐全，写成 Go 字符串常量既难读又难改（一处引号转义就能
// 改坏语义）。文件是它们的唯一事实来源：改文件即改种子，编译期随二进制一起分发
// （//go:embed），运行期不读盘。
//
// 提示词文件是【输入】不是【输出】：本文件只读不改。agent/prompts/ 下还有别的阶段
// 的提示词文件，不属于本组 agent，这里不登记它们。

//go:embed prompts/*.md
var advisoryPromptFS embed.FS

// advisoryPromptFiles 把 agent key 映射到提示词文件（相对 embed.FS 的路径）。
//
// 只有这五个进入种子目录。文件缺失不会让编译失败（embed 只保证 glob 命中了至少
// 一个文件），所以映射写错这种低级错误由测试钉住（见 prompts_files_test.go）。
//
// 权威是这张映射表，不是 //go:embed 的 glob：glob 会把 prompts/ 下所有 .md 的字节
// 都收进二进制（包括不登记的规范文件），但登记不登记只由这张表说了算。
var advisoryPromptFiles = map[string]string{
	"curator":           "prompts/curator.md",
	"verifier":          "prompts/verifier.md",
	"redteam":           "prompts/redteam.md",
	"creative":          "prompts/creative.md",
	"intent_normalizer": "prompts/intent-normalizer.md",
}

// 下面这些文件【刻意不登记】为 agent：
//
//   - prompts/policy-gate.md —— 它是【确定性闸门的规格】，不是 LLM 提示词。
//     policy gate 的全部检查都是确定性的：scope 检查、tier2 一致性、工具熔断、
//     群组额度状态、anomaly 标记；它的裁决顺序是一条短路规则表（scope 失败 →
//     deny；工具熔断 → deny；额度耗尽 → deny；tier2 不一致 → deny；anomaly →
//     needs_review；全过 → approve）。刻意不登记成 agent，是因为项目的铁律是
//     「模型只提议，确定性层裁决」：登记成 agent 就等于让模型来做本该由代码做的
//     裁决，职责分离当场破掉。故：无 agent 行、无工具绑定、提示词不可达。
//   - executor / auditor —— 同样是确定性基础设施（执行已批准的意图、审计执行结果），
//     没有提示词文件、没有 agent 行、没有工具绑定。

// advisoryPromptSeeds 在包初始化时装载五个提示词正文。
var advisoryPromptSeeds = loadAdvisoryPrompts()

// loadAdvisoryPrompts 读出五个提示词文件。读不到就跳过该 key 并记日志，不在初始化
// 阶段 panic：单个提示词缺失不该让整个服务起不来（该 agent 会退回服务端默认提示词
// 路径）。路径写错由测试兜住，避免「某个 agent 悄悄没有提示词」。
func loadAdvisoryPrompts() map[string]string {
	out := make(map[string]string, len(advisoryPromptFiles))
	for key, path := range advisoryPromptFiles {
		b, err := fs.ReadFile(advisoryPromptFS, path)
		if err != nil {
			log.Printf("[prompts] 装载 %s 失败(%s): %v", key, path, err)
			continue
		}
		out[key] = string(b)
	}
	return out
}

// AdvisoryPromptSeeds 返回五个顾问型 agent 的内置提示词正文，按 agent key 索引。
//
// 返回副本：调用方（BuiltinPromptSeeds）把它并进总目录，不该让外部改到包内状态。
func AdvisoryPromptSeeds() map[string]string {
	out := make(map[string]string, len(advisoryPromptSeeds))
	for k, v := range advisoryPromptSeeds {
		out[k] = v
	}
	return out
}

// promptKeyAliases 是查找用的别名 → 规范 agent key。
//
// 存在的唯一原因：agents 表的 key 列有 CHECK (key ~ '^[a-z][a-z0-9_]*$')
// （db/schema.sql），带连字符的 key 插不进库——db.Open 会直接失败，整平台起不来。
// 所以规范 key 只能是 intent_normalizer，而「intent-normalizer」是文件名与阶段名
// （agent/prompts/intent-normalizer.md，policy-gate.md 里也这么写）。查找时两种
// 写法都应解析到同一个 agent，否则连字符写法就查不到任何东西。
//
// 别名只影响【查找】，不进入【枚举】：种子目录里只有规范 key，播种时才不会去查
// 一个不存在的 agent 行。
var promptKeyAliases = map[string]string{
	"intent-normalizer": "intent_normalizer",
}

// BuiltinPromptBody 按 agent key 查内置提示词正文，查不到返回 false。
//
// 别名（连字符写法）同样能解析。规范 key 不在目录里就别硬造——查不到就是查不到。
func BuiltinPromptBody(agentKey string) (string, bool) {
	if k, ok := promptKeyAliases[agentKey]; ok {
		agentKey = k
	}
	body, ok := BuiltinPromptSeeds()[agentKey]
	return body, ok
}
