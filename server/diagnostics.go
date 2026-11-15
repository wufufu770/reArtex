package server

// 启动诊断清单（0.14.6 E3）：把散落在各启动路径的「降级/未配置/未安装」状态
// 聚合成一个只读面——运维一眼看到"哪些功能在降级、为什么"，不用再翻日志。
// 全部实时求值（无状态缓存）：每一项都是廉价检查（env/LookPath/一条 count SQL）。

import (
	"net/http"
	"os"
	"os/exec"

	"github.com/Autumn-27/artex/config"
)

// diagItem 是一条诊断项：ok=功能完整可用；warn=降级（附原因）。
type diagItem struct {
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Level string `json:"level"` // ok | warn
	Msg   string `json:"msg"`
}

func (s *Server) diagnosticsHandler(w http.ResponseWriter, _ *http.Request) {
	items := make([]diagItem, 0, 8)
	add := func(id string, ok bool, msg string) {
		lvl := "ok"
		if !ok {
			lvl = "warn"
		}
		items = append(items, diagItem{ID: id, OK: ok, Level: lvl, Msg: msg})
	}

	// ① 加密主密钥：llm_profiles 与小程序 token 池的凭据加密跟随它。
	if key, err := config.MasterKey(); err == nil && len(key) > 0 {
		add("master_key", true, "已配置——凭据 AES-256-GCM 加密落库")
	} else {
		enforced := false
		if v, _, _ := s.m.pg.GetSetting("require_master_key"); v == "true" {
			enforced = true
		}
		msg := "未配置——凭据明文落过渡列（token_plain / api_key）"
		if enforced {
			msg += "；凭据强制加密模式已开启，明文凭据写入被拒绝"
		}
		add("master_key", false, msg)
	}

	// ② agent-desktop MCP（桌面控制）。
	if _, err := exec.LookPath("agent-desktop"); err != nil {
		add("agent_desktop", false, "未安装（LookPath 失败）——桌面控制面不可用；安装后下次启动自动补种")
	} else {
		add("agent_desktop", true, "已安装——桌面控制 MCP 可用")
	}

	// ③ chrome-devtools MCP（接口发现阶段的浏览器操控）。
	if _, err := exec.LookPath("chrome-devtools-mcp"); err != nil {
		add("chrome_devtools", false, "未安装——浏览器操控入口不可用；安装后下次启动自动补种")
	} else {
		add("chrome_devtools", true, "已安装")
	}

	// ④ wxa-runner（小程序运行时，Node 生态）。
	if _, err := exec.LookPath("wxa-runner"); err != nil {
		if _, nerr := exec.LookPath("node"); nerr != nil {
			add("wxa_runner", false, "node 与 wxa-runner 均未安装——小程序运行时链路不可用（Node≥18）")
		} else {
			add("wxa_runner", false, "node 在但 wxa-runner 未安装——运行时链路不可用")
		}
	} else {
		add("wxa_runner", true, "已安装")
	}

	// ⑤ 小程序运行时地址（内嵌浏览器面板的数据面）。
	if raw, ok, _ := s.m.pg.GetSetting("miniprogram_runtime_url"); ok && raw != "" {
		add("mp_runtime_url", true, "已配置：" + raw)
	} else {
		add("mp_runtime_url", false, "未配置——内嵌浏览器面板不可用（小程序页设置）")
	}

	// ⑥ 解包后端（B 线探针口径：unpack 脚本的后端可换，探针实测可用性）。
	if _, err := exec.LookPath("python3"); err != nil {
		add("unpack_backend", false, "python3 未安装——解包适配层脚本不可用")
	} else {
		add("unpack_backend", true, "python3 在；解包后端可用性以探针实测为准（skills/wechat-miniprogram probe_unpackers）")
	}

	// ⑦ ScopeSentry 数据源（资产同步）。
	if m, err := s.findMCPByName(scopeSentryMCPName); err != nil || m == nil {
		add("scopesentry", false, "MCP 数据源未创建——资产同步不可用")
	} else if m.URL == "" {
		add("scopesentry", false, "数据源存在但未配置 URL——资产同步不可用")
	} else {
		add("scopesentry", true, "已配置")
	}

	// ⑧ ARTEX_MASTER_KEY_FILE 环境变量指向检查（配置了但读不到=配置错误，比
	// 「未配置」更严重）。
	if p := os.Getenv("ARTEX_MASTER_KEY_FILE"); p != "" {
		if _, err := config.MasterKey(); err != nil {
			add("master_key_file", false, "ARTEX_MASTER_KEY_FILE 已设置但读取失败："+err.Error())
		}
	}

	writeJSON(w, 200, map[string]any{"items": items, "warn_count": countWarn(items)})
}

func countWarn(items []diagItem) int {
	n := 0
	for _, it := range items {
		if it.Level == "warn" {
			n++
		}
	}
	return n
}
