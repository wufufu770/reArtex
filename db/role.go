// 运行期角色 / 迁移角色的分工（Phase 0 Task 4：DB 直连靠 DB role 挡）。
//
// 背景：平台原先只有一个数据库凭据，且是超级用户 ⇒ 谁拿到容器里的连接串，
// 就能绕过平台全部 API 层校验直接写库，还能跑 DDL（建表/删表/改结构）。
//
// 这一层把「直连」这条路掐在**数据库权限**上：
//
//	运行期连接（ARTEX_PG_DSN）      —— 受限角色 artex_app：只 CONNECT + USAGE
//	                                  + 业务表 DML + 序列 USAGE。
//	                                  没有 CREATE、没有表所有权 ⇒ 直连也不能 DDL。
//	迁移连接（ARTEX_PG_ADMIN_DSN）  —— 超级用户：**只**用于 schema 应用
//	                                  （db/schema.sql + 各层 Ensure*Schema）
//	                                  与 scripts/db-role-bootstrap.sh。
//
// 两条连接串都只读入一次并从进程环境抹掉：norma 的 Bash 子进程无条件继承
// os.Environ()（norma/tool/task.go:144），留着就等于让 agent 敲一条 env
// 就读出数据库口令。内核那块环境块（/proc/self/environ）抹不掉 ⇒ 所以
// 迁移连接串**优先**从文件读（读入即烧掉磁盘副本），别放环境变量。
package db

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
)

//go:embed role_grants.sql
var roleGrantsSQL string

const (
	// adminDSNEnv 是迁移连接串的环境变量名（不推荐：会留在 /proc/self/environ）。
	adminDSNEnv = "ARTEX_PG_ADMIN_DSN"
	// adminDSNFileEnv 是迁移连接串的文件路径环境变量名（推荐：读入即烧掉）。
	adminDSNFileEnv = "ARTEX_PG_ADMIN_DSN_FILE"
	// appRoleEnv 是受限运行角色的名字（默认 DefaultAppRole）。
	appRoleEnv = "ARTEX_PG_APP_ROLE"
	// DefaultAppRole 是受限运行角色的默认名字。
	DefaultAppRole = "artex_app"
)

// schemaProbeTables 是「schema 已经完整应用」的判定表集。
//
// 为什么逐张列：受限运行角色没有 CREATE 权限 ⇒ schema 必须由迁移连接或
// bootstrap 脚本先应用好。判定「已应用」不能只看一两张表（半截 schema 也会过），
// 也不能只看 db/schema.sql 里的表 —— 有一部分表由 Go 侧的幂等 DDL 建
// （llm_records / llm_usage / strategy_* 那批），它们不在任何 .sql 文件里，
// 脚本侧的 bootstrap 补不出来。
var schemaProbeTables = []string{
	// db/schema.sql 的核心表
	"companies", "assets", "tasks", "agents", "findings", "settings", "tools", "mcp_servers",
	// 各层独立幂等文件建的表
	"chat_groups", "group_quotas", "memory_index", "sod_decisions",
	"sessions", "strategies",
	// Go 侧 DDL 建的表（不在任何 .sql 文件里）
	"llm_records", "llm_usage",
	"strategy_proposals", "strategy_usage", "strategy_replay_corpus",
	"task_strategy_snapshot", "task_strategy_snapshot_meta",
}

// coreSchemaPresenceSQL 判定「schema 已经在库里」的探针。
var coreSchemaPresenceSQL = buildSchemaPresenceSQL(schemaProbeTables)

// buildSchemaPresenceSQL 把表名清单拼成一条 VALUES 查询（表名是常量，
// 不是用户输入；仍然做引号转义，免得将来有人改了清单把 SQL 拼坏）。
func buildSchemaPresenceSQL(tables []string) string {
	quoted := make([]string, 0, len(tables))
	for _, t := range tables {
		quoted = append(quoted, "'"+strings.ReplaceAll(t, "'", "''")+"'")
	}
	return `SELECT count(*) FROM (VALUES (` + strings.Join(quoted, ", ") +
		`)) AS core(name) WHERE to_regclass('public.' || core.name) IS NOT NULL`
}

// canCreateInPublicSQL 判定当前连接角色能否在 public schema 里建对象。
const canCreateInPublicSQL = `SELECT has_schema_privilege(current_user, 'public', 'CREATE')`

var (
	adminDSNCached string
	adminDSNSource string
)

