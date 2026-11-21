package server

// trusted_record 跨语言签名原语（0.15.32）：与 veto-eval/amber-memory 共享的
// canonical JSON + HMAC-SHA256 签名，对齐 veto-eval/testdata/ 里的 Python 侧
// 向量（由 veto.trusted_record 生成——canonical 规则钉死：
// ensure_ascii=False + sort_keys + separators=(',',':') + allow_nan=False）。
//
// 为什么自写编码器而不是 encoding/json.Marshal：① Go 默认 HTML 转义（<>&→\uXXXX）
// 与 Python 不同；② U+2028/29 Go 强制转义、Python 原样；③ 大整数经 float64 丢精度。
// 这里逐字节复刻 Python json.dumps 的语义：字符串只转义 `"` `\` 与控制字符
// （\b\f\n\r\t 用短转义、其余 <0x20 用 \uXXXX），非 ASCII 原样 UTF-8，键按码点
// （=UTF-8 字节序）排序，整数经 json.Number 保留原字面。
//
// 用途：Veto evaluate 记录（responses/record/gate.json）的可信签名——key 不出
// 环境变量，签名覆盖除自身 signature 外的全部字段（篡改任意字段即验签失败）。

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

// TrustedSignaturePrefix 是签名值的形态前缀（与 Python 端 SIGNATURE_PREFIX 一致）。
const TrustedSignaturePrefix = "hmac-sha256:"

// TrustedCanonical 把 v 编码为 canonical JSON 字节（Python json.dumps 逐字节等价）。
func TrustedCanonical(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := trustedEncode(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// TrustedDigest 是 canonical 形态的 SHA-256（64 位小写 hex）。
func TrustedDigest(v any) (string, error) {
	b, err := TrustedCanonical(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// TrustedSign 计算 record 的签名（剥离自身 signature 字段后 HMAC-SHA256）。
// record 原地不变。
func TrustedSign(record map[string]any, key []byte) (string, error) {
	if len(key) == 0 {
		return "", fmt.Errorf("签名密钥不能为空")
	}
	stripped := make(map[string]any, len(record))
	for k, v := range record {
		if k == "signature" {
			continue
		}
		stripped[k] = v
	}
	b, err := TrustedCanonical(stripped)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(b)
	return TrustedSignaturePrefix + hex.EncodeToString(mac.Sum(nil)), nil
}

// TrustedVerify 校验 record 的签名（nil/false = 不通过；篡改任意字段即失败）。
func TrustedVerify(record map[string]any, key []byte) bool {
	sig, _ := record["signature"].(string)
	if sig == "" || len(key) == 0 {
		return false
	}
	want, err := TrustedSign(record, key)
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(want))
}

// ── canonical 编码器（Python json.dumps 语义复刻）────────────────────────────

func trustedEncode(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
		return nil
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
		return nil
	case string:
		trustedEncodeString(buf, t)
		return nil
	case json.Number:
		// UseNumber 反序列化的原字面——Python 端 int/float 的字面原样保留。
		// allow_nan=False 语义：NaN/Infinity 字面直接拒绝。
		switch t.String() {
		case "NaN", "Infinity", "-Infinity":
			return fmt.Errorf("canonical JSON 拒绝非有限数字: %s", t.String())
		}
		buf.WriteString(t.String())
		return nil
	case float64:
		// 非 UseNumber 路径的兜底：按 Go 最短表示输出。
		return trustedEncode(buf, json.Number(fmtFormatFloat(t)))
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := trustedEncode(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys) // UTF-8 字节序 = Python 码点序
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			trustedEncodeString(buf, k)
			buf.WriteByte(':')
			if err := trustedEncode(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
		return nil
	default:
		// 其他 Go 类型先过一遍 json（不带 HTML 转义）再入编码器。
		raw, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("不支持的类型 %T: %w", v, err)
		}
		var dec any
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		if err := d.Decode(&dec); err != nil {
			return err
		}
		return trustedEncode(buf, dec)
	}
}

// trustedEncodeString 复刻 Python json.dumps(ensure_ascii=False) 的字符串转义：
// `"` → \"，`\` → \\，\n \r \t \b \f 用短转义，其余 <0x20 用 \u00XX（小写 hex），
// 非 ASCII 与 U+2028/29 原样。
func trustedEncodeString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(buf, `\u%04x`, r)
			} else {
				buf.WriteRune(r)
			}
		}
	}
	buf.WriteByte('"')
}

// fmtFormatFloat 用 Python json 的 float 表示（repr 最短往返形式）。
func fmtFormatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
