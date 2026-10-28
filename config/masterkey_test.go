package config

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestMasterKeyNotConfigured(t *testing.T) {
	t.Setenv(MasterKeyFileEnv, "")
	key, err := MasterKey()
	if err != nil {
		t.Fatalf("未配置主密钥不应报错: %v", err)
	}
	if key != nil {
		t.Fatal("未配置主密钥时应返回 nil")
	}
	if err := MasterKeyOutsideDir(t.TempDir()); err != nil {
		t.Fatalf("未配置主密钥时位置校验不应报错: %v", err)
	}
}

func TestMasterKeyFromFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "master.key")
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i)
	}
	if err := os.WriteFile(p, []byte(hex.EncodeToString(raw)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(MasterKeyFileEnv, p)

	key, err := MasterKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != 32 {
		t.Fatalf("主密钥应为 32 字节，得到 %d", len(key))
	}
	if hex.EncodeToString(key) != hex.EncodeToString(raw) {
		t.Fatal("主密钥内容不对")
	}
}

func TestMasterKeyRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	short := filepath.Join(dir, "short.key")
	if err := os.WriteFile(short, []byte("abcd"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(MasterKeyFileEnv, short)
	if _, err := MasterKey(); err == nil {
		t.Fatal("长度不对的主密钥文件应报错")
	}

	notB64 := filepath.Join(dir, "notb64.key")
	if err := os.WriteFile(notB64, []byte("这不是 base64 也不是 hex!!"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(MasterKeyFileEnv, notB64)
	if _, err := MasterKey(); err == nil {
		t.Fatal("内容非法的密钥文件应报错")
	}
}

// 主密钥落在数据目录里 = agent 读得到 = 等于没加密。数据目录是 agent 的
// 工作区根，位置校验必须挡住。
func TestMasterKeyMustBeOutsideDataDir(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "master.key")
	raw := make([]byte, 32)
	if err := os.WriteFile(p, []byte(hex.EncodeToString(raw)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(MasterKeyFileEnv, p)

	if err := MasterKeyOutsideDir(dir); err == nil {
		t.Fatal("主密钥文件在数据目录内应报错")
	}
	// 同级子目录同样不行（在数据目录内）
	if err := MasterKeyOutsideDir(filepath.Dir(dir)); err == nil {
		t.Fatal("主密钥文件在数据目录的子目录内应报错")
	}
	// 目录之外则应通过
	if err := MasterKeyOutsideDir(t.TempDir()); err != nil {
		t.Fatalf("数据目录之外的主密钥应通过位置校验: %v", err)
	}
}