// AdminDSN 解析迁移（超级用户）连接串。
//
// 返回空串表示「没有配置迁移连接」—— 这不是错误：单 DSN 部署照旧用运行期
// 连接应用 schema（那条路径的角色本来就是高权限的）。
//
// 优先级：文件 > 环境变量。与 config.PostgresDSN 的「env > file」**故意相反**：
// 环境变量里的明文口令会留在内核的环境块里（os.Unsetenv 只改 Go 进程那份），
// 同 UID 的 agent 子进程一条 `cat /proc/*/environ` 就读得到 —— 而迁移连接串
// 是超级用户凭据，读到它等于这一层防护整个作废。文件读入即烧掉，暴露窗口小得多。
func AdminDSN() (dsn, source string, err error) {
	if adminDSNCached != "" {
		return adminDSNCached, adminDSNSource, nil
	}
	if p := strings.TrimSpace(os.Getenv(adminDSNFileEnv)); p != "" {
		d, err := readAndBurnAdminDSNFile(p)
		if err != nil {
			return "", "", fmt.Errorf("读取 %s %q 失败: %w", adminDSNFileEnv, p, err)
		}
		adminDSNCached, adminDSNSource = d, "文件 "+p
		_ = os.Unsetenv(adminDSNFileEnv)
		return adminDSNCached, adminDSNSource, nil
	}
	if v := strings.TrimSpace(os.Getenv(adminDSNEnv)); v != "" {
		adminDSNCached, adminDSNSource = v, "环境变量 "+adminDSNEnv
		_ = os.Unsetenv(adminDSNEnv)
		return adminDSNCached, adminDSNSource, nil
	}
	return "", "", nil
}

// readAndBurnAdminDSNFile 读迁移连接串后立刻抹掉磁盘上那份。
//
// 与 config.PostgresDSN 的 DSN 文件同款处理：内容已在内存里，磁盘副本没有
// 存在价值（容器里 /app/data 之类的地方是 agent 的工作区根）。
// 只读挂载（docker secret）抹不掉 ⇒ 只告警，不阻断启动（内容已被读进内存，
// 真正的暴露面是「agent 能读到这个文件」，那由部署方式决定，见
// docs/SECURITY-HARDENING.md）。
func readAndBurnAdminDSNFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	dsn := strings.TrimSpace(string(b))
	if dsn == "" {
		return "", fmt.Errorf("文件为空")
	}
	if err := os.WriteFile(path, nil, 0o600); err == nil {
		_ = os.Remove(path)
	} else {
		log.Printf("[pg] %s 里的迁移连接串抹不掉（只读挂载？）：%v", path, err)
	}
	return dsn, nil
}

// RuntimeRoleName 返回受限运行角色的名字（可用 ARTEX_PG_APP_ROLE 覆盖）。
func RuntimeRoleName() string {
	if v := strings.TrimSpace(os.Getenv(appRoleEnv)); v != "" {
		return v
	}
	return DefaultAppRole
}

