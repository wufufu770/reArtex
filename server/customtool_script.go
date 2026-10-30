package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/internal/agentcred"
	actool "github.com/Autumn-27/norma/tool"
)

// agentDropUnavailableOnce 让「降不了权」的提醒只打一次。
var agentDropUnavailableOnce sync.Once

// 本文件是自定义工具 script 类型（仅 Python）的执行器：临时文件 + stdin JSON
// + env 组装 + 子进程身份策略。放在独立文件里是因为这一块有两件互相独立的
// 事要做 —— 「把脚本跑起来」与「以什么身份、什么环境跑」——后者是凭据隔离的
// 落点，改动时必须一眼看得见。

// ---------- script(仅 Python):临时文件 + stdin JSON + env ----------

// toolPrivilege 描述工具子进程的身份策略。
type toolPrivilege int

const (
	// privilegePlatform：子进程继承平台进程身份。用于平台自身的操作
	// （自定义工具编辑器的「试运行」），那不是 agent 调用。
	privilegePlatform toolPrivilege = iota
	// privilegeAgent：子进程降权到 agentcred.AgentUID。agent 调用路径一律用它。
	privilegeAgent
)

// scriptToolCall 描述一次 script 类型自定义工具的调用。
type scriptToolCall struct {
	Key       string
	Exec      json.RawMessage
	Params    map[string]any
	TC        *actool.ToolContext
	Privilege toolPrivilege
}

func (s *Server) runScriptTool(ctx context.Context, call scriptToolCall) (actool.Result, error) {
	var spec scriptExec
	_ = json.Unmarshal(call.Exec, &spec)
	if strings.TrimSpace(spec.Code) == "" {
		return actool.Errorf("script code 为空"), nil
	}
	interp := s.pythonInterpreter()
	if interp == "" {
		return actool.Errorf("未配置且未检测到 python 解释器(在系统配置里设置)"), nil
	}
	workDir := s.m.dir
	var sessionEnv []string
	if call.TC != nil {
		if call.TC.WorkingDir != "" {
			workDir = call.TC.WorkingDir
		}
		sessionEnv = call.TC.Env
	}
	body, err := execPython(ctx, pythonRun{
		Interp:    interp,
		Key:       call.Key,
		Code:      spec.Code,
		Params:    call.Params,
		WorkDir:   workDir,
		Env:       sessionEnv,
		Timeout:   timeoutOr(spec.TimeoutMs, 120000),
		Privilege: call.Privilege,
	})
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(actool.Capture(call.TC, body)), nil
}

// pythonRun 描述一次 script 工具子进程执行：跑什么、在哪跑、什么身份、什么环境。
type pythonRun struct {
	Interp    string         // 解释器绝对路径
	Key       string         // 临时文件名前缀
	Code      string         // 脚本内容
	Params    map[string]any // 参数（stdin JSON + TOOL_* 镜像）
	WorkDir   string         // 工作目录（子进程的 cwd）
	Env       []string       // 会话 env（代理/CA 等）
	Timeout   time.Duration  // 超时
	Privilege toolPrivilege  // 子进程身份策略
}

