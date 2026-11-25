package pathguard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// 入参路径片段的提取与判定。
//
// 为什么要有这一层：短路表是对工具入参 JSON 的【字符串预筛】，挡不住经 shell
// 展开后才现形的路径（`cat $(realpath ../../../jwt.key)`）。本文件把入参里的
// 「路径样片段」挑出来，按工具工作目录解析成真实路径，再走秘密清单判定。
//
// 提取策略：逐个字符串值 → 去掉 URL 形态（scheme://…，其中的 / 不是文件路径）
// → 按 shell 分隔符切成 token → 每个 token 再切出「以 / 或 ../ 或 ~/ 开头」的
// 后缀作为候选路径。之所以按后缀切，是因为 shell 变量与命令替换会把路径
// 粘在别的东西前面（`$HOME/../../jwt.key`、`$(realpath ../../../jwt.key)`）。
var urlLikeRe = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"'` + "`" + `]+`)

// tokenSplitter 是切 token 时要断开的字符：空白与 shell 元字符。
//
// 含 $ 是有意的：变量前缀（$HOME/…）与命令替换（$(…）都要在 $ 处断开，
// 断开后剩下的路径片段才能被单独识别。
const tokenSplitter = " \t\r\n\"'`;|&<>(){}[],$"

// pathStart 判断 token 从该位置起是否像一个路径起点。
func pathStart(s string, i int) bool {
	if i >= len(s) {
		return false
	}
	if s[i] == '/' || s[i] == '~' {
		return true
	}
	// "." 或 ".." 开头（最多两个点，后面必须是 / 或就是结尾）
	if s[i] == '.' {
		j := i
		for j < len(s) && s[j] == '.' {
			j++
		}
		return j-i <= 2 && (j == len(s) || s[j] == '/')
	}
	return false
}

// pathCandidates 从一个 token 里切出所有路径样候选（按出现顺序）。
func pathCandidates(tok string) []string {
	var out []string
	for i := 0; i < len(tok); i++ {
		if !pathStart(tok, i) {
			continue
		}
		cand := strings.TrimRight(tok[i:], ".,;:!?)]}'\"")
		if cand == "" {
			continue
		}
		out = append(out, cand)
	}
	return out
}

// stringValues 递归取出 JSON 里的所有字符串值。
func stringValues(v any, out *[]string) {
	switch t := v.(type) {
	case string:
		*out = append(*out, t)
	case []any:
		for _, e := range t {
			stringValues(e, out)
		}
	case map[string]any:
		for _, e := range t {
			stringValues(e, out)
		}
	}
}

// joinWithBase 把候选路径按工作目录展开成可判定的绝对/相对路径。
//
// 相对路径必须相对工具的工作目录解释：入参里的 `../../../jwt.key` 里的 .. 层数
// 就是按工作目录算的（工作目录 = <data>/sessions/<key>，三层 .. 才回到 /app）。
func joinWithBase(p, base string) string {
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	if !filepath.IsAbs(p) && base != "" {
		p = filepath.Join(base, p)
	}
	return filepath.Clean(p)
}

// CheckInputPaths 判定工具入参里是否携带指向平台秘密的路径。
//
// workingDir 是工具进程的工作目录（norma 的 permission.Context.WorkingDir）；
// 为空时按进程 CWD 处理。
//
// 两段判定：
//  1. 字符串层：把候选路径按工作目录展开并 Clean 后，直接比对秘密清单。
//     命中即拒 —— 这一段不碰文件系统，所以大内容块上也不产生 syscall。
//  2. 真实路径层：统一走 resolveReal（A-11），抓「软链指向密钥」「父目录是软链」
//     「悬空符号链接」这几种字符串层看不出来的形态；不存在路径按最深已存在
//     父目录解析后判定，不再「不存在就跳过」。
func CheckInputPaths(toolName string, input []byte, workingDir string) error {
	var v any
	if err := json.Unmarshal(input, &v); err != nil {
		return nil // 解析不了交给工具自己报错，不因格式问题误伤
	}
	var strs []string
	stringValues(v, &strs)

	seen := make(map[string]bool)
	for _, s := range strs {
		s = urlLikeRe.ReplaceAllString(s, " ")
		for _, tok := range strings.FieldsFunc(s, func(r rune) bool {
			return strings.ContainsRune(tokenSplitter, r)
		}) {
			for _, cand := range tokenCandidates(tok) {
				if seen[cand] {
					continue
				}
				seen[cand] = true

				cleaned := joinWithBase(cand, workingDir)
				if err := denyReason(cleaned); err != nil {
					return fmt.Errorf("拒绝 %s 的路径 %q: %w", toolName, cand, err)
				}
				real, err := resolveReal(cand, workingDir)
				if err != nil {
					return fmt.Errorf("拒绝 %s 的路径 %q: %w", toolName, cand, err)
				}
				if err := denyReason(real); err != nil {
					return fmt.Errorf("拒绝 %s 的路径 %q（真实路径 %s）: %w", toolName, cand, real, err)
				}
			}
		}
	}
	return nil
}

// tokenCandidates 给出单个 token 的全部路径候选。
//
// 有路径起点（/ ~/ ../ ..）的 token 按后缀切出候选（见 pathCandidates）；
// 完全没有路径起点的裸 token 整体作为一个候选（A-11）：`cd /app && cat jwt.key`
// 里的 jwt.key 单独成 token，此前整段被漏掉。裸候选收紧上限——它本质是文件名，
// 超长 token 是压缩文本之类的杂物，不值得为它逐层爬文件系统。
func tokenCandidates(tok string) []string {
	cands := pathCandidates(tok)
	if len(cands) > 0 {
		return cands
	}
	bare := strings.TrimRight(tok, ".,;:!?)]}'\"")
	if bare == "" || len(bare) > 96 || strings.ContainsRune(bare, '/') {
		return nil
	}
	return []string{bare}
}
