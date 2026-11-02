package sod

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTruncateUTF8NeverSplitsAMultiByteCharacter 钉住「按字节上限截断时，绝不把一个
// 多字节字符切成两半」。
//
// 为什么这条重要（真机实测抓到的）：截断结果会被写进 PostgreSQL 的 payload_preview
// 列 ⇒ 若留下半个 UTF-8 字符，PG 直接报
// `invalid byte sequence for encoding "UTF8": 0xe8`（SQLSTATE 22021）⇒ **整条裁决
// 记录写不进账本** ⇒ 审计链断裂（而裁决本身是发生了的）。
//
// 关键在于「旧实现识别不了的那一种切法」：切点落在某个中文字符的中间、而紧跟着的
// 下一个字节恰好是**下一个字符的起始字节**。旧实现只看「最后一个字节是不是起始
// 字节」⇒ 看到起始字节就认为完整 ⇒ 停在半个字符上。
func TestTruncateUTF8NeverSplitsAMultiByteCharacter(t *testing.T) {
	// "中文" = E4 B8 AD E6 96 87。maxBytes=4 ⇒ 切点落在第一个字之后、
	// 第二个字的第一个字节（E6）上 ⇒ 旧实现在这里会停下 ⇒ 留下半个字符。
	raw := []byte("中文")
	cases := []struct {
		name     string
		maxBytes int
	}{
		{"切在第一个字中间", 1},
		{"切在第一个字中间（续字节）", 2},
		{"切在第一个字之后、第二个字首字节上", 4},
		{"切在第二个字中间", 5},
		{"正好两个完整字", 6},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := truncateUTF8(raw, c.maxBytes)
			if !utf8.ValidString(got) {
				t.Fatalf("截断结果不是合法 UTF-8：%q（字节 %x）", got, []byte(got))
			}
			if got != "" && !strings.HasPrefix(string(raw), got) {
				t.Fatalf("截断结果必须是原串的前缀：%q", got)
			}
			// 末尾不能是半个字符：逐字节验证最后一段能被完整解码。
			b := []byte(got)
			if len(b) > 0 {
				if _, size := utf8.DecodeLastRune(b); size == 1 && b[len(b)-1] >= 0x80 {
					t.Fatalf("末尾留下了半个字符：%q", got)
				}
			}
		})
	}
}

// TestTruncateUTF8KeepsValidReplacementCharacter 钉住「合法的 U+FFFD（本身就是一个
// 字符）不能被当成残缺而砍掉」。
//
// 为什么要单列：修复用的判据是 DecodeLastRune 返回 (RuneError, 1) 才算「残缺」，
// 而合法的 U+FFFD 返回 (RuneError, 3) ⇒ 若把判据写成「r == RuneError 就回退」，
// 这个字符会被无辜砍掉。
func TestTruncateUTF8KeepsValidReplacementCharacter(t *testing.T) {
	raw := []byte("中\uFFFD文") // E4 B8 AD EF BF BD E6 96 87
	// maxBytes=6 ⇒ 切点正好落在 U+FFFD 之后 ⇒ 它是**完整**的，必须保留。
	got := truncateUTF8(raw, 6)
	if !utf8.ValidString(got) {
		t.Fatalf("结果不是合法 UTF-8：%q", got)
	}
	if !strings.Contains(got, "\uFFFD") {
		t.Fatalf("合法的 U+FFFD 被误删了：%q", got)
	}
	// maxBytes=4 ⇒ 切在 U+FFFD 的**第一个字节**上 ⇒ 它被切成残缺 ⇒ 必须整段回退，
	// 结果只能是前面那个完整的中文字。
	cut := truncateUTF8(raw, 4)
	if !utf8.ValidString(cut) {
		t.Fatalf("结果不是合法 UTF-8：%q", cut)
	}
	if cut != "中" {
		t.Fatalf("切在 U+FFFD 中间时应回退到最后一个完整字符，实际 %q", cut)
	}
}

// TestRedactPreviewAlwaysYieldsValidUTF8 钉住「脱敏预览的输出一定是合法 UTF-8」——
// 这是它能被写进 PG 的前提。
func TestRedactPreviewAlwaysYieldsValidUTF8(t *testing.T) {
	// 预览上限 512 字节 ⇒ 构造一段中文，让切点落在多字节序列中间。
	var sb strings.Builder
	for i := 0; sb.Len() < 700; i++ {
		sb.WriteString("授权范围内的目标资产")
	}
	payload := []byte(sb.String())
	got := RedactPreview(payload, 512)
	if !utf8.ValidString(got) {
		t.Fatalf("脱敏预览不是合法 UTF-8（写库会报 SQLSTATE 22021）：%q", got)
	}
	if len(got) > 512 {
		t.Fatalf("预览长度 %d 超过上限 512", len(got))
	}
}
