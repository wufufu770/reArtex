package server

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

// trusted_record 跨语言对齐测试（0.15.32）：向量由 Python 侧
// veto.trusted_record 生成（veto-eval/testdata/），Go 实现必须逐字节复现
// canonical / digest / signature / verify——任何一个字节差此测试即红。
func TestTrustedRecordCrossLanguageVectors(t *testing.T) {
	raw, err := os.ReadFile("../veto-eval/testdata/trusted_record_vectors.json")
	if err != nil {
		t.Fatalf("读向量文件失败: %v", err)
	}
	var doc struct {
		Vectors []struct {
			Name         string          `json:"name"`
			KeyHex       string          `json:"key_hex"`
			CanonicalHex string          `json:"canonical_hex"`
			DigestHex    string          `json:"digest_hex"`
			Signature    string          `json:"signature"`
			VerifyTrue   bool            `json:"verify_true"`
			Value        json.RawMessage `json:"value"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Vectors) == 0 {
		t.Fatal("向量文件为空")
	}
	for _, v := range doc.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			canonBytes, err := hex.DecodeString(v.CanonicalHex)
			if err != nil {
				t.Fatal(err)
			}
			var value map[string]any
			dec := json.NewDecoder(bytes.NewReader(canonBytes))
			dec.UseNumber()
			if err := dec.Decode(&value); err != nil {
				t.Fatalf("canonical 反解失败: %v", err)
			}
			// ① canonical 逐字节一致（重编码 == Python 原字节）。
			re, err := TrustedCanonical(value)
			if err != nil {
				t.Fatal(err)
			}
			if string(re) != string(canonBytes) {
				t.Fatalf("canonical 不齐:\n  got  %s\n  want %s", re, canonBytes)
			}
			// ② digest 一致。
			d, err := TrustedDigest(value)
			if err != nil || d != v.DigestHex {
				t.Fatalf("digest 不齐: got %s want %s (err=%v)", d, v.DigestHex, err)
			}
			// ③ 签名一致 + 验签通过。
			key, _ := hex.DecodeString(v.KeyHex)
			sig, err := TrustedSign(value, key)
			if err != nil || sig != v.Signature {
				t.Fatalf("签名不齐: got %s want %s (err=%v)", sig, v.Signature, err)
			}
			signed := value
			signed["signature"] = sig
			if !TrustedVerify(signed, key) {
				t.Fatal("验签应通过")
			}
			// ④ 篡改检测：改任意字段验签即失败。
			tampered := map[string]any{"payload": map[string]any{"n": json.Number("43")}, "signature": sig}
			if v.Name == "idempotent_sign_strip" {
				tampered = map[string]any{"payload": map[string]any{"n": json.Number("43")}, "signature": sig}
			}
			if TrustedVerify(tampered, key) {
				t.Fatal("篡改后验签必须失败")
			}
		})
	}
}
