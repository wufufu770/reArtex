package db

import (
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// openTestDB 打开本包的派生测试库；连不上就跳过需要库的用例。
// db 包已有 TestMain，派生库连接串已由它设进 ARTEX_PG_DSN。
func openTestDB(t *testing.T) *DB {
	t.Helper()
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("无测试库（ARTEX_PG_DSN 未设置）")
	}
	d, err := Open(dsn)
	if err != nil {
		t.Skipf("打开测试库失败: %v", err)
	}
	return d
}

// withMasterKey 给用例装一把临时主密钥。
func withMasterKey(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "master.key")
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 7)
	}
	if err := os.WriteFile(p, []byte(hex.EncodeToString(raw)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARTEX_MASTER_KEY_FILE", p)
}

// 写进去的 key 不能以明文留在库里。
func TestSaveProfileEncryptsAPIKey(t *testing.T) {
	d := openTestDB(t)
	withMasterKey(t)

	id, err := d.SaveProfile(&LLMProfile{
		Name: "enc-p1", Format: "openai", Model: "gpt-x", APIKey: "sk-plaintext-value",
	})
	if err != nil {
		t.Fatal(err)
	}
	var plain, cipherText string
	if err := d.QueryRow(`SELECT COALESCE(api_key,''), COALESCE(api_key_cipher,'') FROM llm_profiles WHERE id=$1`, id).
		Scan(&plain, &cipherText); err != nil {
		t.Fatal(err)
	}
	if plain != "" {
		t.Fatalf("api_key 列不应留明文，得到 %q", plain)
	}
	if cipherText == "" {
		t.Fatal("api_key_cipher 列为空")
	}
	if cipherText == "sk-plaintext-value" {
		t.Fatal("密文等于明文")
	}

	// 读回来必须能用
	got, err := d.ProfileByID(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.APIKey != "sk-plaintext-value" {
		t.Fatalf("读回的 key 不对: %q", got.APIKey)
	}
}

// 改名后密文仍要解得开（AAD 绑行名，改名必须重加密）。
func TestSaveProfileRenameReencryptsKey(t *testing.T) {
	d := openTestDB(t)
	withMasterKey(t)

	id, err := d.SaveProfile(&LLMProfile{
		Name: "rename-p1", Format: "openai", Model: "gpt-x", APIKey: "sk-rename-value",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 只改名字、不带 key（保留原 key）
	if _, err := d.SaveProfile(&LLMProfile{
		ID: id, Name: "rename-p2", Format: "openai", Model: "gpt-x",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := d.ProfileByID(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.APIKey != "sk-rename-value" {
		t.Fatalf("改名后读回的 key 不对: %q", got.APIKey)
	}
}

// 历史明文行必须在迁移后不再以明文留在库里。
func TestMigrateLLMProfileKeys(t *testing.T) {
	d := openTestDB(t)
	withMasterKey(t)

	if _, err := d.Exec(`INSERT INTO llm_profiles(name,format,model,api_key) VALUES ('legacy-p1','openai','m','sk-legacy-plain')`); err != nil {
		t.Fatal(err)
	}
	n, err := d.MigrateLLMProfileKeys()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应迁移 1 行，得到 %d", n)
	}
	var plain, cipherText string
	if err := d.QueryRow(`SELECT COALESCE(api_key,''), COALESCE(api_key_cipher,'') FROM llm_profiles WHERE name='legacy-p1'`).
		Scan(&plain, &cipherText); err != nil {
		t.Fatal(err)
	}
	if plain != "" {
		t.Fatalf("迁移后仍有明文: %q", plain)
	}
	if cipherText == "" {
		t.Fatal("迁移后没有密文")
	}
}

// 没有主密钥时不迁移、不报错：既有部署不能因为这次加固起不来。
func TestMigrateLLMProfileKeysWithoutKey(t *testing.T) {
	d := openTestDB(t)
	t.Setenv("ARTEX_MASTER_KEY_FILE", "")
	n, err := d.MigrateLLMProfileKeys()
	if err != nil {
		t.Fatalf("未配主密钥不应报错: %v", err)
	}
	if n != 0 {
		t.Fatalf("未配主密钥不应迁移，得到 %d", n)
	}
}

// 没配主密钥时，行为与加固前一致：key 仍明文落库，且能读回。
func TestSaveProfileWithoutMasterKeyKeepsPlaintext(t *testing.T) {
	d := openTestDB(t)
	t.Setenv("ARTEX_MASTER_KEY_FILE", "")

	id, err := d.SaveProfile(&LLMProfile{
		Name: "plain-p1", Format: "openai", Model: "gpt-x", APIKey: "sk-plain-ok",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.ProfileByID(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.APIKey != "sk-plain-ok" {
		t.Fatalf("读回的 key 不对: %q", got.APIKey)
	}
}

var _ = sql.ErrNoRows
