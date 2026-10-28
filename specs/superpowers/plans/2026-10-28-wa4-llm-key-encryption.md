# W-A4 LLM 密钥加密落库 · 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 `llm_profiles.api_key` 不再以明文存库——改用应用层 AES-256-GCM 加密落库，主密钥由文件供给（不进环境块），并把历史明文行迁移掉；拿到数据库口令的人读不到 LLM key。

**Architecture:** 新增 `config.MasterKey()` 从 `ARTEX_MASTER_KEY_FILE` 读主密钥（32 字节，hex/base64），`db` 包加密/解密工具（AES-256-GCM，密文 = base64(nonce‖ciphertext)），`llm_profiles` 新增 `api_key_cipher` 列：写路径加密后落 `api_key_cipher`、把 `api_key` 置空；读路径优先解 `api_key_cipher`，遇到历史明文行则照常返回并触发一次性迁移。**AAD 绑定 profile 名**，使不同行的密文不能互换（否则有库写权限的人可把 A 行的 key 挪到 B 行，配上自己可控的 base_url 就等于把 key 送出去）；因此改名必须重加密——该场景单列一步处理。

**Tech Stack:** Go 1.26 标准库 `crypto/aes` + `crypto/cipher`（GCM）、PostgreSQL 16

## Global Constraints

- 所有 git 命令必须带 `GIT_MASTER=1` 前缀。
- **禁止 `git reset --hard`**；**禁止用配置里的活库 `artex` 跑测试**。
- 注释、commit message、文档一律中文。
- **不引入新的第三方依赖**（标准库足够）。
- **主密钥绝不进环境块、绝不进数据库、绝不进版本库**；主密钥文件必须在 agent 工作区之外（`/app/data` 是 agent 的工作区根），读入后只留在内存。
- **向后兼容**：未配置主密钥时，平台行为与现在一致（明文可用），只是告警——不能因为这次加固让既有部署起不来。
- 每一处「闭合」都要同步更新账本 `reartex.md` 并归档。
- 推送前自检：不得出现本机路径、个人 MCP 名、凭据、主机指纹。

---

## File Structure

| 文件 | 职责 | 本计划中的动作 |
| --- | --- | --- |
| `config/masterkey.go` | 主密钥加载与校验（**新建**） | `MasterKey() ([]byte, error)` |
| `config/masterkey_test.go` | 主密钥加载回归（**新建**） | 缺文件/格式错/位置在工作区内 三种情况 |
| `db/crypto.go` | 对称加解密（**新建**） | `encryptSecret` / `decryptSecret` |
| `db/crypto_test.go` | 加解密回归（**新建**） | 往返、密文不含明文、换 AAD 解不开 |
| `db/config.go` | profile 的读写 | 写路径加密、读路径解密、池查询条件修正、改名重加密 |
| `db/schema.sql` | 表结构 | `llm_profiles` 增列 `api_key_cipher TEXT` |
| `db/llmkey_migration.go` | 历史明文行迁移（**新建**） | `MigrateLLMProfileKeys()` |
| `db/llmkey_migration_test.go` | 迁移回归（**新建**） | 明文行迁移后库里无明文 |
| `server/server.go`（或启动装配处） | 启动流程 | 调用一次迁移 |

---

### Task 1: 主密钥加载

**Files:**
- Create: `config/masterkey.go`
- Create: `config/masterkey_test.go`

**Interfaces:**
- Consumes: 环境变量 `ARTEX_MASTER_KEY_FILE`（路径）
- Produces: `config.MasterKey() ([]byte, error)` —— 返回 32 字节主密钥；**未配置时返回 `(nil, nil)`**（不是错误：未配置是合法的过渡态）

- [ ] **Step 1: 写失败测试**

新建 `config/masterkey_test.go`：

