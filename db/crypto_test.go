package db

import (
	"strings"
	"testing"
)

func testMasterKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := testMasterKey()
	enc, err := encryptSecret("sk-secret-value", key, "profile-a")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(enc, "sk-secret-value") {
		t.Fatal("密文里出现了明文")
	}
	got, err := decryptSecret(enc, key, "profile-a")
	if err != nil {
		t.Fatal(err)
	}
	if got != "sk-secret-value" {
		t.Fatalf("解密结果不对: %q", got)
	}
}

// AAD 绑定：换一个 AAD 解不开（防止把 A 行的密文挪到 B 行用）。
func TestDecryptWithWrongAADFails(t *testing.T) {
	key := testMasterKey()
	enc, err := encryptSecret("sk-secret-value", key, "profile-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decryptSecret(enc, key, "profile-b"); err == nil {
		t.Fatal("换了 AAD 仍能解开：AAD 没起作用")
	}
}

// 同一明文两次加密得到不同密文（随机 nonce），避免密文相同被看出「同一把 key」。
func TestEncryptIsRandomized(t *testing.T) {
	key := testMasterKey()
	a, err := encryptSecret("same", key, "p")
	if err != nil {
		t.Fatal(err)
	}
	b, err := encryptSecret("same", key, "p")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("两次加密结果相同：nonce 不随机")
	}
}

func TestDecryptRejectsTamperedCiphertext(t *testing.T) {
	key := testMasterKey()
	enc, err := encryptSecret("value", key, "p")
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(enc)
	raw[len(raw)-1] = 'A'
	if _, err := decryptSecret(string(raw), key, "p"); err == nil {
		t.Fatal("被篡改的密文应解密失败")
	}
}

func TestDecryptRejectsWrongKey(t *testing.T) {
	enc, err := encryptSecret("value", testMasterKey(), "p")
	if err != nil {
		t.Fatal(err)
	}
	other := testMasterKey()
	other[0] ^= 0xff
	if _, err := decryptSecret(enc, other, "p"); err == nil {
		t.Fatal("换一把密钥应解不开")
	}
}