// execPython writes the code to a temp .py under workDir/.tools, runs it via interp
// with the params JSON on stdin + scalar params mirrored to TOOL_<NAME> env, and
// returns combined stdout+stderr (with a timeout/exit note). Standalone + testable.
//
// 身份：privilegeAgent 时子进程降权到 agentcred.AgentUID —— 容器里平台进程是
// root，工具子进程继承同样的身份就能读走平台的库凭据文件
// （/run/secrets/pg_dsn，0600 但对 root 全开），拿到连接串就能绕过全部应用层
// scope 校验直连数据库。降权后这条读路径断了；平台自己照旧连库。
func execPython(ctx context.Context, run pythonRun) (string, error) {
	toolsDir := filepath.Join(run.WorkDir, ".tools")
	if err := os.MkdirAll(toolsDir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(toolsDir, run.Key+"-*.py")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.WriteString(run.Code); err != nil {
		f.Close()
		return "", err
	}
	f.Close()

	// 降权后子进程读不到平台(root)创建的 0600 临时文件 —— 目录与文件都要交给
	// agent 身份。目录同时让子进程能往里写自己的中间产物。
	drop := run.Privilege == privilegeAgent
	if drop && !agentcred.CanDrop() {
		// 平台进程不是 root，降不了权 —— 这条隔离在当前部署里不生效，必须说出口，
		// 不能让人以为凭据已经隔离掉了。只提醒一次，免得刷屏。
		agentDropUnavailableOnce.Do(func() {
			log.Printf("[security] agent 工具子进程未降权：平台进程不是 root，子进程仍继承平台身份")
		})
		drop = false
	}
	if drop {
		if err := agentcred.ChownToAgent(toolsDir); err != nil {
			return "", err
		}
		if err := agentcred.ChownToAgent(tmp); err != nil {
			return "", err
		}
	}

	runCtx, cancel := context.WithTimeout(ctx, run.Timeout)
	defer cancel()
	c := exec.CommandContext(runCtx, run.Interp, tmp)
	c.Dir = run.WorkDir
	c.Env = toolChildEnv(run.Env, run.Params)
	if drop {
		c.SysProcAttr = agentcred.ProcAttr()
		// HOME 指向工作目录：平台进程的 HOME（容器里是 /root）属主是 root，
		// 降权后的子进程写不进去，工具的缓存目录会一律报错。
		c.Env = append(c.Env, "HOME="+run.WorkDir)
	}
	pj, _ := json.Marshal(run.Params)
	c.Stdin = bytes.NewReader(pj) // 参数 JSON 走 stdin
	out, err := c.CombinedOutput()
	body := string(out)
	if runCtx.Err() == context.DeadlineExceeded {
		body += "\n... [超时终止] ..."
	} else if err != nil {
		body += "\n[exit: " + err.Error() + "]"
	}
	return body, nil
}

// privilegedDSNEnvKey 是平台自己的库连接串在环境里的键名。
//
// 它绝不能出现在工具子进程的环境里：子进程环境会被工具打印出来
// （printenv / os.environ），打印出来就是凭据外泄。agent 侧的只读凭据用另一个
// 键名，不受这条约束。
const privilegedDSNEnvKey = "ARTEX_PG_DSN"

// toolChildEnv 组装工具子进程的环境：宿主白名单 + 会话 env + 参数镜像，
// 并剔除平台的特权凭据变量。
//
// 剔除这一步是最后一道闸：白名单已经挡住了宿主环境里的绝大多数变量，但会话
// env（平台注入的代理/CA 等）不在白名单管辖内。
func toolChildEnv(sessionEnv []string, params map[string]any) []string {
	env := append(toolScriptBaseEnv(), sessionEnv...) // 会话代理 env
	for k, v := range params {                        // 标量参数镜像成 TOOL_<NAME>
		if sv, ok := scalarStr(v); ok {
			env = append(env, "TOOL_"+strings.ToUpper(k)+"="+sv)
		}
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k == privilegedDSNEnvKey {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// toolScriptEnvAllowlist 是自定义脚本工具允许从宿主进程继承的环境变量。
//
// 用白名单而非黑名单：进程环境里可能存在本次未预料到的凭据（新增的 *_TOKEN、
// CI 注入的变量、第三方 SDK 塞进来的），黑名单永远追不上新变量。
var toolScriptEnvAllowlist = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true,
	"LANG": true, "LC_ALL": true, "LC_CTYPE": true, "LANGUAGE": true, "TZ": true,
	"TERM": true, "TMPDIR": true, "PWD": true, "HOSTNAME": true, "SHLVL": true,
	"PYTHONPATH": true, "PYTHONHOME": true, "PYTHONUNBUFFERED": true,
	"VIRTUAL_ENV": true, "PYTHONHASHSEED": true,
	// 代理与 CA：会话 env 会覆盖同名项，这里给出缺省值。
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "ALL_PROXY": true,
	"http_proxy": true, "https_proxy": true, "no_proxy": true, "all_proxy": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "NODE_EXTRA_CA_CERTS": true,
	"REQUESTS_CA_BUNDLE": true, "CURL_CA_BUNDLE": true,
}

// toolScriptBaseEnv 返回白名单内的宿主环境变量。
func toolScriptBaseEnv() []string {
	out := make([]string, 0, len(toolScriptEnvAllowlist))
	for _, kv := range os.Environ() {
		k, _, ok := strings.Cut(kv, "=")
		if ok && toolScriptEnvAllowlist[k] {
			out = append(out, kv)
		}
	}
	return out
}
