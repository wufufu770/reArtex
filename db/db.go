// Package db is the PostgreSQL data source for ARTEX (取代旧 graph 单文件 SQLite)。
// 它打开连接、应用 schema、并 seed 内置 agent 与变量目录。
package db

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Autumn-27/artex/config"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // pgx database/sql driver ("pgx")
)

//go:embed schema.sql
var schemaSQL string

const schemaMigrationLockKey int64 = 7337741001

// schemaMigrationLockTimeout 是等待 schema 迁移锁的上限。
//
// 为什么要有上限：这个锁是**会话级**的，拿它的那个进程若被强杀（kill -9 / 掉电），
// 它的数据库会话不会正常关闭 ⇒ 锁就留在那个僵尸会话上 ⇒ 之后每一次 db.Open 都会
// 永久阻塞在拿锁上。等不到就明确报错（并告诉运维去哪儿清），好过无限期挂住。
const schemaMigrationLockTimeout = 30 * time.Second

// maxOpenConns 是连接池上限，见 Open 里的说明。取值远低于 PostgreSQL 默认的
// max_connections=100，同时远高于应用自身的嵌套取连接深度（启动期的 schema
// advisory lock 会在持有一条连接的同时让 seedBuiltins 另取连接），不会自锁。
const maxOpenConns = 32

var schemaDeadlockRetryDelays = [...]time.Duration{
	100 * time.Millisecond,
	250 * time.Millisecond,
	500 * time.Millisecond,
	time.Second,
}

type schemaExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func isPostgresDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

func applySchemaWithRetry(ctx context.Context, execer schemaExecer, sleep func(time.Duration)) error {
	for attempt := 0; ; attempt++ {
		if _, err := execer.ExecContext(ctx, schemaSQL); err != nil {
			if !isPostgresDeadlock(err) || attempt >= len(schemaDeadlockRetryDelays) {
				return err
			}
			sleep(schemaDeadlockRetryDelays[attempt])
			continue
		}
		return nil
	}
}

// schemaMigrationHeld 记录**本进程**当前是否已持有 schema 迁移锁（计数器）。
//
// 为什么需要它：withSchemaMigrationLock 用的是【会话级】 advisory lock（它在一条
// 专用连接上持有）；而 coordinateWithSchemaMigration 用的是【事务级】 lock，且
// 事务是从连接池里另取的一条连接 ⇒ 两者即使 key 相同也**互相冲突**（PostgreSQL 的
// 会话级与事务级 advisory lock 共用同一把锁的空间）⇒ 启动期 DDL 自己请求事务级锁时
// 会等自己持有的那把锁 ⇒ 跨连接自锁，表现为「启动时永久挂住」。
//
// ⇒ 进程内再取一次同一把锁没有意义（本进程已经持着它），直接放行。
var schemaMigrationHeld atomic.Int32

// withSchemaMigrationLock pins the session-level lock to one checked-out
// connection. Running pg_advisory_lock through *sql.DB is incorrect because a
// later schema or unlock call may use a different pooled PostgreSQL session.
// schemaApplied 记录某个【数据库】的 schema 与内置种子已经应用过。
//
// 键是数据库身份（host:port/库名），不是连接句柄 —— 同一个库可以被多个连接
// 打开（每个测试实例自建 Manager 就是如此），按句柄记等于没记。
//
// 为什么值得记：schema 应用实测要 1 秒以上（首次 6 秒），而 db.Open 每个
// 测试实例都会调一次 ⇒ 一个包里上百次 Open 就是两分多钟纯耗在重复建表上。
// 实测 server 包因此跑到 553 秒，离 Go 默认的 600 秒包超时只差 47 秒 ——
// 任何额外负载都会把它顶过超时线。
//
// 边界：memo 只在**进程内**有效。跨进程仍由 withSchemaMigrationLock 的
// advisory lock 串行（那是给「两个进程同时起」准备的）；库被重建只发生在
// 各包的 TestMain 里（早于任何 Open），所以 memo 不会陈旧。
var schemaApplied sync.Map

// databaseIdentity 从 DSN 取出数据库身份（host:port/库名）。
//
// 为什么按【库】而不是按连接记：见 schemaApplied 的说明。
func databaseIdentity(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	return u.Host + u.Path
}

