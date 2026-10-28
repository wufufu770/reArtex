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
// 平台照旧工作（LLM key 明文落库），只是告警。不能因为这次加固让既有部署起不来。
//
// 位置要求（不在这里校验）：主密钥文件必须在数据目录之外 —— 那是 agent 的
// 工作区根，agent 读得到就等于没加密。知道数据目录的是 server，故由它在
// 启动时校验（见 MasterKeyPath 的说明）。
func MasterKey() ([]byte, error) {
	p := MasterKeyPath()
	if p == "" {
		return nil, nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("读取主密钥文件 %q 失败: %w", p, err)
	}
	raw := strings.TrimSpace(string(b))
	var key []byte
	if len(raw) == masterKeyLen*2 {
		key, err = hex.DecodeString(raw)
	} else {
		key, err = base64.StdEncoding.DecodeString(raw)
	}
	if err != nil {
		return nil, fmt.Errorf("主密钥文件 %q 内容不是 hex 或 base64: %w", p, err)
	}
	if len(key) != masterKeyLen {
		return nil, fmt.Errorf("主密钥长度应为 %d 字节，得到 %d", masterKeyLen, len(key))
	}
	return key, nil
}

// MasterKeyPath 返回配置的主密钥文件路径（未配置时为空串）。
//
// 供 server 在启动时做位置校验：主密钥不得落在数据目录内（agent 的工作区根）。
func MasterKeyPath() string {
	return strings.TrimSpace(os.Getenv(MasterKeyFileEnv))
}

// MasterKeyOutsideDir 判断主密钥文件是否落在指定目录之外。
//
// 数据目录是 agent 的工作区根（会话、任务、证据都在里面），主密钥放在那里
// 等于把钥匙挂在门把手上 —— 拿库口令的人顺手就能读走。
func MasterKeyOutsideDir(dir string) error {
	p := MasterKeyPath()
	if p == "" {
		return nil
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return fmt.Errorf("主密钥文件路径 %q 无效: %w", p, err)
	}
	base, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("数据目录路径 %q 无效: %w", dir, err)
	}
	if abs == base || strings.HasPrefix(abs, base+string(filepath.Separator)) {
		return fmt.Errorf("主密钥文件 %q 落在数据目录 %s 内：agent 读得到，等于没加密。请放到该目录之外", abs, base)
	}
	return nil
}
