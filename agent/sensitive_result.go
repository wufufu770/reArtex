package agent

// 敏感输出元信息化（0.11.9 桌面控制施工 §6.3）。
//
// 机制按【类别】注册而非为单一工具开特例：命中名单的工具，其 tool_result 正文
// 在持久层（activity.detail）替换为元信息——命令名、字节数、截断时间。模型上下文
// 不受影响（截断只作用于 capture 的落库路径；工具结果流照旧进推理）。
//
// 名单口径（桌面控制施工的三类宿主私有内容）：
//   - desktop_screenshot        —— PNG 全图（base64 体积大且是宿主画面）
//   - desktop_clipboard_get     —— 剪贴板正文（凭据/复制的敏感文本）
//   - desktop_list_notifications—— 通知正文（消息级隐私）
//
// 匹配规则：精确名 + 前缀（mcp__<server>__ 前缀形态与直连裸名形态都命中——
// 同一工具可能以两种 key 出现在活动流里）。

import (
	"fmt"
	"strings"
	"time"
)

// sensitiveResultTools 是敏感输出工具的匹配表：key 为工具名片段，命中即元信息化。
var sensitiveResultTools = []string{
	"desktop_screenshot",
	"desktop_clipboard_get",
	"desktop_list_notifications",
}

// registerSensitiveResultTool 给未来其它类别的敏感输出注册（平台侧统一机制，
// 不为单一工具开特例的口径由此兑现）。
func registerSensitiveResultTool(fragment string) {
	sensitiveResultTools = append(sensitiveResultTools, fragment)
}

// isSensitiveResultTool 判一个工具名（直连名或 mcp__server__tool 全名）是否命中
// 敏感输出名单。
func isSensitiveResultTool(tool string) bool {
	if tool == "" {
		return false
	}
	for _, frag := range sensitiveResultTools {
		if tool == frag || strings.HasSuffix(tool, "__"+frag) || strings.HasSuffix(tool, "."+frag) {
			return true
		}
	}
	return false
}

// sensitiveResultMetadata 构造落库元信息：正文不保留，只留可追溯的壳。
func sensitiveResultMetadata(tool, out string) string {
	return fmt.Sprintf("[敏感输出已元信息化] tool=%s bytes=%d captured_at=%s 正文不入库（宿主私有内容，留存策略见 docs/开发纪律.md）；模型上下文中保留全文。",
		tool, len(out), time.Now().UTC().Format(time.RFC3339))
}