func withSchemaMigrationLock(ctx context.Context, sqlDB *sql.DB, action func(*sql.Conn) error) (err error) {
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	// ⚠️ 用「尝试 + 超时」而不是阻塞式 pg_advisory_lock。
	//
	// 阻塞式的问题：这个锁是**会话级**的（pg_advisory_xact_lock 才是事务级）。
	// 持锁的进程若被 kill -9 / 掉电，它的数据库会话**不会**正常关闭 ⇒ 锁留在
	// 那个会话上 ⇒ 之后**每一个** db.Open 都永久阻塞在拿锁上 ⇒ 应用再也起不来，
	// 只能手工去 pg_stat_activity 里终止那个僵尸会话。
	//
	// 本机就撞到过这个：一次被强制超时的测试留下了持锁会话，之后所有
	// db.Open 都卡在 coordinateWithSchemaMigration，应用起不来。
	//
	// 改成 try + 超时之后，最坏情况是**明确报错**（并告诉运维去哪儿清僵尸会话），
	// 而不是无限期挂住。
	lockDeadline := time.Now().Add(schemaMigrationLockTimeout)
	for {
		var locked bool
		if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, schemaMigrationLockKey).Scan(&locked); err != nil {
			return fmt.Errorf("advisory lock: %w", err)
		}
		if locked {
			break
		}
		if time.Now().After(lockDeadline) {
			return fmt.Errorf("advisory lock: 等待 %s 仍未获得 schema 迁移锁。"+
				"若持锁的进程已被强制杀死，它的会话可能仍在数据库里持锁（会话级 advisory lock 不会随进程消失而释放）——"+
				"请在 pg_stat_activity 里找出持有该锁的会话并终止它",
				schemaMigrationLockTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	schemaMigrationHeld.Add(1)
	defer func() {
		schemaMigrationHeld.Add(-1)
		if _, unlockErr := conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, schemaMigrationLockKey); unlockErr != nil && err == nil {
			err = fmt.Errorf("advisory unlock: %w", unlockErr)
		}
	}()
	return action(conn)
}

// coordinateWithSchemaMigration makes long, multi-table archive transactions
// mutually exclusive with startup DDL while allowing ordinary runtime queries
// to continue normally.
func coordinateWithSchemaMigration(tx *sql.Tx) error {
	// 本进程已经持有 schema 迁移锁（启动期 DDL 正在锁内跑）⇒ 不能再从另一条
	// 连接去要同一把锁：会话级与事务级同 key 互相冲突 ⇒ 那会等自己持有的锁
	// （跨连接自锁，表现为启动永久挂住）。此时锁已经被本进程持有，直接放行。
	if schemaMigrationHeld.Load() > 0 {
		return nil
	}
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock($1)`, schemaMigrationLockKey); err != nil {
		return fmt.Errorf("coordinate with schema migration: %w", err)
	}
	return nil
}

// DSN resolves the PostgreSQL connection string and reports where it came from.
// Precedence: env ARTEX_PG_DSN > config file (config.json). There is no
// built-in default — it errors if neither source is configured.
func DSN() (dsn, source string, err error) {
	if dsnCached != "" {
		return dsnCached, dsnSource, nil
	}
	dsn, source, err = config.PostgresDSN()
	if err != nil {
		return "", "", err
	}
	dsnCached, dsnSource = dsn, source
	// 读入即从进程环境抹掉。norma 的 Bash 子进程无条件继承 os.Environ()
	// （norma/tool/task.go:144 的 append(os.Environ(), extra...)），留着就等于
	// 让 agent 敲一条 env / printenv 就读出数据库口令。
	//
	// 抹除放在这一层而不是 config.PostgresDSN()：后者是通用的取值函数，缓存会改掉
	// 它的「env > 配置文件」优先级语义；这一层每进程只被启动期的 NewManager 调一次，
	// 值在进程生命周期内本就不变。
	_ = os.Unsetenv("ARTEX_PG_DSN")
	return dsnCached, dsnSource, nil
}

var (
	dsnCached string
	dsnSource string
)

// DB wraps the shared *sql.DB. PG handles its own connection pool + concurrency
// (MVCC), so unlike the old SQLite store there is no process-wide write mutex.
type DB struct {
	*sql.DB
	// admin 是**迁移连接**（超级用户）池，只用于 schema 应用与给运行期角色授权。
	//
	// 为 nil ⇒ 没配置迁移连接串（单 DSN 部署）⇒ schema 应用走运行期连接，
	// 与本层落地之前的行为一致。
	//
	// 为什么单独一个池而不是复用运行期连接：运行期角色刻意没有 DDL 权限
	// （见 db/role.go）⇒ 建表/改表必须换一条有 DDL 权限的连接。
	admin *sql.DB
	// toolTableFallback 报「工具表不可读但仍有上一份可用快照」。
	// 由 server 装配时注册（见 server/assembly.go 的 wireTools）。
	toolTableFallback func() bool
	// listTools 读工具表，默认用 d.ListTools。抽成字段是为了让「工具表不可读
	// 时拒绝创建 run」这道闸能在没有数据库的情况下被测试。
	listTools func() ([]*Tool, error)
}

// Close 关掉连接池。
//
// 迁移连接池（admin）也要关：它是超级用户凭据的持有者，进程退出时不该留着
// 打开的会话。迁移连接池没有时只关运行期池。
func (d *DB) Close() error {
	err := d.DB.Close()
	if d.admin != nil {
		if adminErr := d.admin.Close(); err == nil {
			err = adminErr
		}
	}
	return err
}

// ensureDatabase connects to the postgres system database and creates the target
// database if it does not exist. dsn must be a postgres:// URL.
func ensureDatabase(dsn string) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil // unparseable DSN — let the normal Open fail with a clear error
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	if dbName == "" || dbName == "postgres" {
		return nil
	}
	// connect to the postgres maintenance database instead
	adminDSN := *u
	adminDSN.Path = "/postgres"
	admin, err := sql.Open("pgx", adminDSN.String())
	if err != nil {
		return nil // best-effort; let Open surface the real error
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		return nil
	}
	var exists bool
	_ = admin.QueryRow(`SELECT true FROM pg_database WHERE datname=$1`, dbName).Scan(&exists)
	if !exists {
		if _, err := admin.Exec(`CREATE DATABASE "` + dbName + `"`); err != nil {
			return fmt.Errorf("create database %q: %w", dbName, err)
		}
	}
	return nil
}

// Open connects, applies the schema (idempotent), and seeds builtin rows.
//
// 连接分工（见 db/role.go）：
//   - dsn（运行期）：受限角色 artex_app。普通查询与全部业务读写走它。
//   - ARTEX_PG_ADMIN_DSN（迁移）：超级用户。**只**用于 schema 应用
//     （db/schema.sql + 各层 Ensure*Schema）与给运行期角色授权。
//
// 没配迁移连接串时 schema 应用退回运行期连接 —— 单 DSN 部署照旧工作
// （那条路径的角色本来就是高权限的，不需要这一层防护）。
func Open(dsn string) (*DB, error) {
	adminDSN, adminSource, err := AdminDSN()
	if err != nil {
		return nil, err
	}
	// 建库与 schema 应用都必须走「有 DDL 权限」的那条连接。
	schemaDSN := dsn
	if adminDSN != "" {
		schemaDSN = adminDSN
	}
	if err := ensureDatabase(schemaDSN); err != nil {
		return nil, err
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	// database/sql 默认不限制连接数：池里没有空闲连接时会无条件新建，一路顶到
	// PostgreSQL 的 max_connections（默认 100）才被拒，于是高峰期的查询拿到的是
	// `FATAL: sorry, too many clients already` 这种**错误**。封顶之后超额查询改为
	// 排队等待空闲连接——同样的负载下变成变慢而不是报错，调用方不必再去区分
	// "读不到"和"没有"。maxOpenConns 要留出余量给 psql / reset-password.sh 以及
	// 可能并存的其他实例；若 max_connections 调低过，这里也要跟着往下调。
	sqlDB.SetMaxOpenConns(maxOpenConns)
	sqlDB.SetMaxIdleConns(maxOpenConns)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)
	// 注意：这里【不】先 ping 运行期池——全新库上运行角色还不存在，得等下面
	// 的迁移闭包自举（v9 批次1④）之后再验连通性，否则冷启动被误杀。
	d := &DB{DB: sqlDB}
	// 迁移连接池：只在配置了迁移连接串时存在。没有它 ⇒ schema 应用走运行期连接。
	schemaPool := sqlDB
	if adminDSN != "" {
		adminPool, err := sql.Open("pgx", adminDSN)
		if err != nil {
			sqlDB.Close()
			return nil, err
		}
		adminPool.SetMaxOpenConns(maxOpenConns)
		adminPool.SetMaxIdleConns(maxOpenConns)
		adminPool.SetConnMaxLifetime(30 * time.Minute)
		adminPool.SetConnMaxIdleTime(5 * time.Minute)
		if err := adminPool.Ping(); err != nil {
			adminPool.Close()
			sqlDB.Close()
			return nil, fmt.Errorf("ping postgres (迁移连接, %s): %w", config.Redact(adminDSN), err)
		}
		d.admin = adminPool
		schemaPool = adminPool
		log.Printf("[pg] 迁移连接已就绪（%s）：%s", adminSource, config.Redact(adminDSN))
	}
	// pgx runs multi-statement Exec via the simple protocol when there are no args.
	// Keep the dedicated lock connection checked out until both DDL and seeding
	// finish so concurrent application instances cannot initialize out of order.
	schemaKey := databaseIdentity(schemaDSN)
	err = withSchemaMigrationLock(context.Background(), schemaPool, func(conn *sql.Conn) error {
		// schemaDB 的底层连接就是 schemaPool ⇒ 这里的每一次 DDL 都跑在有 DDL
		// 权限的连接上（受限运行角色建不了表）。
		schemaDB := &DB{DB: schemaPool}
		// 没配迁移连接 ⇒ schema 应用跑在运行期连接上。若该角色连 CREATE 都没有
		//（受限角色），schema 必须已经在库里（bootstrap 脚本应用过）⇒ 只校验
		// 核心表在不在：在就跳过，不在就明确报错让人去补迁移连接。
		if adminDSN == "" {
			canDDL, err := roleCanDDL(context.Background(), conn)
			if err != nil {
				return err
			}
			if !canDDL {
				present, err := coreSchemaPresent(context.Background(), conn)
				if err != nil {
					return err
				}
				if !present {
					return fmt.Errorf("当前连接角色无权在 public schema 建对象（CREATE 被拒），且核心表缺失。" +
						"请设置 ARTEX_PG_ADMIN_DSN（迁移连接，超级用户）后重试，或先运行 scripts/db-role-bootstrap.sh 把 schema 应用好")
				}
				log.Printf("[pg] 运行期角色无 DDL 权限且核心 schema 已就位 ⇒ 跳过 schema 应用")
				markRuntimeSchemasReady(sqlDB)
				return nil
			}
		}
		// schema 与内置种子按【数据库】只做一次（见 schemaApplied 的说明）。
		if _, done := schemaApplied.Load(schemaKey); !done {
			if err := applySchemaWithRetry(context.Background(), conn, time.Sleep); err != nil {
				return fmt.Errorf("apply schema: %w", err)
			}
			if err := schemaDB.seedBuiltins(); err != nil {
				return fmt.Errorf("seed builtins: %w", err)
			}
			schemaApplied.Store(schemaKey, struct{}{})
		}
		// 群组预算 DDL：与 db/schema.sql 并行应用（见 db/quota.go 的
		// EnsureQuotaSchema）。预算表外键指向 chat_groups，因此它内部会先
		// 补齐群聊 DDL。
		if err := schemaDB.EnsureQuotaSchema(); err != nil {
			return fmt.Errorf("apply quota schema: %w", err)
		}
		// 记忆层（方案 B）DDL：append-only 的版本化记忆索引，走独立幂等文件
		// （db/schema_memory.sql）。启动时接 ⇒ 库里没有这张表这件事在启动期就暴露，
		// 而不是等某个接口运行期才报「关系不存在」。
		if err := schemaDB.EnsureMemorySchema(); err != nil {
			return fmt.Errorf("apply memory schema: %w", err)
		}
		// 奇点循环执行层（方案 v10 §12+§16 批次 F2-F5）DDL：intent_predictions /
		// contradictions / gaps 三表，走独立幂等文件（db/schema_loop.sql）。
		// 启动时接的理由与其余 Ensure*Schema 相同：库里没表这件事要在启动期暴露，
		// 而不是等矛盾检测器在 worker 终态路径上运行期报「关系不存在」。
		if err := schemaDB.EnsureLoopSchema(); err != nil {
			return fmt.Errorf("apply loop schema: %w", err)
		}
		// 经验权重卡（方案 v10 §6 T1/X1）DDL：experience_weights 记账表。
		// ⚠️ v0.9.16 事故：这层漏接启动流程 ⇒ 生产库（受限运行角色）首次记账时
		// 运行期懒执行 DDL 被 permission denied for schema public 拒掉（日志
		// 「[legacy] 经验 #N 权重命中记账失败」），经验权重在生产永远记不上。
		// 与上面 EnsureLoopSchema 同理：必须在启动期用有 DDL 权限的连接建好，
		// 运行期只标记就位（markRuntimeSchemasReady）。
		if err := schemaDB.EnsureExperienceWeightSchema(); err != nil {
			return fmt.Errorf("apply experience weight schema: %w", err)
		}
		// finding 级验证态与返工边（方案书 §7 的 P0-2）：三态 + rework 边，
		// 走独立幂等文件（db/schema_finding_validation.sql）。
		if err := schemaDB.EnsureFindingValidationSchema(); err != nil {
			return fmt.Errorf("apply finding validation schema: %w", err)
		}
		// 会话实体化 DDL：会话成为第一类对象（sessions / session_members /
		// session_attachments / session_messages），走独立幂等文件
		// （db/schema_session.sql）。
		//
		// 为什么必须在这里接：会话表不接启动流程 ⇒ 库里没有这几张表 ⇒ 会话相关
		// 接口全在运行期才报「关系不存在」，而不是启动期就报出来。幂等
		// （CREATE TABLE IF NOT EXISTS）⇒ 重复调用无副作用。
		if err := schemaDB.EnsureSessionSchema(); err != nil {
			return fmt.Errorf("apply session schema: %w", err)
		}
		// 策略库（L4）DDL：同样走独立幂等文件（db/schema_strategy.sql）。
		//
		// 为什么必须在这里接：策略表不接启动流程 ⇒ 库里没有这几张表 ⇒
		// 策略相关接口全在运行期才报「关系不存在」，而不是启动期就报出来。
		// 失败即启动失败，不静默跳过。
		if err := schemaDB.EnsureStrategySchema(); err != nil {
			return fmt.Errorf("apply strategy schema: %w", err)
		}
		// 策略库的第二批表（strategy_proposals / strategy_usage /
		// strategy_replay_corpus / task_strategy_snapshot）的 DDL。
		//
		// 为什么必须在启动时接：它们的 DDL 写在 Go 字符串常量里（访问层就近定义），
		// 于是它们原本是**懒执行**的 —— 只有第一次用到该表时才建。懒执行的坏处是
		// 「库里没有这张表」这件事只在运行期的某个接口上暴露，而不是启动期 ⇒
		// 排障时看到的是「关系不存在」这种底层报错，而不是「启动期就报出来」。
		// 幂等（CREATE TABLE IF NOT EXISTS）⇒ 重复调用无副作用。
		if err := schemaDB.EnsureStrategySnapshotSchema(); err != nil {
			return fmt.Errorf("apply strategy snapshot schema: %w", err)
		}
		// 职责分离链的 DDL：裁决留痕、待对抗复核队列、工具熔断状态。
		//
		// 为什么必须在这里接：不接 ⇒ 库里没有这三张表 ⇒ 闸门写裁决时运行期才报
		// 「关系不存在」，而不是启动期就报出来。
		if err := schemaDB.EnsureSoDSchema(); err != nil {
			return fmt.Errorf("apply sod schema: %w", err)
		}
		// 任务拦截规则的 kind 枚举放宽（含命令级唯一索引）：在启动时补一次。
		// 不能留在写路径上懒执行 —— 重建 CHECK 约束要拿这张热表的表级锁，
		// 同库并发时互相排队。
		if err := schemaDB.EnsureTaskInterceptSchema(); err != nil {
			return fmt.Errorf("apply task intercept schema: %w", err)
		}
		// llm_records / llm_usage 不在 db/schema.sql 里（这两张表由它们自己的
		// 幂等 DDL 建，见 db/commands.go 与 db/llm_usage.go）⇒ 必须在启动期用
		// 有 DDL 权限的连接建好：受限运行角色建不了表。
		if err := schemaDB.EnsureLLMRecordsTable(); err != nil {
			return fmt.Errorf("apply llm_records schema: %w", err)
		}
		if err := schemaDB.EnsureLLMUsageTable(); err != nil {
			return fmt.Errorf("apply llm_usage schema: %w", err)
		}
		// 运行度量（P0）：llm_profile_prices 单价表，同为 Go 侧幂等 DDL。
		if err := schemaDB.EnsureMetricsSchema(); err != nil {
			return fmt.Errorf("apply metrics schema: %w", err)
		}
		// 传承图（P5，第三张图）：legacy_nodes/edges/usage。
		if err := schemaDB.EnsureLegacySchema(); err != nil {
			return fmt.Errorf("apply legacy schema: %w", err)
		}
		// 报告中心（/system/reports）：reports 表，独立幂等文件
		// （db/schema_reports.sql）。正文整篇存库——必须在启动期用有 DDL 权限的
		// 连接建好（与上方各 Ensure*Schema 同理，受限运行角色建不了表）。
		if err := schemaDB.EnsureReportsSchema(); err != nil {
			return fmt.Errorf("apply reports schema: %w", err)
		}
		// 后台命令（bg_task，db/bg_task.go）：memo 名单里有但启动序列漏接的第三例
		//（reports 之后）——主控 bg 命令登记/查询在受限角色下 42P01。同款修法。
		if err := schemaDB.EnsureBgTaskSchema(); err != nil {
			return fmt.Errorf("apply bg_task schema: %w", err)
		}
		// 业务建模（第三张图）的 DDL：biz_nodes/edges/anchors/layer_diffs/
		// model_versions 五表（db/schema_biz.sql）。启动期建好的理由同上各层：
		// 受限运行角色建不了表，懒执行会在建模 agent 第一次写图时报 42P01。
		if err := schemaDB.EnsureBizSchema(); err != nil {
			return fmt.Errorf("apply biz schema: %w", err)
		}
		// 偏锋探索的 DDL：edge_cells/edge_runs 两表（db/schema_edge.sql）。同上。
		if err := schemaDB.EnsureEdgeSchema(); err != nil {
			return fmt.Errorf("apply edge schema: %w", err)
		}
		// 小程序渗透的 DDL：miniprogram_tokens 一表（db/schema_miniprogram.sql）。
		// 同上——token 池的表要在启动期建好，受限运行角色建不了表。
		if err := schemaDB.EnsureMiniprogramSchema(); err != nil {
			return fmt.Errorf("apply miniprogram schema: %w", err)
		}
		// 把受限运行角色的权限收敛到最小集（幂等）。表集随 schema 演进，
		// 所以每次启动都重授一次：新增的表自动进授权范围，不用重跑脚本。
		if adminDSN != "" {
			// 冷启动自举（v9 批次1④）：角色缺失即自建、口令漂移即对齐——
			// 全新库不再需要手工 CREATE ROLE。
			if err := ensureRuntimeRole(context.Background(), conn, RuntimeRoleName(), dsn); err != nil {
				log.Printf("[pg] 运行角色自举失败: %v（请重跑 scripts/db-role-bootstrap.sh）", err)
			}
			if err := applyRuntimeRoleGrants(context.Background(), conn, RuntimeRoleName()); err != nil {
				log.Printf("[pg] 给受限运行角色 %s 授权失败: %v（请重跑 scripts/db-role-bootstrap.sh）", RuntimeRoleName(), err)
			}
		}
		return nil
	})
	if err != nil {
		sqlDB.Close()
		if d.admin != nil {
			d.admin.Close()
		}
		return nil, err
	}
	// 运行期连通性校验：自举之后才 ping（全新库的角色此刻才刚建好）。
	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		if d.admin != nil {
			d.admin.Close()
		}
		return nil, fmt.Errorf("ping postgres (%s): %w", config.Redact(dsn), err)
	}
	// 各层 Ensure*Schema 的「已就位」标记按**运行期连接池**打上：运行期的懒
	// 执行 DDL 因此变成空操作（受限角色跑不了 DDL）。
	markRuntimeSchemasReady(sqlDB)
	return d, nil
}

// builtinAgent describes one of the fixed agents and its prompt-variable catalog.
type builtinAgent struct {
	key, name, role, desc string
	vars                  []promptVar
	interactiveShell      bool // 建行时的默认交互式 shell 开关；ON CONFLICT 不覆盖用户后续手动开关
	runSeconds            *int // 建行时的单次 run 墙钟上限(秒)；nil=用种子默认(1200)，0=不限时
}

type promptVar struct{ name, desc, example, source string }

// intp 返回 v 的指针，用于给 builtinAgent 可选字段(如 runSeconds)显式取值。
func intp(v int) *int { return &v }

// builtinAgents mirrors docs §5(a). 内置工具不入库；这里只 seed agent + 变量目录。
// 注：planner/worker/mainagent/auto 的交互式 shell 默认由下方 interactive_shell_default_v1
// 块统一置 true（尊重后续 toggle）；这里的 interactiveShell 只给需要「建行即默认开」的新 agent。
var builtinAgents = []builtinAgent{
	{"goals", "目标拆解", "goals", "把渗透任务目标拆解成若干独立、可验证的子目标。", []promptVar{
		{"EngagementDescription", "任务描述（测试对象/背景）", "测试 example.com 站点", "exploration"},
		// Now 是全局 runtime 变量(见 server.globalPromptVars),不再在各 agent 目录里
		// 重复定义,否则 withGlobalVars 追加时会与全局项撞名。
	}, false, nil},
	{"planner", "规划", "planner", "读取态势、判定目标，只在确有未覆盖的新方向时补充探索意图（每任务一个规划循环）。", []promptVar{
		{"Goal", "任务总目标", "拿下 example.com 的管理员权限", "exploration"},
		{"AssetSummary", "资产计数/类型分布摘要(可选)", "domain:3 ip:5 site:2", "distilled"},
	}, false, nil},
	{"mainagent", "主", "main", "人机接口：观察进展，把人的意图落成 hint 或高优先级意图。", []promptVar{
		{"Goal", "当前任务目标", "拿下 example.com 的管理员权限", "exploration"},
		{"AssetSummary", "开局态势摘要(可选)", "domain:3 ip:5", "distilled"},
		{"FindingsSummary", "已确认漏洞摘要(可选)", "high:1 medium:2", "distilled"},
	}, false, nil},
	{"worker", "执行", "worker", "领取一条意图执行，把发现的事实/漏洞写回知识图谱后停止。", []promptVar{
		{"ProxyAddr", "记录代理地址(驱动 if 双文案)", "127.0.0.1:8080", "runtime"},
		{"WorkerName", "worker 自我标识(可选)", "worker-1", "runtime"},
	}, false, nil},
	// Auto:内置「平台操作」agent。不参与渗透编排循环,经对话页驱动,用工具操作平台。
	{"auto", "Auto", "assistant", "平台操作助手：用工具管理任务(建/看/暂停/给提示)与资产，并可创建/修改 skill、自定义工具、MCP。", nil, false, nil},
	// 渗透测试:内置「独立渗透」agent。经对话页驱动,一人从侦察到收尾走完整条渗透链,自己规划自己执行自己验证。默认开启交互式 shell。
	{"pentest", "渗透测试", "assistant", "独立渗透 agent：一人从侦察→找攻击面→深入利用→验证→收尾走完整条链，自己规划、自己执行、自己对抗式验证。", nil, true, intp(0)},
	// 资产收集(recon):内置「资产收集」agent(方案 v4 §3-C)。经对话页/群聊驱动,按 P0-P9 流程先被动后主动
	// 地收集资产,一切发现结构化入库;只收集不利用。与 pentest 同属会话型长跑 agent:单次 run 不限时(0)。
	// 不开交互式 shell(收集面用 Bash 一次性命令足够);联网搜索默认开见下方 recon_web_search_default_v1 块。
	{"recon", "资产收集", "assistant", "资产收集 agent：按「授权确认→被动扩展→主动枚举→解析存活→端口服务→指纹→内容深挖→CDN 溯源→去重入库→汇总」走完收集链，先被动后主动，一切发现结构化入库；只收集不利用。", nil, false, intp(0)},
	// 顾问型五件套（策略库 / 职责分离阶段）：只【提议】、不【裁决】。它们不参与渗透编排循环，
	// 经对话页/触发器驱动，与 auto/pentest 同属「assistant」类会话型 agent。
	// 工具绑定由 db 层策略决定（见 config.go 的 AdvisoryAgentToolPolicies）：
	// 只给只读与「自己的建议通道」工具，绝不给改写共享状态的工具。
	{"curator", "策略仲裁", "assistant", "策略库仲裁者：依据冻结重放得分、canary 通过率变化、独立确认计数与机械重放证伪，给出 adopt/reject/defer 裁决，不写任何共享状态。", nil, false, nil},
	{"verifier", "建议验证", "assistant", "建议性验证者：只读证据给出 PASS/FAIL/UNCERTAIN 建议，最终判定由确定性层做出，不改任何漏洞与策略状态。", nil, false, nil},
	{"redteam", "证伪", "assistant", "证伪者：在任务 scope 内主动证伪已确认漏洞与高后验策略，只产出可机械重放的证伪记录，不产正面漏洞。", nil, true, nil},
	{"creative", "新颖探索", "assistant", "新颖性探索者：提出与现有策略库距离足够远的新策略与场景特征候选，交由仲裁者裁决，不直接写策略库。", nil, false, nil},
	// intent-normalizer（规整器）：worker 的原始动作 → 结构化意图，交给确定性闸门裁决。
	// 它是职责分离链上唯一新增的 LLM 环节，因此工具面收得最紧：白名单里只有
	// emit_structured_intent 一个工具（见 config.go 的 AdvisoryAgentToolPolicies）。
	// 不给交互式 shell——它只转换，不动手。
	{"intent_normalizer", "意图规整", "assistant", "结构化意图转换者：把 worker 的原始动作（shell 命令 / HTTP 请求 / 工具参数）转成结构化意图，交给确定性闸门裁决；不执行、不改写语义、不查 scope。", nil, false, nil},
	// 偏锋探索（EDGE E0）：方法论顾问（轻量）——出现在会话选择器，可咨询方法论、
	// 查判据进度、出候选假设；完整运行由 edge_exec 承担。与顾问五件套同属
	// 「只提议」类 assistant 会话型 agent。
	{"edge", "偏锋探索", "assistant", "偏锋探索：在既有探索图之上，用可注册的方法论（STPA/pre-mortem/逆向与负空间等）驱动一条与常规路径不同的探索线——方法论咨询、判据进度查询、候选假设；完整探索运行由「偏锋探索·执行」承担。", nil, false, nil},
	// 偏锋探索·执行（EDGE E0）：完整偏锋运行的调度与判据收尾——查判据进度、下发
	// 偏锋假设、判完成、封格、换方法论。长跑由任务引擎承担（spawn_task timeout=0），
	// 它自己是每轮 ≤15min 的总控轮；runSeconds=0（不限时）与 pentest/recon 同款，
	// 语义是「触发运行的 run 不设墙钟」，轮间隔由 fireEdgeWake 轮转驱动。
	// ⚠️ chat_visible=false：执行体藏出会话选择器（seedBuiltins 落库后统一置 false，
	// 见下方 edge_exec_chat_visible_v1 块）——选择器里只出现方法论那个 agent。
	{"edge_exec", "偏锋探索·执行", "assistant", "偏锋探索的执行体：在指定任务上做总控——查判据进度、下发偏锋假设、判完成、封格、换方法论；判据成立即确定性收尾。", nil, false, intp(0)},
	//
	// 以下三者是【确定性基础设施】，刻意不作为 agent 落库：
	//   - policy-gate：它的裁决（scope、tier2 一致性、工具熔断、群组额度状态、anomaly 标记）
	//     全是确定性检查，裁决顺序是短路规则表。它的规范写在 agent/prompts/policy-gate.md，
	//     但那是【确定性闸门的规格】，不是 LLM 提示词——登记成 agent 等于让模型接手裁决，
	//     违反「模型只提议，确定性层裁决」这条铁律。故：无提示词文件、无 agent 行、无工具绑定。
	//   - executor：执行已批准的意图，同样是确定性代码。无提示词文件、无 agent 行、无工具绑定。
	//   - auditor：审计执行结果，同样是确定性代码。无提示词文件、无 agent 行、无工具绑定。
}

// seedBuiltins inserts the fixed built-in agents and their variable catalog (idempotent).
func (d *DB) seedBuiltins() error {
	for _, a := range builtinAgents {
		var agentID int64
		err := d.QueryRow(`
INSERT INTO agents(key, name, description, role, builtin, enabled, interactive_shell, run_seconds)
VALUES ($1, $2, NULLIF($3,''), $4, true, true, $5, COALESCE($6, 1200))
ON CONFLICT (key) DO UPDATE SET name = EXCLUDED.name, description = EXCLUDED.description
RETURNING id`, a.key, a.name, a.desc, a.role, a.interactiveShell, a.runSeconds).Scan(&agentID)
		if err != nil {
			return fmt.Errorf("agent %s: %w", a.key, err)
		}
		for _, v := range a.vars {
			if _, err := d.Exec(`
INSERT INTO agent_prompt_vars(agent_id, var_name, description, example, source)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (agent_id, var_name) DO UPDATE
  SET description = EXCLUDED.description, example = EXCLUDED.example, source = EXCLUDED.source`,
				agentID, v.name, v.desc, v.example, v.source); err != nil {
				return fmt.Errorf("agent %s var %s: %w", a.key, v.name, err)
			}
		}
	}
	// Drop catalog entries for variables that were renamed, so the white-list no
	// longer advertises a name templates can't resolve (EngagementTitle→Description).
	// 'Now' 从各 agent 目录提升为全局 runtime 变量后,旧库里 goals 仍残留一条 'Now'
	// 会与全局项撞名(前端变量列表 key 重复);一并清掉。
	if _, err := d.Exec(`DELETE FROM agent_prompt_vars WHERE var_name IN ('EngagementTitle', 'CoverageGaps', 'Now')`); err != nil {
		return fmt.Errorf("cleanup renamed vars: %w", err)
	}
	// Default-on interactive_shell for the runtime agents (planner/worker/mainagent/auto)
	// ONCE — respects a later user toggle-off (guarded by a settings flag). goals(one-shot
	// decomposer) stays off. Runs after the column exists (schema applied before seed).
	if v, _, _ := d.GetSetting("interactive_shell_default_v1"); v != "true" {
		if _, err := d.Exec(`UPDATE agents SET interactive_shell=true WHERE key IN ('planner','worker','mainagent','auto')`); err != nil {
			return fmt.Errorf("seed interactive_shell defaults: %w", err)
		}
		_ = d.SetSetting("interactive_shell_default_v1", "true")
	}
	// recon(资产收集)默认开联网搜索 ONCE:P1 被动扩展(搜索引擎/公开情报)离不开 web_search。
	// 一次性(settings 旗标)——用户之后在 agent 页关掉不会被重启复活(同上方 interactive_shell 块)。
	// 这里只开 per-agent 开关;搜索后端/Key 仍受系统全局联网搜索配置门控。
	if v, _, _ := d.GetSetting("recon_web_search_default_v1"); v != "true" {
		if _, err := d.Exec(`UPDATE agents SET web_search=true WHERE key='recon'`); err != nil {
			return fmt.Errorf("seed recon web_search default: %w", err)
		}
		_ = d.SetSetting("recon_web_search_default_v1", "true")
	}
	// 内置任务类别(资产收集/渗透测试/漏洞验证,方案 v4 §3-C2):首次启动播一次,settings 旗标
	// 防重播(用户删改不复活)。引擎按类别给 worker 拼纪律段,见 db/task_category_seed.go。
	if err := d.seedDefaultTaskCategories(); err != nil {
		return fmt.Errorf("seed task categories: %w", err)
	}
	// 功能扩展类别(业务建模/偏锋探索):独立旗标,幂等语义同上——业务建模任务的
	// 类别落点与偏锋探索热切(SetTaskCategory)的目标类别都在这里。
	if err := d.seedBizEdgeTaskCategories(); err != nil {
		return fmt.Errorf("seed biz/edge task categories: %w", err)
	}
	// edge_exec 藏出会话选择器（EDGE E1）：种子行落库后置 chat_visible=false。
	// 一次性旗标：用户之后手动把它开回选择器是有意的，重启不复活。
	if v, _, _ := d.GetSetting("edge_exec_chat_visible_v1"); v != "true" {
		if _, err := d.Exec(`UPDATE agents SET chat_visible=false WHERE key='edge_exec'`); err != nil {
			return fmt.Errorf("seed edge_exec chat_visible: %w", err)
		}
		_ = d.SetSetting("edge_exec_chat_visible_v1", "true")
	}
	// Seed the built-in browser (Playwright) MCP once — DISABLED by default (用户
	// 需要时自行启用), no proxy by default. The traffic-capture toggle injects/strips
	// the recording proxy + CA at runtime (server.Manager.syncBrowserMCPProxy).
	// Insert only if absent so we never clobber user edits (args/env/enabled/
	// visibility) on restart.
	if _, err := d.Exec(`
INSERT INTO mcp_servers(name, transport, command, args, env, enabled)
VALUES ('browser', 'stdio', 'npx', $1, '{}', false)
ON CONFLICT (name) DO NOTHING`,
		`["@playwright/mcp","--headless"]`); err != nil {
		return fmt.Errorf("seed browser mcp: %w", err)
	}
	// v0.9.17：playwright-cli 的 MCP 入口（用户明确要求「playwright-cli 的 mcp
	// 要配置上」，此前库里只有名字叫 browser 的同一套 @playwright/mcp，与
	// playwright-cli skill 对不上号）。与 browser 同一二进制、默认启用；可见性
	// 给全部内置 agent（ON CONFLICT DO NOTHING：用户改过就尊重用户的）。
	// ⚠️ command 必须是绝对路径：enabled=true 会在启动自动发现时过 stdio 命令
	// 守卫——npx 这类包执行器等价于任意第三方代码执行（包名可抢注），会被
	// 「禁止使用包执行器」拒绝（镜像内 @playwright/mcp 全局安装 ⇒
	// /usr/bin/playwright-mcp 符号链接总是存在）。
	if _, err := d.Exec(`
INSERT INTO mcp_servers(name, transport, command, args, env, enabled)
VALUES ('playwright-cli', 'stdio', '/usr/bin/playwright-mcp', $1, '{}', true)
ON CONFLICT (name) DO NOTHING`,
		`["--headless"]`); err != nil {
		return fmt.Errorf("seed playwright-cli mcp: %w", err)
	}
	if _, err := d.Exec(`
INSERT INTO agent_visibility(agent_id, resource_kind, resource_id, enabled)
SELECT a.id, 'mcp', m.id, true FROM agents a
JOIN mcp_servers m ON m.name='playwright-cli'
ON CONFLICT DO NOTHING`); err != nil {
		return fmt.Errorf("seed playwright-cli mcp visibility: %w", err)
	}
	// 桌面控制面（0.11.9，全 agent 共有，见 docs/开发纪律.md 与施工方案 §四-D1）：
	// agent-desktop（vercel-labs，读无障碍树 + 稳定 ref 点击）以 stdio MCP 注册。
	//   - command 取安装期解析的绝对路径（LookPath；未安装则跳过且不落旗标，
	//     装好后下次启动自动补种——种子不复活用户手删的行，见旗标语义）；
	//   - args 固定 ["mcp","--headed"]：物理输入是注册期决策（D6），运行期不可切；
	//   - 可见性给【全部 agent】（用户裁定：桌面控制是平台共有能力，非某功能点专属）。
	// 治理由拦截矩阵承担（intercept_desktop_seed_v1 旗标下的启用名单合并 + 四族规则）；
	// 动作面经「桌面操作」skill 的 mcps 门控延迟加载（skills/desktop-ops）。
	if v, _, _ := d.GetSetting("agent_desktop_mcp_seed_v1"); v != "true" {
		abs, err := exec.LookPath("agent-desktop")
		if err != nil {
			log.Printf("[seed] agent-desktop 未安装（npm i -g agent-desktop），跳过 MCP 种子；装好后下次启动自动补种")
		} else {
			if _, err := d.Exec(`
INSERT INTO mcp_servers(name, transport, command, args, env, enabled)
VALUES ('agent-desktop', 'stdio', $1, $2, '{}', true)
ON CONFLICT (name) DO NOTHING`, abs, `["mcp","--headed"]`); err != nil {
				return fmt.Errorf("seed agent-desktop mcp: %w", err)
			}
			if _, err := d.Exec(`
INSERT INTO agent_visibility(agent_id, resource_kind, resource_id, enabled)
SELECT a.id, 'mcp', m.id, true FROM agents a
JOIN mcp_servers m ON m.name='agent-desktop'
ON CONFLICT DO NOTHING`); err != nil {
				return fmt.Errorf("seed agent-desktop mcp visibility: %w", err)
			}
			_ = d.SetSetting("agent_desktop_mcp_seed_v1", "true")
			log.Printf("[seed] agent-desktop 桌面控制 MCP 已注册（%s），可见性=全部 agent", abs)
		}
	}
	// 桌面工具进拦截系统（治理三段之一，施工方案 §四-D1）：启用名单按【内层名】
	// 逐项合并（延迟调用经 guard 解包后按内层名求值，见 intercept.UnwrapDeferredName）；
	// 四族规则（观测 allow / 动作 ask / 敏感读取 ask / 高危 deny）。旗标一次性：
	// 用户之后删规则/改名单是有意的，重启不复活。
	if v, _, _ := d.GetSetting("intercept_desktop_seed_v1"); v != "true" {
		if err := d.seedDesktopIntercept(); err != nil {
			return fmt.Errorf("seed desktop intercept: %w", err)
		}
		_ = d.SetSetting("intercept_desktop_seed_v1", "true")
	}
	// NOTE: the placeholder ScopeSentry data-source MCP (empty URL + empty X-API-Key,
	// disabled) is seeded directly in schema.sql §F so a raw `psql < schema.sql` init
	// also gets it. schema.sql is Exec'd on every startup, so it stays idempotent.
	if err := d.seedBuiltinSkillVisibility(); err != nil {
		return fmt.Errorf("seed skill visibility: %w", err)
	}
	if err := d.seedDefaultInterceptRules(); err != nil {
		return fmt.Errorf("seed intercept rules: %w", err)
	}
	if err := d.seedDefaultInterceptRulesV2(); err != nil {
		return fmt.Errorf("seed intercept rules v2: %w", err)
	}
	if err := d.seedDefaultInterceptRulesV3(); err != nil {
		return fmt.Errorf("seed intercept rules v3: %w", err)
	}
	if err := d.seedDefaultAssetInterceptRules(); err != nil {
		return fmt.Errorf("seed asset intercept rules: %w", err)
	}
	// 拦截种类表种子（方案 v4.1 §2）：13+2 个内建种类的默认动作/上限——
	// 「只硬拦删除，其余灵活」的出厂档；全部可在 UI 改（锁定行除外）。
	if err := d.SeedSodCategories(); err != nil {
		return fmt.Errorf("seed sod categories: %w", err)
	}
	// 顾问 agent 的工具绑定策略在这里落到 tools 表：老库补绑白名单工具、收回被
	// 硬拒绝的工具、白名单里尚未注册的工具跳过并记录。新库的工具行是服务端
	// 稍后播的（tools 表在 db.Open 之后才被 server 播种），所以新库要等下一次
	// 启动才补齐编排类工具的绑定；内置工具的绑定在播种那一刻就已按策略写入。
	if err := d.seedAdvisoryAgentToolBindings(); err != nil {
		return fmt.Errorf("seed advisory agent tool bindings: %w", err)
	}
	return nil
}

// seedDefaultAssetInterceptRules inserts the built-in asset blocklist (fuzzy
// domain matches for government / education sites) once on first startup. Gated
// by a settings flag so a user's later disable/delete is never resurrected on
// restart — same policy as the intercept-rule seed.
func (d *DB) seedDefaultAssetInterceptRules() error {
	if v, _, _ := d.GetSetting("asset_intercept_default_rules_v1"); v == "done" {
		return nil
	}
	rules := []struct {
		kind    string
		pattern string
		note    string
	}{
		{"fuzzy_domain", ".gov", "[内置] 政府网站 (.gov)"},
		{"fuzzy_domain", ".gov.cn", "[内置] 政府网站 (.gov.cn)"},
		{"fuzzy_domain", ".edu", "[内置] 教育网站 (.edu)"},
		{"fuzzy_domain", ".edu.cn", "[内置] 教育网站 (.edu.cn)"},
	}
	for _, r := range rules {
		if _, err := d.Exec(`
INSERT INTO asset_intercept_rules(enabled, kind, pattern, note, builtin)
VALUES (true, $1, $2, $3, true)
ON CONFLICT DO NOTHING`, r.kind, r.pattern, r.note); err != nil {
			return fmt.Errorf("asset rule %q: %w", r.pattern, err)
		}
	}
	return d.SetSetting("asset_intercept_default_rules_v1", "done")
}

// builtinSkillVisibility maps a shipped skill's directory name → the built-in
// agent keys that should see it by default. The skill FILES themselves live on the
// filesystem (SkillDir, loaded by norma at runtime); DB only carries this visibility
// binding. Skills omitted here (e.g. playwright-cli, scopesentry) ship invisible by
// default — the user turns them on per-agent when needed. scopesentry additionally
// declares `mcps: ScopeSentry`, which only takes effect once it's made visible and
// that MCP is enabled/configured.
var builtinSkillVisibility = map[string][]string{
	"api-recon": {"auto", "pentest", "worker"},
	// 桌面操作（0.11.9）：mcps 门控的动作面解锁入口——全 agent 共有（用户裁定：
	// 桌面控制是平台能力，非某功能点专属）。"*" = 绑全部 agent（含自定义 agent）。
	"desktop-ops": {"*"},
	// 小程序渗透（0.12.0）：阶段执行在任务路径（worker）与会话驱动（pentest/auto/
	// mainagent）两条线；token 池工具同绑定。
	"wechat-miniprogram": {"auto", "pentest", "worker", "mainagent"},
}

// seedBuiltinSkillVisibility binds the shipped built-in skills to their default
// agents. Insert-if-absent (ON CONFLICT DO NOTHING) so a user's later toggle-off is
// never resurrected on restart — matches the browser-MCP / intercept-rule seed policy.
func (d *DB) seedBuiltinSkillVisibility() error {
	for skillName, agentKeys := range builtinSkillVisibility {
		for _, key := range agentKeys {
			// "*" 通配 = 全部 agent（桌面操作的全 agent 共有口径；roledispatch
			// 钉死的纪律 agent 由白名单继续兜——可见不等于可动手）。
			if key == "*" {
				if _, err := d.Exec(`
INSERT INTO agent_skill_visibility(agent_id, skill_name, enabled)
SELECT id, $1, true FROM agents
ON CONFLICT (agent_id, skill_name) DO NOTHING`, skillName); err != nil {
					return fmt.Errorf("skill %s → 全部 agent: %w", skillName, err)
				}
				continue
			}
			if _, err := d.Exec(`
INSERT INTO agent_skill_visibility(agent_id, skill_name, enabled)
SELECT id, $2, true FROM agents WHERE key=$1
ON CONFLICT (agent_id, skill_name) DO NOTHING`, key, skillName); err != nil {
				return fmt.Errorf("skill %s → agent %s: %w", skillName, key, err)
			}
		}
	}
	return nil
}

// seedDefaultInterceptRules inserts built-in safety intercept rules once on
// first startup. The seed is gated by a settings flag so user edits (disable,
// delete, re-order) are never overwritten on subsequent restarts.
func (d *DB) seedDefaultInterceptRules() error {
	if v, _, _ := d.GetSetting("intercept_default_rules_v1"); v == "done" {
		return nil
	}
	type rule struct {
		name     string
		target   string // tool_name | tool_input
		typ      string // string | regex
		pattern  string
		action   string
		message  string
		priority int
	}
	rules := []rule{
		// ── 系统破坏性命令 (priority 100) ──────────────────────────────────
		{
			name:     "[内置] 递归强制删除 rm -rf",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\brm\b.{0,80}(?:-[a-z]*r[a-z]*f[a-z]*|-[a-z]*f[a-z]*r[a-z]*|--recursive|--no-preserve-root)`,
			action:   "deny",
			message:  "禁止执行递归强制删除（rm -rf / rm --recursive），可能永久损坏系统或靶机环境",
			priority: 100,
		},
		{
			name:     "[内置] 删除系统关键目录",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\brm\b[^"'\n]{0,60}["'\s](/|/etc|/bin|/usr|/boot|/var|/lib|/sys|/proc|/dev|/sbin|/root)`,
			action:   "deny",
			message:  "禁止删除系统关键路径",
			priority: 100,
		},
		{
			name:     "[内置] 磁盘格式化 mkfs",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\bmkfs\b`,
			action:   "deny",
			message:  "禁止格式化磁盘（mkfs）",
			priority: 100,
		},
		{
			name:     "[内置] 覆写磁盘设备 dd",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\bdd\b[^|\n]{0,100}\bof=\s*/dev/[a-zA-Z]`,
			action:   "deny",
			message:  "禁止使用 dd 覆写磁盘设备",
			priority: 100,
		},
		{
			name:     "[内置] Fork 炸弹",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `:\(\)\s*\{[^}]*:\|:`,
			action:   "deny",
			message:  "禁止执行 Fork 炸弹",
			priority: 100,
		},
		{
			name:     "[内置] 关机 / 重启",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\b(?:shutdown|reboot|halt|poweroff|init\s+[06])\b`,
			action:   "deny",
			message:  "禁止执行关机或重启命令",
			priority: 100,
		},
		{
			name:     "[内置] 杀死全部进程",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\bkill\s+-9\s+-1\b|\bkillall\s+-9\b`,
			action:   "deny",
			message:  "禁止 kill -9 -1 或 killall -9（杀死所有进程）",
			priority: 100,
		},
		{
			name:     "[内置] 磁盘擦除 shred / wipe",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\b(?:shred|wipe)\b[^|\n]{0,80}/dev/[a-zA-Z]`,
			action:   "deny",
			message:  "禁止对磁盘设备执行 shred/wipe 擦除",
			priority: 100,
		},
		{
			name:     "[内置] 清空防火墙规则",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\biptables\s+(?:-F|--flush)\b|\bnft\s+flush\s+ruleset\b`,
			action:   "deny",
			message:  "禁止清空防火墙规则（iptables -F / nft flush）",
			priority: 100,
		},
		// ── 数据库破坏性操作 (priority 90) ─────────────────────────────────
		{
			name:     "[内置] SQL DROP DATABASE / TABLE / SCHEMA",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\bDROP\s+(?:DATABASE|TABLE|SCHEMA|INDEX|VIEW|TABLESPACE|USER|ROLE)\b`,
			action:   "deny",
			message:  "禁止执行 DROP 操作，可能不可逆地销毁数据库对象",
			priority: 90,
		},
		{
			name:     "[内置] SQL TRUNCATE",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\bTRUNCATE\s+(?:TABLE\s+)?\w`,
			action:   "deny",
			message:  "禁止执行 TRUNCATE，可能清空数据表所有数据",
			priority: 90,
		},
		{
			name:     "[内置] MongoDB drop / dropDatabase",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\.(?:dropDatabase|dropCollection|drop)\s*\(`,
			action:   "deny",
			message:  "禁止执行 MongoDB drop 操作",
			priority: 90,
		},
		{
			name:     "[内置] Redis FLUSHALL / FLUSHDB",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\b(?:FLUSHALL|FLUSHDB)\b`,
			action:   "deny",
			message:  "禁止执行 Redis FLUSHALL / FLUSHDB，可能清空全部缓存数据",
			priority: 90,
		},
		// ── HTTP 破坏性请求 (priority 80) ──────────────────────────────────
		// Agent 发送 DELETE 请求的三种常见方式：
		//   1. curl -X DELETE / --request DELETE（Bash 工具直接执行或写入脚本）
		//   2. Python HTTP 客户端 .delete() 方法
		//   3. JS/通用脚本里的 method: 'DELETE' / method="DELETE"
		{
			name:     "[内置] curl / wget 发送 DELETE 请求",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\bcurl\b[^|\n&;"]{0,300}(?:-X\s*DELETE|--request\s+DELETE|-XDELETE)|\bwget\b[^|\n&;"]{0,300}--method[=\s]+DELETE`,
			action:   "deny",
			message:  "禁止通过 curl/wget 发送 HTTP DELETE 请求，可能删除目标系统数据",
			priority: 80,
		},
		{
			name:     "[内置] Python HTTP 客户端 DELETE（requests/httpx/aiohttp）",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\b(?:requests|httpx|aiohttp|urllib\.request)\.delete\s*\(|session\.delete\s*\(|client\.delete\s*\(`,
			action:   "deny",
			message:  "禁止使用 Python HTTP 客户端发送 DELETE 请求",
			priority: 80,
		},
		{
			name:     "[内置] 脚本中声明 HTTP DELETE 方法（JS/通用）",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)axios\.delete\s*\(|method\s*[:=]\s*['"]DELETE['"]`,
			action:   "deny",
			message:  "禁止在脚本中声明并发送 HTTP DELETE 请求",
			priority: 80,
		},
		{
			name:     "[内置] 批量清空 / 清除接口路径",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)/(?:clear|wipe|flush|purge|truncate|drop|destroy|factory[-_]reset|reset[-_]all)(?:[/?#"'\s]|$)`,
			action:   "deny",
			message:  "禁止调用批量清空或销毁类接口（/clear /wipe /flush /purge 等）",
			priority: 80,
		},
	}
	for _, r := range rules {
		if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action)
VALUES ($1, true, $2, $3, $4, $5, $6, $7, false, 60, 'deny')
ON CONFLICT DO NOTHING`,
			r.name, r.priority, r.target, r.typ, r.pattern, r.action, r.message,
		); err != nil {
			return fmt.Errorf("rule %q: %w", r.name, err)
		}
	}
	return d.SetSetting("intercept_default_rules_v1", "done")
}

// seedDefaultInterceptRulesV2 migrates the two safety patterns that used to be
// hard-coded in guard.go (destructive shell + data-exfil pipe) into ordinary
// intercept rules. Gated by its own flag so it also lands on DBs that already ran
// v1. Unlike the old guard.go floor, these are plain [内置] rules — the user can
// disable or delete them. The exfil rule ships DISABLED by default (its
// curl/wget/nc pipe pattern mis-fires on legitimate CTF/pentest reverse-shell and
// data-transfer pipes); enable it manually when exfil gating is actually wanted.
func (d *DB) seedDefaultInterceptRulesV2() error {
	if v, _, _ := d.GetSetting("intercept_default_rules_v2"); v == "done" {
		return nil
	}
	rules := []struct {
		name     string
		pattern  string
		action   string
		message  string
		enabled  bool
		priority int
	}{
		{
			name:     "[内置] 破坏性系统命令",
			pattern:  `(?i)\b(rm\s+-rf\s+/|mkfs|dd\s+if=|:\(\)\s*\{|shutdown|reboot|>\s*/dev/sd)`,
			action:   "deny",
			message:  "破坏性命令被拒绝（rm -rf / / mkfs / dd / fork bomb / 关机重启 / 覆写磁盘设备）",
			enabled:  true,
			priority: 100,
		},
		{
			name:     "[内置] 数据外泄管道",
			pattern:  `(?i)(curl|wget|nc|ncat)\b[^|]*\b(\|\s*(curl|wget|nc))`,
			action:   "deny",
			message:  "疑似数据外泄管道被拒绝（命令输出经 curl/wget/nc 外传）",
			enabled:  false,
			priority: 80,
		},
	}
	for _, r := range rules {
		if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action)
VALUES ($1, $2, $3, 'tool_input', 'regex', $4, $5, $6, false, 60, 'deny')
ON CONFLICT DO NOTHING`,
			r.name, r.enabled, r.priority, r.pattern, r.action, r.message,
		); err != nil {
			return fmt.Errorf("rule %q: %w", r.name, err)
		}
	}
	return d.SetSetting("intercept_default_rules_v2", "done")
}

// seedDefaultInterceptRulesV3 adds the delete-endpoint path rule. The v1 HTTP rules
// only catch the DELETE *method* (curl -X DELETE, requests.delete(, method:'DELETE'),
// and v1's path rule covers only /clear /wipe /flush /purge /truncate /drop /destroy
// /factory-reset /reset-all — so a plain `curl 'http://t/api/user/delete?id=1'` (a
// delete endpoint reached with GET/POST, which is how most web apps expose deletion)
// slipped through every built-in rule. Own flag so it also lands on DBs that already
// ran v1/v2, where editing the v1 seed would have no effect.
//
// The pattern deliberately requires a separator after the verb so /delivery,
// /details, /delta and /delegate do not match, while /deleteAll, /delete_user and
// /delete-user do. destroy is re-covered here because v1's rule does not allow a
// suffix (/destroyAll was missed).
//
// Exported as a package const only so the seeded regex is unit-testable without a DB.
const deleteEndpointPathPattern = `(?i)/(?:(?:delete|remove|unlink|erase|destroy)[-\w]*|del)(?:[/?#"'\s]|$)`

func (d *DB) seedDefaultInterceptRulesV3() error {
	if v, _, _ := d.GetSetting("intercept_default_rules_v3"); v == "done" {
		return nil
	}
	const name = "[内置] 删除类接口路径"
	if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action)
SELECT $1, true, 80, 'tool_input', 'regex', $2, 'deny', $3, false, 60, 'deny'
WHERE NOT EXISTS (SELECT 1 FROM intercept_rules WHERE name = $1)`,
		name,
		deleteEndpointPathPattern,
		"禁止调用删除类接口（/delete /remove /unlink /erase 等），不论使用哪种 HTTP 方法——多数应用的删除接口用 GET/POST 就能触发，同样会真实删除目标数据",
	); err != nil {
		return fmt.Errorf("rule %q: %w", name, err)
	}
	return d.SetSetting("intercept_default_rules_v3", "done")
}