```go
package config

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMasterKeyNotConfigured(t *testing.T) {
	t.Setenv("ARTEX_MASTER_KEY_FILE", "")
	key, err := MasterKey()
	if err != nil {
		t.Fatalf("未配置主密钥不应报错: %v", err)
	}
	if key != nil {
		t.Fatal("未配置主密钥时应返回 nil")
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
	t.Setenv("ARTEX_MASTER_KEY_FILE", p)

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
	t.Setenv("ARTEX_MASTER_KEY_FILE", short)
	if _, err := MasterKey(); err == nil {
		t.Fatal("长度不对的主密钥文件应报错")
	}
}

// 主密钥文件落在 agent 工作区（数据目录）里 = agent 读得到 = 等于没加密。
func TestMasterKeyRejectsPathInsideDataDir(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "master.key")
	raw := make([]byte, 32)
	if err := os.WriteFile(p, []byte(hex.EncodeToString(raw)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARTEX_MASTER_KEY_FILE", p)
	orig := DataDir
	DataDir = dir
	t.Cleanup(func() { DataDir = orig })
	if _, err := MasterKey(); err == nil {
		t.Fatal("主密钥文件在工作区内应报错")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./config/ -run 'TestMasterKey' -v`
Expected: FAIL —— `MasterKey` / `DataDir` 尚不存在（编译错误）。

- [ ] **Step 3: 实现**

若 `config` 包尚无 `DataDir`（数据目录变量），在 `config/masterkey.go` 里定义：

```go
// DataDir 是数据目录（agent 工作区的根）。主密钥文件不得落在里面。
var DataDir = func() string { return filepath.Join(BaseDir(), "data") }()
```

新建 `config/masterkey.go`：

```go
package config

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MasterKeyFileEnv 是主密钥文件路径的环境变量名。
const MasterKeyFileEnv = "ARTEX_MASTER_KEY_FILE"

// masterKeyLen 是主密钥长度（AES-256）。
const masterKeyLen = 32

// MasterKey 读主密钥。返回 (nil, nil) 表示未配置 —— 未配置是合法的过渡态：
// 平台照旧工作（密钥明文落库），只是启动时告警。不能因为这次加固让既有
// 部署起不来。
//
// 位置约束：文件不得落在数据目录内 —— 那是 agent 工作区的根，agent 读得到
// 就等于没加密。
func MasterKey() ([]byte, error) {
	p := strings.TrimSpace(os.Getenv(MasterKeyFileEnv))
	if p == "" {
		return nil, nil
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("主密钥文件路径 %q 无效: %w", p, err)
	}
	if withinDataDir(abs) {
		return nil, fmt.Errorf("主密钥文件 %q 落在数据目录内：agent 读得到，等于没加密。请放到 %s 之外", abs, DataDir)
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("读取主密钥文件 %q 失败: %w", abs, err)
	}
	raw := strings.TrimSpace(string(b))
	var key []byte
	if len(raw) == masterKeyLen*2 {
		key, err = hex.DecodeString(raw)
	} else {
		key, err = base64.StdEncoding.DecodeString(raw)
	}
	if err != nil {
		return nil, fmt.Errorf("主密钥文件 %q 内容不是 hex 或 base64: %w", abs, err)
	}
	if len(key) != masterKeyLen {
		return nil, fmt.Errorf("主密钥长度应为 %d 字节，得到 %d", masterKeyLen, len(key))
	}
	return key, nil
}

// withinDataDir 判断路径是否落在数据目录内。
func withinDataDir(abs string) bool {
	dir, err := filepath.Abs(DataDir)
	if err != nil {
		return false
	}
	return abs == dir || strings.HasPrefix(abs, dir+string(filepath.Separator))
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./config/ -count=1 -v`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
GIT_MASTER=1 git add config/masterkey.go config/masterkey_test.go
GIT_MASTER=1 git commit -m "feat(config): 主密钥从文件加载，落在数据目录内一律拒绝"
```

---

### Task 2: 加解密工具

**Files:**
- Create: `db/crypto.go`
- Create: `db/crypto_test.go`

**Interfaces:**
- Consumes: 标准库 `crypto/aes`、`crypto/cipher`、`crypto/rand`
- Produces:
  - `func encryptSecret(plaintext string, key []byte, aad string) (string, error)`
  - `func decryptSecret(ciphertext string, key []byte, aad string) (string, error)`

- [ ] **Step 1: 写失败测试**

新建 `db/crypto_test.go`：

```go
package db

