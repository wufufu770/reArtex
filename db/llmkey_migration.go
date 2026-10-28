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