// ensureRuntimeRole 冷启动自举（方案 v9 批次1④「已初始化库的探测兜底」）：
// 迁移连接在位时，确保受限运行角色存在且口令与运行期 DSN 同源——
// 全新库（initdb 完成、角色没建过）应用自身即可拉起，不再依赖手工
// CREATE ROLE。幂等：角色在 ⇒ 只对齐口令（secret 是唯一真相源，漂移即自愈）。
//
// 注意 initdb.d 脚本路线不可靠：官方 entrypoint 全程以 postgres 用户跑，
// docker secret 以宿主属主 0600 挂载，postgres 读不了 ⇒ 应用侧自举才是主路径。
func ensureRuntimeRole(ctx context.Context, conn *sql.Conn, role, runtimeDSN string) error {
	user, pass, err := dsnUserPassword(runtimeDSN)
	if err != nil {
		return fmt.Errorf("解析运行期 DSN 凭据: %w", err)
	}
	if user != "" && user != role {
		// 运行期 DSN 用的不是受限角色（如超级用户直连的老部署）——无需自举。
		return nil
	}
	var exists bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname=$1)`, role).Scan(&exists); err != nil {
		return fmt.Errorf("检查角色: %w", err)
	}
	if !exists {
		if _, err := conn.ExecContext(ctx, `CREATE ROLE `+quoteIdent(role)+` LOGIN`); err != nil {
			return fmt.Errorf("创建角色: %w", err)
		}
		log.Printf("[pg] 已自举受限运行角色 %s（冷启动自动化）", role)
	}
	if pass != "" {
		// ALTER ROLE 是 utility 语句，不吃绑定参数 ⇒ 单引号字面量（' 翻倍），
		// standard_conforming_strings=on（默认）下安全。
		lit := "'" + strings.ReplaceAll(pass, "'", "''") + "'"
		if _, err := conn.ExecContext(ctx, `ALTER ROLE `+quoteIdent(role)+` LOGIN PASSWORD `+lit); err != nil {
			return fmt.Errorf("对齐角色口令: %w", err)
		}
	}
	return nil
}

// dsnUserPassword 从连接串解析用户与口令：兼容 URL 形态（postgres://u:p@h/db）
// 与 key=value 形态（host=... user=... password=...）。
func dsnUserPassword(dsn string) (user, pass string, err error) {
	s := strings.TrimSpace(dsn)
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return "", "", err
		}
		if u.User != nil {
			user = u.User.Username()
			pass, _ = u.User.Password()
		}
		return user, pass, nil
	}
	for _, kv := range strings.Fields(s) {
		if k, v, ok := strings.Cut(kv, "="); ok {
			switch k {
			case "user":
				user = v
			case "password":
				pass = v
			}
		}
	}
	return user, pass, nil
}

// quoteIdent 把标识符包成安全的双引号字面量（角色名是受控配置，这里只为规整）。
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// applyRuntimeRoleGrants 把受限运行角色的权限收敛到最小集（幂等）。
//
// 执行的 SQL 是 db/role_grants.sql —— 与 scripts/db-role-bootstrap.sh 同一份
// 文件（Go 侧 //go:embed，脚本侧 psql -f），两边不会漂移。
//
// ⚠️ 角色名通过会话 GUC artex.app_role 传，而 GUC 是**会话级**的 ⇒ 必须与
// SQL 在同一条连接上执行（这里传进来的就是启动期那条被 pin 住的 *sql.Conn）。
//
// ⚠️ GRANT ... ON ALL TABLES 需要表属主或超级用户 ⇒ 只能由迁移连接执行。
func applyRuntimeRoleGrants(ctx context.Context, conn *sql.Conn, role string) error {
	if role == "" {
		role = DefaultAppRole
	}
	if _, err := conn.ExecContext(ctx, `SELECT set_config('artex.app_role', $1, false)`, role); err != nil {
		return fmt.Errorf("设置 artex.app_role: %w", err)
	}
	if _, err := conn.ExecContext(ctx, roleGrantsSQL); err != nil {
		return fmt.Errorf("授予 %s 最小权限: %w", role, err)
	}
	return nil
}

// roleCanDDL 报告当前连接角色能否在 public schema 里建对象。
func roleCanDDL(ctx context.Context, conn *sql.Conn) (bool, error) {
	var can bool
	if err := conn.QueryRowContext(ctx, canCreateInPublicSQL).Scan(&can); err != nil {
		return false, fmt.Errorf("查询 DDL 权限: %w", err)
	}
	return can, nil
}

// coreSchemaPresent 报告核心表是否已齐（schema 已经应用过的判定）。
func coreSchemaPresent(ctx context.Context, conn *sql.Conn) (bool, error) {
	var n int
	if err := conn.QueryRowContext(ctx, coreSchemaPresenceSQL).Scan(&n); err != nil {
		return false, fmt.Errorf("检查核心表: %w", err)
	}
	return n >= len(schemaProbeTables), nil
}

// markRuntimeSchemasReady 把各层 DDL 的「已就位」标记打上（键 = 运行期连接池指针）。
//
// 为什么需要：仓里有若干「首次访问才建表」的懒执行范式（各层 Ensure*Schema），
// 第一次调用会真的跑 DDL。受限运行角色没有 DDL 权限 ⇒ 那些调用会失败
// （SoD / 策略 / 记忆 / 会话 / 配额这些层的运行期入口都在 server 侧调用它们）。
// 启动期已经把 schema 应用过（迁移连接或 bootstrap 脚本）⇒ 这里直接标记就位，
// 运行期的懒执行就变成空操作。
//
// ⚠️ 这份清单与各单元的 memo 变量一一对应。新增 Ensure*Schema 的单元要同步加进来，
// 否则该层会在运行期尝试 DDL 并被权限拒绝（表现是「第一次访问那个功能报权限错」）。
func markRuntimeSchemasReady(pool *sql.DB) {
	if pool == nil {
		return
	}
	quotaSchemaReady.Store(pool, struct{}{})
	chatGroupSchemaReady.Store(pool, struct{}{})
	sessionSchemaReady.Store(pool, struct{}{})
	sessionShareSchemaReady.Store(pool, struct{}{})
	sessionDigestSchemaReady.Store(pool, struct{}{})
	memorySchemaReady.Store(pool, struct{}{})
	memoryRetrievalSchemaReady.Store(pool, struct{}{})
	sodSchemaReady.Store(pool, struct{}{})
	strategySchemaReady.Store(pool, struct{}{})
	strategyProposalSchemaReady.Store(pool, struct{}{})
	strategySnapshotSchemaReady.Store(pool, struct{}{})
	findingValidationSchemaReady.Store(pool, struct{}{})
	taskCommandRuleSchemaReady.Store(pool, struct{}{})
	bgTaskSchemaReady.Store(pool, struct{}{})
	llmRecordsSchemaReady.Store(pool, struct{}{})
	llmUsageSchemaReady.Store(pool, struct{}{})
	metricsSchemaReady.Store(pool, struct{}{})
	legacySchemaReady.Store(pool, struct{}{})
	// 奇点循环执行层（db/looptrack.go）与经验权重卡（db/experience_weight.go）：
	// v0.9.16 事故——这两层漏登记 ⇒ 生产受限角色运行期懒执行 DDL 被
	// permission denied for schema public 拒掉（治理 tab 列空缺/列矛盾、经验
	// 权重记账全部失败）。启动期 schemaDB 已用迁移连接把 DDL 应用过 ⇒ 这里
	// 标记就位，运行期懒执行变空操作。
	loopTrackSchemaReady.Store(pool, struct{}{})
	// 报告中心（db/reports.go）：同上，启动期由迁移连接应用，运行期免建表。
	reportsSchemaReady.Store(pool, struct{}{})
	experienceWeightSchemaReady.Store(pool, struct{}{})
}
