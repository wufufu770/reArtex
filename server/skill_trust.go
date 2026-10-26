package server

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"github.com/Autumn-27/norma/skill"

	"github.com/Autumn-27/artex/db"
)

// Skill 信任分级：决定注入对话的 skill 正文是否按「权威指导」呈现。
//
// 为什么用「谁写的」而不是内容签名：签名只有在 agent 无法写该文件时才有意义，而
// update_skill_file 的存在意义恰恰是让 agent 能写。于是任何由 agent 影响过内容的
// 签名都可被重算，签名给不了保证。改为记录写入来源：agent 写的降权注入，operator
// 写的照常注入。

const (
	skillTierTrusted   = "trusted"
	skillTierUntrusted = "untrusted"

	// skillTrustKeyPrefix 是 settings 表里的键前缀，键名带 skill 目录名。
	skillTrustKeyPrefix = "skill_trust."
)

// db 返回管理后台数据源，无连接时为 nil。
//
// 不能直接解引用 s.m.pg：纯文件系统工具（如 create_skill）的单元测试构造的是
// 不带 Manager 的 Server，此时 s.m 为 nil。
func (s *Server) db() *db.DB {
	if s == nil || s.m == nil {
		return nil
	}
	return s.m.pg
}

// skillContentHash 返回 skill 目录里 SKILL.md 的内容哈希。
func skillContentHash(skillDir, name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(skillDir, name, "SKILL.md"))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// recordSkillTrust 在 ARTEX 自己的写入路径上落一条信任记录。
//
// 记录绑内容哈希：若文件此后被 ARTEX 之外的路径改动（operator 在宿主机直接编辑、
// git pull），哈希对不上，判为 operator 所为（受信）并刷新记录。因此 ARTEX 的每条
// 写入路径都必须调用本函数——否则 agent 的第二次写入会因哈希不匹配而被误判成
// operator 编辑，从而被当成可信。
func recordSkillTrust(pg *db.DB, skillDir, skillName, tier string) {
	if pg == nil || skillName == "" {
		return
	}
	sum, err := skillContentHash(skillDir, skillName)
	if err != nil {
		return
	}
	_ = pg.SetSetting(skillTrustKeyPrefix+skillName, sum+"|"+tier)
}

// clearSkillTrust 在 skill 被删除时清掉记录。
func clearSkillTrust(pg *db.DB, skillName string) {
	if pg == nil || skillName == "" {
		return
	}
	_ = pg.SetSetting(skillTrustKeyPrefix+skillName, "")
}

// skillTrustTier 判定某个 skill 当前应按哪一档注入。
//
// 无记录 → trusted：skill 随镜像分发或被 operator 直接放在宿主机 skill 目录里，
// 都属 operator 布置。记录在但哈希不匹配 → trusted：文件是经 ARTEX 之外的路径
// 被改的，来源是 operator。
func skillTrustTier(pg *db.DB, skillDir string, sk skill.Skill) string {
	if pg == nil || sk.Dir == "" {
		return skillTierTrusted
	}
	name := filepath.Base(sk.Dir)
	rec, ok, err := pg.GetSetting(skillTrustKeyPrefix + name)
	if err != nil || !ok || rec == "" {
		return skillTierTrusted
	}
	sum, tier, found := strings.Cut(rec, "|")
	if !found {
		return skillTierTrusted
	}
	cur, err := skillContentHash(skillDir, name)
	if err != nil {
		// 读不到内容就没有正文可注入，按可信处理并停止。
		return skillTierTrusted
	}
	if cur != sum {
		// 内容在 ARTEX 之外被改动 → operator 直接编辑 → 受信并刷新记录。
		_ = pg.SetSetting(skillTrustKeyPrefix+name, cur+"|"+skillTierTrusted)
		return skillTierTrusted
	}
	if tier == skillTierUntrusted {
		return skillTierUntrusted
	}
	return skillTierTrusted
}
