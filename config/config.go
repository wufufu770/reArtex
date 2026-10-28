// Package config loads runtime configuration from a JSON file, with environment
// variables taking precedence. Currently it carries the PostgreSQL connection.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Database is the PostgreSQL connection config. Either set DSN directly, or set
// the component fields and a DSN is assembled from them.
type Database struct {
	DSN      string `json:"dsn"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	DBName   string `json:"dbname"`
	SSLMode  string `json:"sslmode"`
}

// Config is the on-disk config file shape.
type Config struct {
	Database Database `json:"database"`
	SkillDir string   `json:"skill_dir"`
}

// BaseDir is the directory that anchors all runtime artifacts (config.json and
// the data/ store). It is the directory the running binary lives in, so a
// distributed executable keeps its files next to itself on any OS (Windows,
// Linux, …) regardless of the working directory it is launched from.
//
// When launched via `go run`, the binary is throwaway: it sits either in a temp
// build dir (cache miss → fresh link) OR straight inside the Go build cache
// (cache hit → run from $GOCACHE/.../...-d). We detect both and fall back to the
// current working directory so dev artifacts (data/, transcripts) and config
// resolve against the project dir, not the throwaway binary's location.
func BaseDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	dir := filepath.Dir(exe)
	if isGoRunDir(dir) {
		return "." // throwaway `go run` binary → use CWD
	}
	return dir
}

// isGoRunDir reports whether dir is where `go run` parked its executable: under
// the system temp dir (cache miss), or anywhere inside a Go build cache
// (…/go-build/…, the cache-hit case — NOT under os.TempDir(), which is why the
// old temp-only check failed intermittently). In both cases the binary is
// throwaway, so config/data must resolve against the CWD.
func isGoRunDir(dir string) bool {
	if tmp := os.TempDir(); tmp != "" {
		if rel, err := filepath.Rel(tmp, dir); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	for _, seg := range strings.Split(filepath.ToSlash(dir), "/") {
		if seg == "go-build" {
			return true
		}
	}
	return false
}

// Path returns the config file path. Resolution order:
//  1. env ARTEX_CONFIG (explicit override)
//  2. ./config.json in the current working directory (running from the project
//     dir — robust no matter where `go run` placed the temp/cached binary)
//  3. config.json next to the executable (a distributed binary keeps it beside)
//
// The first existing file wins. If none exist, the CWD path is returned so the
// "not found" message points at the project dir the user most likely expected.
func Path() string {
	if v := strings.TrimSpace(os.Getenv("ARTEX_CONFIG")); v != "" {
		return v
	}
	var candidates []string
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, "config.json"))
	}
	candidates = append(candidates, filepath.Join(BaseDir(), "config.json"))
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return candidates[0]
}

// Load reads and parses the config file. A missing/unreadable file yields a zero
// Config (so callers fall back to defaults) rather than an error.
func Load() Config {
	var c Config
	b, err := os.ReadFile(Path())
	if err != nil {
		return c
	}
	_ = json.Unmarshal(b, &c)
	return c
}

// SkillDir returns the skill root directory with precedence:
//
//	env ARTEX_SKILL_DIR  >  config file (skill_dir)  >  BaseDir()/skills
//
// The directory is created if it does not exist.
func SkillDir() string {
	var d string
	if v := strings.TrimSpace(os.Getenv("ARTEX_SKILL_DIR")); v != "" {
		d = v
	} else if v := strings.TrimSpace(Load().SkillDir); v != "" {
		d = v
	} else {
		d = filepath.Join(BaseDir(), "skills")
	}
	_ = os.MkdirAll(d, 0o755)
	return d
}

// PostgresDSN resolves the connection string with precedence:
//
//	env ARTEX_PG_DSN  >  file ARTEX_PG_DSN_FILE  >  config file (database.dsn, or assembled from fields)
//
// There is NO built-in fallback: when no source supplies a database config,
// it returns an error naming the config path it inspected, so startup fails loudly
// instead of silently connecting to a wrong default. source describes where the
// DSN came from (for startup logging).
func PostgresDSN() (dsn, source string, err error) {
	if v := strings.TrimSpace(os.Getenv("ARTEX_PG_DSN")); v != "" {
		return v, "环境变量 ARTEX_PG_DSN", nil
	}
	if p := strings.TrimSpace(os.Getenv("ARTEX_PG_DSN_FILE")); p != "" {
		d, err := readAndBurnDSNFile(p)
		if err != nil {
			return "", "", fmt.Errorf("读取 ARTEX_PG_DSN_FILE %q 失败: %w", p, err)
		}
		return d, "文件 " + p + " (读入后已删除)", nil
	}
	db := Load().Database
	if d := strings.TrimSpace(db.DSN); d != "" {
		return d, "配置文件 " + Path() + " (database.dsn)", nil
	}
	if db.Host != "" || db.DBName != "" || db.User != "" {
		return db.buildDSN(), "配置文件 " + Path() + " (database 字段)", nil
	}
	return "", "", fmt.Errorf("未找到数据库配置：环境变量 ARTEX_PG_DSN 未设置，且配置文件 %s 未提供 database（dsn 或 host/user/dbname）。请创建该配置文件或设置环境变量后重试", Path())
}

// readOnlySecretDir 是只读 secret 的目录前缀。docker secret 以只读方式挂到
// 这里：既不能清零也不能删。它不在 agent 的工作区（/app/data）内，agent 够不着，
// 故可容忍「不烧毁」。
//
// 做成包级变量是为了让测试能临时指向临时目录（手法与 pathguard 的
// withSecretRoots 一致），生产代码不留测试专用钩子。
var readOnlySecretDir = "/run/secrets"

// readOnlySecret 判断路径是否落在只读 secret 目录内。
func readOnlySecret(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return abs == readOnlySecretDir || strings.HasPrefix(abs, readOnlySecretDir+string(filepath.Separator))
}

// readAndBurnDSNFile 读连接串后立刻把磁盘上的那份抹掉。
//
// 只读不删等于把秘密留在盘上等 agent 来捡：agent 的 Bash 与 artex 同 UID，
// 且 /app/data 是它的工作区根。内容已在内存里，磁盘上的副本没有存在价值。
//
// 例外：只读 secret（/run/secrets 下的 docker secret）无法清零也无法删除。
// 那条路径不在 agent 工作区内（且已被 pathguard 的秘密清单拒绝），故容忍
// 「不烧毁」而不是让启动失败 —— 否则 compose 用 secret 供给 DSN 的部署起不来。
//
// 顺序是先清零再删：删除失败时至少内容不在了。
func readAndBurnDSNFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if readOnlySecret(path) {
		dsn := strings.TrimSpace(string(b))
		if dsn == "" {
			return "", errors.New("DSN 文件为空")
		}
		return dsn, nil
	}
	// 顺序是先清零再删：删除失败时至少内容不在了。
	// 清零是否真的成功要如实记录 —— 只读挂载上打不开写句柄，内容其实还在，
	// 报错却说「已清零」会把人带偏（以为盘上已经没有副本了）。
	zeroed := false
	if f, err := os.OpenFile(path, os.O_WRONLY, 0); err == nil {
		if err := f.Truncate(0); err == nil {
			zeroed = true
		}
		_ = f.Close()
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		if zeroed {
			return "", fmt.Errorf("删除 DSN 文件失败（内容已清零）: %w", err)
		}
		return "", fmt.Errorf("删除 DSN 文件失败（内容未能清零，文件里仍有明文）: %w", err)
	}
	dsn := strings.TrimSpace(string(b))
	if dsn == "" {
		return "", errors.New("DSN 文件为空")
	}
	return dsn, nil
}

func (d Database) buildDSN() string {
	host := d.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := d.Port
	if port == 0 {
		port = 5432
	}
	ssl := d.SSLMode
	if ssl == "" {
		ssl = "disable"
	}
	u := url.URL{
		Scheme: "postgres",
		Host:   host + ":" + strconv.Itoa(port),
		Path:   "/" + d.DBName,
	}
	if d.User != "" {
		if d.Password != "" {
			u.User = url.UserPassword(d.User, d.Password)
		} else {
			u.User = url.User(d.User)
		}
	}
	u.RawQuery = url.Values{"sslmode": {ssl}}.Encode()
	return u.String()
}

// String is a redacted view of the resolved DSN (password masked) for logging.
func Redact(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	if u.User != nil {
		if _, hasPw := u.User.Password(); hasPw {
			u.User = url.UserPassword(u.User.Username(), "****")
		}
	}
	return fmt.Sprintf("%s", u.String())
}

// secretEnvKeys 是平台自身凭据在环境块里的键名。
var secretEnvKeys = []string{"ARTEX_PG_DSN", "ANTHROPIC_API_KEY", "OPENAI_API_KEY"}

// SecretEnvReport 是启动自检的结论。
type SecretEnvReport struct {
	Leaks  []string // 仍可从 /proc/self/environ 读到的平台秘密键名
	Fatal  bool     // 是否应拒绝启动
	Reason string   // 人类可读的结论（无泄露时为空）
}

// leakSecretEnvKeys 从一段环境块字节里挑出仍带值的平台秘密键名。
//
// 空值不算泄露：键在但没值，agent 读走也没有秘密。
func leakSecretEnvKeys(block []byte) []string {
	want := map[string]bool{}
	for _, k := range secretEnvKeys {
		want[k] = true
	}
	var hits []string
	for _, kv := range strings.Split(string(block), "\x00") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !want[k] || strings.TrimSpace(v) == "" {
			continue
		}
		hits = append(hits, k)
	}
	return hits
}

// SecretEnvLeak 返回仍能从 /proc/self/environ 读到的平台秘密键名。
//
// 平台自己的 exec 期环境块是 agent 子进程够得着的（同 UID）：os.Unsetenv 只改
// Go 进程内的副本，改不动内核那块。所以凭据一旦进了容器环境，agent 一条
// `cat /proc/*/environ` 就能读走。这里读的就是那块，用来在启动时把问题喊出来。
//
// 读不到（非 Linux / 权限不足）时返回 nil：无法判定就不谎报。
func SecretEnvLeak() []string {
	b, err := os.ReadFile("/proc/self/environ")
	if err != nil {
		return nil
	}
	return leakSecretEnvKeys(b)
}

// secretEnvPolicy 决定发现残留凭据时的动作：致命，还是仅告警。
//
// requireEnvScrub 来自 ARTEX_REQUIRE_ENV_SCRUB：只有显式设为 "1" 才致命。
// 默认只告警 —— 官方镜像等既有部署可能合法地用 env 传参，硬失败会让它们起不来。
func secretEnvPolicy(leaks []string, requireEnvScrub string) (fatal bool, reason string) {
	if len(leaks) == 0 {
		return false, ""
	}
	reason = fmt.Sprintf("平台凭据仍可经 /proc/self/environ 读取（%v）："+
		"同 UID 的 agent 子进程可用 `cat /proc/*/environ` 读走。"+
		"请改用 ARTEX_PG_DSN_FILE，或把密钥移出容器环境。", leaks)
	return requireEnvScrub == "1", reason
}

// CheckSecretEnv 读进程自身的环境块，判断平台凭据是否仍可被 agent 子进程读到。
func CheckSecretEnv() SecretEnvReport {
	leaks := SecretEnvLeak()
	fatal, reason := secretEnvPolicy(leaks, os.Getenv("ARTEX_REQUIRE_ENV_SCRUB"))
	return SecretEnvReport{Leaks: leaks, Fatal: fatal, Reason: reason}
}
