package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Autumn-27/norma/skill"

	"github.com/Autumn-27/artex/db"
)

// Skill 信任分级：决定注入对话的 skill 正文是否按「权威指导」呈现。
//
// A-02（0.15.90）：信任绑定从「SKILL.md 单文件哈希」升级为「整个 skill 目录树
// 哈希」。改单文件哈希时，agent 用 update_skill_file 写 scripts/*.py 引用脚本
// 就能绕开记录——SKILL.md 没变、哈希不变、档位不动，注入的正文里却多了平台
// 看不见的可执行引用。树哈希把目录里每个文件（相对路径 + 内容）都算进摘要，
// 改任何一处都会失配。
//
// 判定语义随树哈希翻转：失配不再默认 trusted。旧逻辑的前提是「ARTEX 之外的
// 改动 = operator 亲手编辑」，但同 UID 部署里 agent 与平台共享文件系统，agent
// 用 Write 工具直接改文件与 operator 宿主机编辑在文件系统层面无法区分——失配
// 默认可信等于把「谁能改文件」升级成「谁能授信」。新语义：失配 ⇒ 降级 untrusted
// + 审计事件，只有 operator 侧显式动作（信任端点 / 管理接口写路径）能把档位
// 升回 trusted。

const (
	skillTierTrusted   = "trusted"
	skillTierUntrusted = "untrusted"

	// skillTrustKeyPrefix 是 settings 表里的键前缀，键名带 skill 目录名。
	// 记录值格式不变："<treeHash>|<tier>"。
	skillTrustKeyPrefix = "skill_trust."

	// skillTrustWriterOperator / skillTrustWriterAgent 是 recordSkillTrust 的
	// 写入方身份。agent 身份的写入永远不产出信任（tier 被强制为 untrusted）：
	// 「信任记录只能由 operator 侧写入」不是靠约定，而是写在函数里。
	skillTrustWriterOperator = "operator"
	skillTrustWriterAgent    = "agent"
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

// skillTreeHash 计算 skill 目录树的哈希：filepath.WalkDir 按字典序遍历，每个
// 文件以「相对路径 \0 长度 \0 内容 \0」进摘要——改内容、加文件、删文件、改名
// 都会算出不同的树哈希。
//
// 忽略 .git 目录与临时文件（编辑器交换文件 / 上传残留）：它们的存在与内容都
// 不是 skill 的有效组成部分，跟着算会让哈希无意义地抖动。
//
// 目录不存在、遍历失败、目录为空（没有可复核的正文）都返回错误——调用方
// 一律 fail-closed 处理，不给「空 = 可信」留口子。
func skillTreeHash(skillDir, name string) (string, error) {
	root := filepath.Join(skillDir, name)
	h := sha256.New()
	sawFile := false
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		base := d.Name()
		if strings.HasSuffix(base, "~") || strings.HasSuffix(base, ".tmp") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sawFile = true
		fmt.Fprintf(h, "%s\x00%d\x00", rel, len(b))
		h.Write(b)
		h.Write([]byte{0})
		return nil
	})
	if err != nil {
		return "", err
	}
	if !sawFile {
		return "", fmt.Errorf("skill %s 目录为空，没有可复核的内容", name)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// recordSkillTrust 在平台自己的写入路径上落一条信任记录。
//
// writer 标明写入方身份：
//   - operator（管理接口 / 信任端点 / 启动 reconcile）：按传入 tier 落记录，
//     operator 显式动作是唯一能把档位升回 trusted 的入口；
//   - agent（create_skill / update_skill_file）：tier 被强制为 untrusted——
//     agent 传什么都不会把 skill 写成 trusted。
//
// 记录绑当前树哈希：写入即重算重钉，此后任何记录之外的改动（agent 直改文件、
// 宿主机编辑、git pull）都会失配而降级，等 operator 复核。
func recordSkillTrust(pg *db.DB, skillDir, skillName, tier, writer string) {
	if pg == nil || skillName == "" {
		return
	}
	if writer == skillTrustWriterAgent {
		tier = skillTierUntrusted
	}
	sum, err := skillTreeHash(skillDir, skillName)
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
// 无库可查 / 无目录 / 无记录 / 记录格式坏 / 树哈希失配 / 记录档位是 untrusted
// ——一律 untrusted（fail-closed）。无记录的 skill（随镜像分发、operator 宿主机
// 直放）由启动 reconcile 或信任端点首次标记，标记完成前按 untrusted 注入，
// 不静默放行。
func skillTrustTier(pg *db.DB, skillDir string, sk skill.Skill) string {
	if pg == nil || sk.Dir == "" {
		return skillTierUntrusted // 无库可查时不默认可信（fail-closed）
	}
	name := filepath.Base(sk.Dir)
	rec, ok, err := pg.GetSetting(skillTrustKeyPrefix + name)
	if err != nil || !ok || rec == "" {
		return skillTierUntrusted
	}
	sum, tier, found := strings.Cut(rec, "|")
	if !found {
		return skillTierUntrusted
	}
	cur, err := skillTreeHash(skillDir, name)
	if err != nil {
		// 读不到 / 空目录：没有可复核的正文，按不可信处理。
		return skillTierUntrusted
	}
	if cur != sum {
		// 树哈希失配：内容在记录之外被改过。同 UID 下 agent 与平台共享文件
		// 系统，「ARTEX 之外改动 = operator」的前提不成立 ⇒ 降级 + 审计事件，
		// 等 operator 显式复核后才可恢复 trusted。
		//
		// 把失配状态重钉成 cur|untrusted：同一次改动的后续注入走「记录档位
		// = untrusted」的稳定路径，审计链只在内容真变时多一条，不按注入次数刷屏。
		_ = pg.SetSetting(skillTrustKeyPrefix+name, cur+"|"+skillTierUntrusted)
		_, _ = pg.AppendAuditEvent(db.AuditEvent{
			Actor:    "platform",
			ToolName: "skill_trust",
			Action:   "skill.demoted",
			Source:   "platform",
			Reason:   "skill " + name + " 的目录树在信任记录之外被改动，降级为 untrusted（A-02）",
			Payload:  []byte(`{"skill":"` + name + `"}`),
		})
		return skillTierUntrusted
	}
	if tier == skillTierTrusted {
		return skillTierTrusted
	}
	return skillTierUntrusted
}

// skillLegacyContentHash 是 A-02 之前的单文件哈希口径（sha256(SKILL.md)），
// 仅用于启动 reconcile 识别存量记录格式并静默迁移（见 reconcileSkillTrust）。
func skillLegacyContentHash(skillDir, name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(skillDir, name, "SKILL.md"))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