import (
	"bytes"
	"strings"
	"testing"
)

func testKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := testKey()
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
	key := testKey()
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
	key := testKey()
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
	key := testKey()
	enc, err := encryptSecret("value", key, "p")
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(enc)
	raw[len(raw)-1] = 'A'
	if _, err := decryptSecret(string(raw), key, "p"); err == nil {
		t.Fatal("被篡改的密文应解密失败")
	}
	_ = bytes.MinRead
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./db/ -run 'TestEncrypt|TestDecrypt' -v`
Expected: FAIL —— `encryptSecret` / `decryptSecret` 尚不存在（编译错误）。

- [ ] **Step 3: 实现**

新建 `db/crypto.go`：

```go
package db

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// 平台密钥的落库加密。
//
// 为什么在应用层加密而不是用数据库的加密能力：拿到数据库口令的人本来就能
// 读明文列；PG 的 TDE 之类要额外的服务端配置，reArtex 的部署形态不保证有。
// 应用层加密的边界很清楚：口令能读库，但没有主密钥就解不开。
//
// 密文格式：base64(nonce ‖ ciphertext ‖ tag)，nonce 每次随机。
// AAD 绑定行标识（profile 名）：防止有库写权限的人把 A 行的密文挪到 B 行，
// 再配上自己可控的 base_url，把 key 送到别处去。

