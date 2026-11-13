package db

// 小程序 token 池访问层（0.12.0，小程序渗透施工 §四-B3）。
//
// 纪律（施工方案 §五）：
//   - 落库即加密：config.MasterKey() 配置时走 encryptSecret（AAD = app_id+":"+label，
//     把凭据绑死在「哪个应用的哪个角色」上——换库挪用会解密失败）；未配主密钥时
//     明文过渡列（与 llm_profiles 的 api_key/api_key_cipher 同款取舍，如实声明）。
//   - transcript 面只见标签与指纹；明文只在显式 reveal 时返回（构造 Burp 重放的
//     replacement 时刻）。
//   - DDL 独立幂等文件 db/schema_miniprogram.sql（db/schema.sql 是 RESERVED），
//     EnsureMiniprogramSchema 启动期接线（照 EnsureBizSchema 同款模式）。

import (
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/config"
)

//go:embed schema_miniprogram.sql
var miniprogramSchemaSQL string

// miniprogramSchemaReady 记录某个连接已经应用过小程序 DDL（照 loopTrackSchemaReady）。
var miniprogramSchemaReady sync.Map

// EnsureMiniprogramSchema 应用小程序渗透的幂等 DDL。
func (d *DB) EnsureMiniprogramSchema() error {
	if _, ok := miniprogramSchemaReady.Load(d.DB); ok {
		return nil
	}
	if _, err := d.Exec(miniprogramSchemaSQL); err != nil {
		return fmt.Errorf("应用小程序渗透 schema 失败: %w", err)
	}
	miniprogramSchemaReady.Store(d.DB, struct{}{})
	return nil
}

// ensureMiniprogramSchema 是访问层的幂等入口。
func (d *DB) ensureMiniprogramSchema() error { return d.EnsureMiniprogramSchema() }

// MiniprogramToken 是 miniprogram_tokens 的一行（列表面不含明文/密文）。
type MiniprogramToken struct {
	ID          int64     `json:"id"`
	TaskID      int64     `json:"task_id"`
	AppID       string    `json:"app_id"`
	Label       string    `json:"label"`
	Fingerprint string    `json:"fingerprint"`
	Scope       string    `json:"scope"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// TokenFingerprint 是凭据指纹口径：sha256 前 16 位 hex——可比对、不可还原。
func TokenFingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum[:8])
}

// UpsertMiniprogramToken 保存/轮换一条 token（唯一键 task+app+label）。
// 加密跟随主密钥配置：有密钥走 cipher（AAD 绑 app+label），无密钥明文过渡列。
// 返回指纹。
func (d *DB) UpsertMiniprogramToken(taskID int64, appID, label, value, scope string) (string, error) {
	if err := d.ensureMiniprogramSchema(); err != nil {
		return "", err
	}
	appID = strings.TrimSpace(appID)
	label = strings.TrimSpace(label)
	if taskID <= 0 {
		return "", fmt.Errorf("task_id 必填：token 池按任务作用域绑定")
	}
	if appID == "" || label == "" || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("app_id / label / token 值均必填")
	}
	if scope != "task" && scope != "company" {
		scope = "task"
	}
	fp := TokenFingerprint(value)
	plain, cipher := value, ""
	key, err := config.MasterKey()
	if err != nil {
		return "", fmt.Errorf("读主密钥: %w", err)
	}
	if len(key) > 0 {
		c, err := encryptSecret(value, key, appID+":"+label)
		if err != nil {
			return "", fmt.Errorf("加密 token: %w", err)
		}
		cipher = c
		plain = ""
	}
	if _, err := d.Exec(`
INSERT INTO miniprogram_tokens(task_id, app_id, label, token_plain, token_cipher, fingerprint, scope)
VALUES ($1,$2,$3,$4,$5,$6,$7)
ON CONFLICT (task_id, app_id, label) DO UPDATE SET
  token_plain = EXCLUDED.token_plain,
  token_cipher = EXCLUDED.token_cipher,
  fingerprint = EXCLUDED.fingerprint,
  scope = EXCLUDED.scope,
  updated_at = now()`,
		taskID, appID, label, plain, cipher, fp, scope); err != nil {
		return "", fmt.Errorf("upsert miniprogram token: %w", err)
	}
	return fp, nil
}

// ListMiniprogramTokens 列 token（清单面：标签 + 指纹，无凭据内容）。
func (d *DB) ListMiniprogramTokens(taskID int64, appID string) ([]*MiniprogramToken, error) {
	if err := d.ensureMiniprogramSchema(); err != nil {
		return nil, err
	}
	q := `SELECT id, task_id, app_id, label, fingerprint, scope, created_at, updated_at
FROM miniprogram_tokens WHERE task_id=$1`
	args := []any{taskID}
	if appID != "" {
		q += ` AND app_id=$2`
		args = append(args, appID)
	}
	q += ` ORDER BY label`
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*MiniprogramToken{}
	for rows.Next() {
		t := &MiniprogramToken{}
		if err := rows.Scan(&t.ID, &t.TaskID, &t.AppID, &t.Label, &t.Fingerprint, &t.Scope,
			&t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// RevealMiniprogramToken 按 id 取凭据明文（使用时刻专用——构造 Burp 重放的
// replacement）。解密失败（主密钥变更/行被挪库）如实报错，不静默降级。
func (d *DB) RevealMiniprogramToken(id int64) (appID, label, value string, err error) {
	if err := d.ensureMiniprogramSchema(); err != nil {
		return "", "", "", err
	}
	var plain, cipher string
	err = d.QueryRow(`SELECT app_id, label, token_plain, token_cipher FROM miniprogram_tokens WHERE id=$1`, id).
		Scan(&appID, &label, &plain, &cipher)
	if err == sql.ErrNoRows {
		return "", "", "", fmt.Errorf("token #%d 不存在", id)
	}
	if err != nil {
		return "", "", "", err
	}
	if cipher != "" {
		key, err := config.MasterKey()
		if err != nil {
			return "", "", "", err
		}
		if len(key) == 0 {
			return "", "", "", fmt.Errorf("token #%d 是密文落库但当前未配置主密钥——解密需要同一把主密钥", id)
		}
		value, err = decryptSecret(cipher, key, appID+":"+label)
		if err != nil {
			return "", "", "", fmt.Errorf("token #%d 解密失败（主密钥不匹配或行被挪库）: %w", id, err)
		}
		return appID, label, value, nil
	}
	if plain == "" {
		return "", "", "", fmt.Errorf("token #%d 内容为空", id)
	}
	return appID, label, plain, nil
}

// DeleteMiniprogramToken 删除一条 token。
func (d *DB) DeleteMiniprogramToken(id int64) error {
	if err := d.ensureMiniprogramSchema(); err != nil {
		return err
	}
	_, err := d.Exec(`DELETE FROM miniprogram_tokens WHERE id=$1`, id)
	return err
}

// MiniprogramTokenMeta 按 id 取清单面元信息（标签 + 指纹，无凭据内容）。
// 不存在返回 (nil, nil)。
func (d *DB) MiniprogramTokenMeta(id int64) (*MiniprogramToken, error) {
	if err := d.ensureMiniprogramSchema(); err != nil {
		return nil, err
	}
	t := &MiniprogramToken{}
	err := d.QueryRow(`SELECT id, task_id, app_id, label, fingerprint, scope, created_at, updated_at
FROM miniprogram_tokens WHERE id=$1`, id).
		Scan(&t.ID, &t.TaskID, &t.AppID, &t.Label, &t.Fingerprint, &t.Scope, &t.CreatedAt, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}