// encryptSecret 用 AES-256-GCM 加密，返回 base64 密文。
func encryptSecret(plaintext string, key []byte, aad string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	if len(key) != 32 {
		return "", fmt.Errorf("主密钥长度应为 32 字节，得到 %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, nonce, []byte(plaintext), []byte(aad))
	return base64.StdEncoding.EncodeToString(append(nonce, sealed...)), nil
}

// decryptSecret 解开 base64 密文。
func decryptSecret(ciphertext string, key []byte, aad string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}
	if len(key) != 32 {
		return "", errors.New("主密钥长度应为 32 字节")
	}
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("密文不是合法 base64: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("密文长度不足")
	}
	nonce, sealed := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, sealed, []byte(aad))
	if err != nil {
		return "", fmt.Errorf("解密失败（密钥不对，或密文被改过）: %w", err)
	}
	return string(plain), nil
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./db/ -run 'TestEncrypt|TestDecrypt' -v`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
GIT_MASTER=1 git add db/crypto.go db/crypto_test.go
GIT_MASTER=1 git commit -m "feat(db): 平台密钥的应用层加解密工具（AES-256-GCM，AAD 绑定行）"
```

---

### Task 3: 表结构与读写路径改造

**Files:**
- Modify: `db/schema.sql`
- Modify: `db/config.go`

**Interfaces:**
- Consumes: `config.MasterKey()`、`encryptSecret` / `decryptSecret`
- Produces:
  - `llm_profiles.api_key_cipher TEXT`（新列）
  - `LLMProfile.APIKeyCipher string`（`json:"-"`，不序列化）
  - `func (d *DB) resolveProfileAPIKey(p *LLMProfile) error` —— 读路径解密

- [ ] **Step 1: 加列**

`db/schema.sql` 的 `llm_profiles` 定义里，在 `api_key TEXT,` 之后加一行：

```sql
    api_key_cipher TEXT,
```

（列的顺序不影响既有 INSERT——那些都按列名写 ✓。）

- [ ] **Step 2: 写失败测试**

新建 `db/llmkey_test.go`（`db` 包已有 TestMain 与派生测试库 ✓）：

```go
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
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Skipf("打开测试库失败: %v", err)
	}
	if err := conn.Ping(); err != nil {
		t.Skipf("测试库不可用: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return NewWithConn(conn)
}

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

（`NewWithConn` 若不存在，用 `db` 包里既有的「用现成 *sql.DB 构造 DB」的入口；
落地时先 `grep -n "func New" db/db.go` 确认后替换调用名。）

// 写进去的 key 不能以明文留在库里。
func TestSaveProfileEncryptsAPIKey(t *testing.T) {
	d := openTestDB(t)
	withMasterKey(t)

	id, err := d.SaveProfile(&LLMProfile{
		Name: "p1", Format: "openai", Model: "gpt-x", APIKey: "sk-plaintext-value",
	})
	if err != nil {
		t.Fatal(err)
	}
	var plain, cipher string
	if err := d.QueryRow(`SELECT COALESCE(api_key,''), COALESCE(api_key_cipher,'') FROM llm_profiles WHERE id=$1`, id).Scan(&plain, &cipher); err != nil {
		t.Fatal(err)
	}
	if plain != "" {
		t.Fatalf("api_key 列不应留明文，得到 %q", plain)
	}
	if cipher == "" {
		t.Fatal("api_key_cipher 列为空")
	}
	if cipher == "sk-plaintext-value" {
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
```

- [ ] **Step 3: 运行测试确认失败**

Run: `go test ./db/ -run TestSaveProfileEncryptsAPIKey -v`
Expected: FAIL —— 库里 `api_key` 仍是明文。

- [ ] **Step 4: 改写路径**

`db/config.go`：

**(a) 读写列加上 `api_key_cipher`（加在末尾，避免打乱既有列序）**

```go
const profileCols = `id,name,format,COALESCE(base_url,''),COALESCE(proxy,''),model,COALESCE(api_key_hint,''),rate_per_second,rate_per_minute,context_window_k,COALESCE(reasoning_effort,''),is_default,priority,pool_exclude,COALESCE(thinking_type,''),COALESCE(streaming,true),COALESCE(max_tokens,0),COALESCE(max_tokens_field,''),COALESCE(session_header_key,''),` + profileRetryCols + `,COALESCE(api_key_cipher,'')`
const profileColsKey = `id,name,format,COALESCE(base_url,''),COALESCE(proxy,''),model,COALESCE(api_key,''),rate_per_second,rate_per_minute,context_window_k,COALESCE(reasoning_effort,''),is_default,priority,pool_exclude,COALESCE(thinking_type,''),COALESCE(streaming,true),COALESCE(max_tokens,0),COALESCE(max_tokens_field,''),COALESCE(session_header_key,''),` + profileRetryCols + `,COALESCE(api_key_cipher,'')`
```

**(b) `LLMProfile` 增字段**

```go
	APIKeyCipher string  `json:"-"` // 加密落库的 key（不序列化）
```

**(c) `scanProfile` 扫上第末列**

```go
func scanProfile(sc interface{ Scan(...any) error }, into *string, p *LLMProfile) error {
	return sc.Scan(&p.ID, &p.Name, &p.Format, &p.BaseURL, &p.Proxy, &p.Model, into,
		&p.RatePerSecond, &p.RatePerMinute, &p.ContextWindowK, &p.ReasoningEffort, &p.IsDefault, &p.Priority, &p.PoolExclude, &p.ThinkingType, &p.Streaming,
		&p.MaxTokens, &p.MaxTokensField, &p.SessionHeaderKey,
		&p.Retry.Connect.Attempts, &p.Retry.Connect.IntervalMS,
		&p.Retry.Empty.Attempts, &p.Retry.Empty.IntervalMS,
		&p.Retry.Stream.Attempts, &p.Retry.Stream.IntervalMS,
		&p.APIKeyCipher)
}
```

**(d) 读路径解密**

新增：

```go
// resolveProfileAPIKey 把密文解成明文 key。
//
// 三种情形：
//   - 没有密文：历史明文行（api_key 列仍有值），照旧返回，不报错。
//   - 有密文且配了主密钥：解出明文。
//   - 有密文但没配主密钥：报错（不能静默当成「无 key」，否则会表现为
//     「配了却调不通」这种难查的现象）。
func resolveProfileAPIKey(p *LLMProfile) error {
	if p.APIKeyCipher == "" {
		return nil // 历史明文行
	}
	key, err := config.MasterKey()
	if err != nil {
		return err
	}
	if len(key) == 0 {
		return fmt.Errorf("profile %q 的 api_key 已加密，但未配置 %s：无法解密", p.Name, config.MasterKeyFileEnv)
	}
	plain, err := decryptSecret(p.APIKeyCipher, key, p.Name)
	if err != nil {
		return fmt.Errorf("profile %q 的 api_key 解密失败: %w", p.Name, err)
	}
	p.APIKey = plain
	return nil
}
```

在**带 key 的**读路径（`ActiveProfile` / `ProfileByID` / `PoolProfiles`）扫完行之后调用 `resolveProfileAPIKey(p)`，并在出错时返回错误。

**(e) 写路径加密**

`SaveProfile` 的 INSERT 分支：把 `p.APIKey` 换成密文后落 `api_key_cipher`，`api_key` 落空串。

```go
	// 加密落库：密文进 api_key_cipher，明文列留空。
	cipherText := ""
	if p.APIKey != "" {
		key, err := config.MasterKey()
		if err != nil {
			return 0, err
		}
		if len(key) > 0 {
			cipherText, err = encryptSecret(p.APIKey, key, p.Name)
			if err != nil {
				return 0, err
			}
		}
	}
```

- 写 `api_key_cipher` 用 `NULLIF($25,'')` 形态并把列序整体后移一位（`$25` → `$26`）；INSERT 语句末尾追加 `api_key_cipher`。
- UPDATE 分支（有 key）同理：把 `api_key=$6` 换成 `api_key=NULLIF($6,''), api_key_cipher=NULLIF($7,'')`，并把 `api_key_hint` 从 `$7` 顺延为 `$8`，其余参数整体后移。
- UPDATE 分支（无 key，保留原值）：**追加改名重加密**——若行名变了，AAD 变了，旧密文解不开，故须取出旧名解出明文、用新名重加密：

```go
	if p.APIKey == "" {
		// 保留原 key。但 AAD 绑的是行名：改名后旧密文解不开，
		// 故取旧名解出明文、用新名重加密。
		if err := d.reencryptProfileKey(p.ID, p.Name); err != nil {
			return p.ID, err
		}
		...（其余 UPDATE 参数不变）
	}
```

配套：

```go
// reencryptProfileKey 在行名变化时用新名重加密该行的 key。
func (d *DB) reencryptProfileKey(id int64, newName string) error {
	var oldName, cipherText string
	err := d.QueryRow(`SELECT name, COALESCE(api_key_cipher,'') FROM llm_profiles WHERE id=$1`, id).Scan(&oldName, &cipherText)
	if err != nil {
		return err
	}
	if cipherText == "" || oldName == newName {
		return nil
	}
	key, err := config.MasterKey()
	if err != nil {
		return err
	}
	if len(key) == 0 {
		return nil // 没配主密钥：明文行，无需重加密
	}
	plain, err := decryptSecret(cipherText, key, oldName)
	if err != nil {
		return fmt.Errorf("profile %q 改名失败：旧名下的密文解不开: %w", oldName, err)
	}
	newCipher, err := encryptSecret(plain, key, newName)
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE llm_profiles SET api_key_cipher=$2 WHERE id=$1`, id, newCipher)
	return err
}
```

**(f) 池查询条件修正**

`PoolProfiles()` 的 `WHERE COALESCE(api_key,'') <> ''` 必须改成同时看密文列，否则加密后池子会空：

```sql
WHERE (COALESCE(api_key,'') <> '' OR COALESCE(api_key_cipher,'') <> '')
  AND (is_default OR NOT pool_exclude)
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./db/ -run 'TestSaveProfile|TestPoolProfiles' -count=1 -v`
Expected: PASS。

- [ ] **Step 6: 全包测试 + 提交**

```bash
go test ./db/ ./config/ -count=1
GIT_MASTER=1 git add db/schema.sql db/config.go db/llmkey_test.go
GIT_MASTER=1 git commit -m "feat(db): LLM profile 的 api_key 改为加密落库"
```

---

### Task 4: 历史明文行迁移

**Files:**
- Create: `db/llmkey_migration.go`
- Create: `db/llmkey_migration_test.go`
- Modify: 启动装配处（`server/` 里调用 `db` 初始化的位置）

**Interfaces:**
- Consumes: `encryptSecret`、`config.MasterKey()`
- Produces: `func (d *DB) MigrateLLMProfileKeys() (int, error)` —— 返回迁移行数；未配置主密钥时返回 (0, nil)

- [ ] **Step 1: 写失败测试**

新建 `db/llmkey_migration_test.go`：

```go
package db

import "testing"

// 历史明文行必须在迁移后不再以明文留在库里。
func TestMigrateLLMProfileKeys(t *testing.T) {
	d := openTestDB(t)
	withMasterKey(t)

	// 造一行历史明文
	if _, err := d.Exec(`INSERT INTO llm_profiles(name,format,model,api_key) VALUES ('old','openai','m','sk-legacy-plain')`); err != nil {
		t.Fatal(err)
	}
	n, err := d.MigrateLLMProfileKeys()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应迁移 1 行，得到 %d", n)
	}
	var plain, cipher string
	if err := d.QueryRow(`SELECT COALESCE(api_key,''), COALESCE(api_key_cipher,'') FROM llm_profiles WHERE name='old'`).Scan(&plain, &cipher); err != nil {
		t.Fatal(err)
	}
	if plain != "" {
		t.Fatalf("迁移后仍有明文: %q", plain)
	}
	if cipher == "" {
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
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./db/ -run 'TestMigrateLLMProfileKeys' -v`
Expected: FAIL —— `MigrateLLMProfileKeys` 尚不存在（编译错误）。

- [ ] **Step 3: 实现迁移**

新建 `db/llmkey_migration.go`：

```go
package db

import (
	"fmt"

	"github.com/Autumn-27/artex/config"
)

// MigrateLLMProfileKeys 把历史明文 key 行迁移成密文行。
//
// 返回迁移行数。未配置主密钥时不做任何事、也不报错 —— 既有部署照旧工作
// （明文），等管理员配好主密钥并重启后再迁。
func (d *DB) MigrateLLMProfileKeys() (int, error) {
	key, err := config.MasterKey()
	if err != nil {
		return 0, err
	}
	if len(key) == 0 {
		return 0, nil
	}
	rows, err := d.Query(`SELECT id, name, COALESCE(api_key,'') FROM llm_profiles
		WHERE COALESCE(api_key,'') <> '' AND COALESCE(api_key_cipher,'') = ''`)
	if err != nil {
		return 0, err
	}
	type row struct {
		id   int64
		name string
		key  string
	}
	var todo []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.name, &r.key); err != nil {
			rows.Close()
			return 0, err
		}
		todo = append(todo, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, r := range todo {
		cipherText, err := encryptSecret(r.key, key, r.name)
		if err != nil {
			return 0, fmt.Errorf("profile %q 迁移失败: %w", r.name, err)
		}
		if _, err := d.Exec(`UPDATE llm_profiles SET api_key_cipher=$2, api_key='' WHERE id=$1`, r.id, cipherText); err != nil {
			return 0, err
		}
	}
	return len(todo), nil
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./db/ -run 'TestMigrateLLMProfileKeys' -v`
Expected: PASS。

- [ ] **Step 5: 接进启动流程**

在 `server` 初始化数据库之后、开始服务之前调用一次：

```go
	// 历史明文 key 行迁移成密文（未配主密钥时是 no-op）。
	if n, err := d.MigrateLLMProfileKeys(); err != nil {
		log.Printf("[security] LLM key 迁移失败: %v", err)
	} else if n > 0 {
		log.Printf("[security] 已把 %d 行 LLM key 迁移为密文", n)
	}
```

（放在 `server/` 里数据库连接建立之后的位置；具体函数名以该文件实际为准。）

- [ ] **Step 6: 全包测试 + 提交**

```bash
go test ./db/ ./server/ ./config/ -count=1
GIT_MASTER=1 git add db/llmkey_migration.go db/llmkey_migration_test.go server/
GIT_MASTER=1 git commit -m "feat(db): 历史明文 LLM key 迁移为密文，启动时执行一次"
```

---

### Task 5: 运维文档与账本

**Files:**
- Modify: `README.md`（运维段：主密钥怎么给、怎么轮换、丢了会怎样）
- Modify: 账本本体 + 归档副本

- [ ] **Step 1: README 补主密钥说明**

在安装/运维章节加一小节：

```markdown
### 加密落库的 LLM key（可选但推荐）

`llm_profiles.api_key` 可以用主密钥加密后落库。启用方式：

1. 生成主密钥并放到**数据目录之外**（`/app/data` 是 agent 工作区，放里面等于没加密）：

   ```bash
   mkdir -p /run/artex
   openssl rand -hex 32 > /run/artex/master.key
   chmod 600 /run/artex/master.key
   ```

2. 在 compose 的 `artex.environment` 里加一行（**只放路径，不放密钥本身**）：

   ```yaml
   ARTEX_MASTER_KEY_FILE: /run/artex/master.key
   ```

3. 重启后，历史明文行会在启动时自动迁移成密文。

**丢了主密钥 = 所有已加密的 LLM key 都解不开**（只能重新填 key）。请备份主密钥文件。
未配置该变量时平台行为与之前完全一致（key 明文落库），只是没有这层保护。
```

- [ ] **Step 2: 账本口径同步**

把「`llm_profiles.api_key` 仍以明文存于数据库」那条改为：

```
`llm_profiles.api_key` 原本明文存库（拿到 DB 口令即可读出全部 LLM key）。
**已闭合（2026-10-28）**：改为 AES-256-GCM 加密落库（api_key_cipher 列，明文列置空），
主密钥由 ARTEX_MASTER_KEY_FILE 供给（只存路径、密钥在内存、不进环境块/不进库），
历史明文行在启动时自动迁移；AAD 绑定行名，改名时重加密，防止有库写权限的人把
A 行的密文挪到 B 行再配上自己可控的 base_url 把 key 送出去。
**边界**：拿到 DB 口令**且**拿到主密钥文件的人仍能解出 key（主密钥文件权限是
这层保护的边界）；未配置主密钥时行为与之前一致。
```

- [ ] **Step 3: 归档账本**

复制到归档路径并 `cmp` 校验。

- [ ] **Step 4: 提交**

```bash
GIT_MASTER=1 git add README.md
GIT_MASTER=1 git commit -m "docs: 主密钥的启用方式、备份要求与失效后果"
```

---

## 验收（整个计划完成后）

```bash
go test ./db/ ./config/ ./server/ -count=1
go test ./... -timeout 30m -count=1
# 端到端：设了主密钥后，库里不应再有明文 key
#   docker exec artex-artex-1 psql ... -c "select count(*) from llm_profiles where api_key <> ''"
# 期望 0
```

**本计划不覆盖**：per-task 隔离、签名与 provenance、多架构冒烟、生态四项。
