package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/Autumn-27/artex/config"
)

// ---------- LLM profiles ----------

type LLMProfile struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Format     string `json:"format"`
	BaseURL    string `json:"base_url,omitempty"`
	Proxy      string `json:"proxy,omitempty"` // LLM 出站代理(http/https/socks5);空=用环境变量
	Model      string `json:"model"`
	// Models 是该 profile 可用的模型列表（E1 多模型）。空/缺省视为 [Model]；
	// Model 恒为「默认模型」——列表的第一个语义槽位。引用方可用列表里的任何
	// 一个模型名构造变体（同 key/网关/限流，只换 model 参数）。
	Models []string `json:"models,omitempty"`
	// BaseURLs 是网关候选地址（方案 v5 G1）：有序（第一项为主）。空 = 只用
	// BaseURL 单地址。provider 在连接类失败后自动切下一候选，主地址恢复回切。
	BaseURLs []string `json:"base_urls,omitempty"`
	APIKey     string `json:"-"` // never serialized to UI
	APIKeyHint string `json:"api_key_hint,omitempty"`
	// APIKeyCipher 是加密落库的 key（AAD 绑定 Name）。不序列化。
	APIKeyCipher  string  `json:"-"`
	RatePerSecond float64 `json:"rate_per_second"`
	RatePerMinute float64 `json:"rate_per_minute"`
	// ContextWindowK is the model's context window in K tokens, used to size
	// compaction thresholds. 0 = use a 200K default; capped at 1000 (1M).
	ContextWindowK int `json:"context_window_k"`
	// ThinkingType 独立控制思考「开关」(thinking.type):"" = 不发送(默认);
	// "disabled" = 显式关闭; "enabled" = 开启. 与 ReasoningEffort 解耦.
	ThinkingType string `json:"thinking_type"`
	// ReasoningEffort 独立控制思考「强度」:"" = 不发送(默认);
	// "low"/"medium"/"high"/"xhigh"/"max" = 对应强度. 见 agent.Config.NewProvider.
	ReasoningEffort string `json:"reasoning_effort"`
	IsDefault       bool   `json:"is_default"`
	// Priority orders the failover chain: higher goes first. The ACTIVE profile
	// (IsDefault) always heads the chain regardless of this value.
	Priority int `json:"priority"`
	// PoolExclude=true keeps this profile out of the failover chain — it stays
	// usable when an agent/task binds it explicitly, it just never gets picked up
	// as a fallback target.
	PoolExclude bool `json:"pool_exclude"`
	// Streaming selects the wire protocol: true (default) = streaming (SSE);
	// false = real non-streaming (stream:false, single JSON response via
	// Provider.Complete). Non-streaming sidesteps flaky gateway SSE at the cost of
	// live in-run progress. Maps to agent.Config.Stream.
	Streaming bool `json:"streaming"`
	// MaxTokens caps a single reply's output in tokens. 0 = send no cap and let
	// the endpoint's own default apply (the historical behaviour). Unlike
	// ContextWindowK — the model's total capacity, used locally to size compaction
	// — this value travels with every request.
	MaxTokens int `json:"max_tokens"`
	// MaxTokensField picks the request key carrying MaxTokens, for format
	// "openai" only: "" = max_tokens (default); "max_completion_tokens" = the
	// newer key, which OpenAI's reasoning models require and whose budget covers
	// reasoning tokens plus visible output. Ignored by anthropic and
	// openai-responses, which name the field themselves.
	MaxTokensField string `json:"max_tokens_field"`
	// SessionHeaderKey, when non-empty, names a custom HTTP header sent on every
	// request built from this profile; its value is the current run's session id
	// (chat conversation / worker intent). For gateways that key prompt caching
	// or sticky routing off a session-id header. "" = not sent. Maps to
	// agent.Config.SessionHeaderKey.
	SessionHeaderKey string `json:"session_header_key"`
	// Retry overrides this profile's share of the retry ladder. Zero value =
	// inherit the global policy (LLMRetryPolicy), so an untouched profile behaves
	// exactly as before. See RetryOverride.
	Retry RetryOverride `json:"retry"`
}

// RetryOverride is one profile's optional override of the three retry layers
// that are per-endpoint: 建连(connect) / 空响应(empty) / 同 provider 安全窗口
// (stream). Each rule's zero value means "inherit the global policy"; see
// RetryRule for the -1 / 0 / >0 semantics.
type RetryOverride struct {
	Connect RetryRule `json:"connect"`
	Empty   RetryRule `json:"empty"`
	Stream  RetryRule `json:"stream"`
}

// profileCols is the read column list (hint variant, no api key) shared by the
// list query; profileColsKey is the same with api_key for the single-row loads.
const profileRetryCols = `COALESCE(retry_connect_attempts,0),COALESCE(retry_connect_interval_ms,0),COALESCE(retry_empty_attempts,0),COALESCE(retry_empty_interval_ms,0),COALESCE(retry_stream_attempts,0),COALESCE(retry_stream_interval_ms,0)`
const profileCols = `id,name,format,COALESCE(base_url,''),COALESCE(proxy,''),model,COALESCE(models,'[]'::jsonb),COALESCE(api_key_hint,''),rate_per_second,rate_per_minute,context_window_k,COALESCE(reasoning_effort,''),is_default,priority,pool_exclude,COALESCE(thinking_type,''),COALESCE(streaming,true),COALESCE(max_tokens,0),COALESCE(max_tokens_field,''),COALESCE(session_header_key,''),` + profileRetryCols + `,COALESCE(api_key_cipher,'')`
const profileColsKey = `id,name,format,COALESCE(base_url,''),COALESCE(proxy,''),model,COALESCE(models,'[]'::jsonb),COALESCE(api_key,''),rate_per_second,rate_per_minute,context_window_k,COALESCE(reasoning_effort,''),is_default,priority,pool_exclude,COALESCE(thinking_type,''),COALESCE(streaming,true),COALESCE(max_tokens,0),COALESCE(max_tokens_field,''),COALESCE(session_header_key,''),` + profileRetryCols + `,COALESCE(api_key_cipher,'')`

// scanProfile reads one row in the profileCols / profileColsKey column order. The
// 7th column lands in APIKeyHint or APIKey depending on which list the caller used.
func scanProfile(sc interface{ Scan(...any) error }, into *string, p *LLMProfile) error {
	var models []byte
	if err := sc.Scan(&p.ID, &p.Name, &p.Format, &p.BaseURL, &p.Proxy, &p.Model, &models, into,
		&p.RatePerSecond, &p.RatePerMinute, &p.ContextWindowK, &p.ReasoningEffort, &p.IsDefault, &p.Priority, &p.PoolExclude, &p.ThinkingType, &p.Streaming,
		&p.MaxTokens, &p.MaxTokensField, &p.SessionHeaderKey,
		&p.Retry.Connect.Attempts, &p.Retry.Connect.IntervalMS,
		&p.Retry.Empty.Attempts, &p.Retry.Empty.IntervalMS,
		&p.Retry.Stream.Attempts, &p.Retry.Stream.IntervalMS,
		&p.APIKeyCipher); err != nil {
		return err
	}
	_ = json.Unmarshal(models, &p.Models)
	// 归一化：空列表 = [默认模型]；默认模型必须出现在列表里（迁移保证，写路径兜底）。
	if len(p.Models) == 0 {
		p.Models = []string{p.Model}
	}
	found := false
	for _, m := range p.Models {
		if m == p.Model {
			found = true
			break
		}
	}
	if !found {
		p.Models = append([]string{p.Model}, p.Models...)
	}
	return nil
}

func (d *DB) ListProfiles() ([]*LLMProfile, error) {
	rows, err := d.Query(`SELECT ` + profileCols + ` FROM llm_profiles ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*LLMProfile
	for rows.Next() {
		var p LLMProfile
		if err := scanProfile(rows, &p.APIKeyHint, &p); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

// ActiveProfile returns the default (active) profile with its api key, or nil.
func (d *DB) ActiveProfile() (*LLMProfile, error) {
	var p LLMProfile
	err := scanProfile(d.QueryRow(`SELECT `+profileColsKey+` FROM llm_profiles WHERE is_default LIMIT 1`), &p.APIKey, &p)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := resolveProfileAPIKey(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

// ProfileByID returns one profile with its api key by id, or nil if not found.
// Used to run a task on a specific (non-default) LLM profile.
func (d *DB) ProfileByID(id int64) (*LLMProfile, error) {
	var p LLMProfile
	err := scanProfile(d.QueryRow(`SELECT `+profileColsKey+` FROM llm_profiles WHERE id=$1`, id), &p.APIKey, &p)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := resolveProfileAPIKey(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

// PoolProfiles returns the failover chain in run order, api keys included: the
// active profile first, then every other keyed profile that isn't excluded, by
// priority DESC (id ASC to stay stable). Profiles without an api key can't serve
// a request, so they never enter the chain. The ordering IS the policy — callers
// walk the slice front to back.
func (d *DB) PoolProfiles() ([]*LLMProfile, error) {
	rows, err := d.Query(`SELECT ` + profileColsKey + ` FROM llm_profiles
WHERE (COALESCE(api_key,'') <> '' OR COALESCE(api_key_cipher,'') <> '') AND (is_default OR NOT pool_exclude)
ORDER BY is_default DESC, priority DESC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*LLMProfile
	for rows.Next() {
		var p LLMProfile
		if err := scanProfile(rows, &p.APIKey, &p); err != nil {
			return nil, err
		}
		if err := resolveProfileAPIKey(&p); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

// SaveProfile inserts (id==0) or updates a profile. Empty apiKey on update keeps existing.
func (d *DB) SaveProfile(p *LLMProfile) (int64, error) {
	hint := p.APIKeyHint
	if len(p.APIKey) >= 4 {
		hint = "…" + p.APIKey[len(p.APIKey)-4:]
	}
	r := p.Retry.Clamped()
	// 加密落库：密文进 api_key_cipher，明文列留空。
	// 没配主密钥时保持原样（明文落库）—— 既有部署不能因为这次加固起不来。
	cipherText := ""
	plainColumn := p.APIKey
	if p.APIKey != "" {
		key, err := config.MasterKey()
		if err != nil {
			return 0, err
		}
		if len(key) > 0 {
			cipherText, err = encryptSecret(p.APIKey, key, p.Name)
			if err != nil {
				return 0, err
			}
			// 密文落库后，明文列必须置空 —— 两处都留明文等于没加密。
			plainColumn = ""
		}
	}
	// E1：models 归一化——空 = [默认模型]；默认模型必须存在于列表首位。
	if len(p.Models) == 0 {
		p.Models = []string{p.Model}
	}
	found := false
	for _, m := range p.Models {
		if m == p.Model {
			found = true
			break
		}
	}
	if !found {
		p.Models = append([]string{p.Model}, p.Models...)
	}
	modelsJSON, _ := json.Marshal(p.Models)
	baseURLsJSON, _ := json.Marshal(p.BaseURLs)
	if p.ID == 0 {
		var id int64
		err := d.QueryRow(`INSERT INTO llm_profiles(name,format,base_url,proxy,model,models,base_urls,api_key,api_key_hint,rate_per_second,rate_per_minute,context_window_k,reasoning_effort,priority,pool_exclude,thinking_type,streaming,max_tokens,max_tokens_field,session_header_key,retry_connect_attempts,retry_connect_interval_ms,retry_empty_attempts,retry_empty_interval_ms,retry_stream_attempts,retry_stream_interval_ms,api_key_cipher)
VALUES ($1,$2,NULLIF($3,''),NULLIF($4,''),$5,$6,$7,NULLIF($8,''),NULLIF($9,''),$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,NULLIF($27,'')) RETURNING id`,
			p.Name, p.Format, p.BaseURL, p.Proxy, p.Model, string(modelsJSON), string(baseURLsJSON), plainColumn, hint, p.RatePerSecond, p.RatePerMinute, p.ContextWindowK, p.ReasoningEffort, p.Priority, p.PoolExclude, p.ThinkingType, p.Streaming, p.MaxTokens, p.MaxTokensField, p.SessionHeaderKey,
			r.Connect.Attempts, r.Connect.IntervalMS, r.Empty.Attempts, r.Empty.IntervalMS, r.Stream.Attempts, r.Stream.IntervalMS, cipherText).Scan(&id)
		return id, err
	}
	if p.APIKey == "" {
		// 保留原 key。但 AAD 绑的是行名：改名后旧密文解不开，
		// 故取旧名解出明文、用新名重加密（必须在改名的 UPDATE 之前做，
		// 否则旧名就丢了）。
		if err := d.reencryptProfileKey(p.ID, p.Name); err != nil {
			return p.ID, err
		}
		_, err := d.Exec(`UPDATE llm_profiles SET name=$1,format=$2,base_url=NULLIF($3,''),proxy=NULLIF($4,''),model=$5,models=$6,rate_per_second=$7,rate_per_minute=$8,context_window_k=$9,reasoning_effort=$10,priority=$11,pool_exclude=$12,thinking_type=$13,streaming=$14,max_tokens=$15,max_tokens_field=$16,session_header_key=$17,retry_connect_attempts=$18,retry_connect_interval_ms=$19,retry_empty_attempts=$20,retry_empty_interval_ms=$21,retry_stream_attempts=$22,retry_stream_interval_ms=$23 WHERE id=$24`,
			p.Name, p.Format, p.BaseURL, p.Proxy, p.Model, string(modelsJSON), p.RatePerSecond, p.RatePerMinute, p.ContextWindowK, p.ReasoningEffort, p.Priority, p.PoolExclude, p.ThinkingType, p.Streaming, p.MaxTokens, p.MaxTokensField, p.SessionHeaderKey,
			r.Connect.Attempts, r.Connect.IntervalMS, r.Empty.Attempts, r.Empty.IntervalMS, r.Stream.Attempts, r.Stream.IntervalMS, p.ID)
		return p.ID, err
	}
	_, err := d.Exec(`UPDATE llm_profiles SET name=$1,format=$2,base_url=NULLIF($3,''),proxy=NULLIF($4,''),model=$5,models=$6,api_key=$7,api_key_hint=$8,rate_per_second=$9,rate_per_minute=$10,context_window_k=$11,reasoning_effort=$12,priority=$13,pool_exclude=$14,thinking_type=$15,streaming=$16,max_tokens=$17,max_tokens_field=$18,session_header_key=$19,retry_connect_attempts=$20,retry_connect_interval_ms=$21,retry_empty_attempts=$22,retry_empty_interval_ms=$23,retry_stream_attempts=$24,retry_stream_interval_ms=$25,api_key_cipher=NULLIF($26,'') WHERE id=$27`,
		p.Name, p.Format, p.BaseURL, p.Proxy, p.Model, string(modelsJSON), plainColumn, hint, p.RatePerSecond, p.RatePerMinute, p.ContextWindowK, p.ReasoningEffort, p.Priority, p.PoolExclude, p.ThinkingType, p.Streaming, p.MaxTokens, p.MaxTokensField, p.SessionHeaderKey,
		r.Connect.Attempts, r.Connect.IntervalMS, r.Empty.Attempts, r.Empty.IntervalMS, r.Stream.Attempts, r.Stream.IntervalMS, cipherText, p.ID)
	return p.ID, err
}

var (
	ErrActiveLLMProfileDelete      = errors.New("cannot delete the active LLM profile; activate another profile first")
	ErrLLMProfileReferencesChanged = errors.New("LLM profile references changed while deleting; retry the request")
	ErrLLMProfileNotFound          = errors.New("LLM profile not found")
)

func (d *DB) DeleteProfile(id int64) error {
	return d.DeleteProfileContext(context.Background(), id)
}

// DeleteProfileContext removes a non-default profile while preserving the task
// failover cursor. Reference changes are retried because a task, agent, or
// conversation may start pointing at the profile between the initial scan and
// the profile row lock. A bound prevents a continuously changing workload from
// keeping an HTTP request alive forever.
func (d *DB) DeleteProfileContext(ctx context.Context, id int64) error {
	const (
		maxAttempts = 8
		maxDuration = 15 * time.Second
	)
	ctx, cancel := context.WithTimeout(ctx, maxDuration)
	defer cancel()
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		retry, err := d.deleteProfile(ctx, id)
		if err != nil || !retry {
			return err
		}
	}
	return fmt.Errorf("%w: profile %d", ErrLLMProfileReferencesChanged, id)
}

func (d *DB) deleteProfile(ctx context.Context, id int64) (bool, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	// Reference setters lock their task/agent/conversation row before an
	// llm_profiles row can be locked by a foreign-key check. Keep deletion in the
	// same order: the old profile -> child order deadlocked with a setter's child
	// -> profile order. Sorting also gives concurrent profile deletions a stable
	// order when their chains overlap multiple tasks.
	lockedTasks, err := lockProfileReferenceRows(ctx, tx, `SELECT t.id
FROM tasks t
WHERE t.llm_profile_id=$1
   OR t.active_llm_profile_id=$1
   OR EXISTS (
       SELECT 1 FROM task_llm_profiles x
       WHERE x.task_id=t.id AND x.profile_id=$1
   )
ORDER BY t.id
FOR UPDATE OF t`, id)
	if err != nil {
		return false, err
	}
	lockedAgents, err := lockProfileReferenceRows(ctx, tx, `SELECT id FROM agents
WHERE llm_profile_id=$1
ORDER BY id
FOR UPDATE`, id)
	if err != nil {
		return false, err
	}
	lockedConversations, err := lockProfileReferenceRows(ctx, tx, `SELECT id FROM conversations
WHERE llm_profile_id=$1
ORDER BY id
FOR UPDATE`, id)
	if err != nil {
		return false, err
	}

	var isDefault bool
	if err := tx.QueryRowContext(ctx, `SELECT is_default FROM llm_profiles WHERE id=$1 FOR UPDATE`, id).Scan(&isDefault); err != nil {
		if err == sql.ErrNoRows {
			return false, ErrLLMProfileNotFound
		}
		return false, err
	}
	if isDefault {
		return false, ErrActiveLLMProfileDelete
	}

	type affectedTask struct {
		id        int64
		position  int
		wasActive bool
	}
	// A task may have committed a new reference after the first statement took
	// its snapshot but before this transaction acquired the profile lock. The
	// profile lock now prevents further references; retry if that committed task
	// was not part of the task-first lock set. Never acquire a new task lock while
	// holding the profile lock, because that would recreate the inversion.
	rows, err := tx.QueryContext(ctx, `SELECT ref_kind, ref_id FROM (
	SELECT 'task'::text AS ref_kind, t.id AS ref_id
FROM tasks t
WHERE t.llm_profile_id=$1
   OR t.active_llm_profile_id=$1
   OR EXISTS (
       SELECT 1 FROM task_llm_profiles x
       WHERE x.task_id=t.id AND x.profile_id=$1
   )
	UNION ALL
	SELECT 'agent', a.id FROM agents a WHERE a.llm_profile_id=$1
	UNION ALL
	SELECT 'conversation', c.id FROM conversations c WHERE c.llm_profile_id=$1
) refs
ORDER BY ref_kind, ref_id`, id)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var (
			kind  string
			rowID int64
		)
		if err := rows.Scan(&kind, &rowID); err != nil {
			rows.Close()
			return false, err
		}
		locked := false
		switch kind {
		case "task":
			_, locked = lockedTasks[rowID]
		case "agent":
			_, locked = lockedAgents[rowID]
		case "conversation":
			_, locked = lockedConversations[rowID]
		}
		if !locked {
			rows.Close()
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}

	rows, err = tx.QueryContext(ctx, `SELECT x.task_id, x.position, COALESCE(t.active_llm_profile_id=$1, false)
FROM task_llm_profiles x
JOIN tasks t ON t.id=x.task_id
WHERE x.profile_id=$1
ORDER BY x.task_id`, id)
	if err != nil {
		return false, err
	}
	var affected []affectedTask
	for rows.Next() {
		var task affectedTask
		if err := rows.Scan(&task.id, &task.position, &task.wasActive); err != nil {
			rows.Close()
			return false, err
		}
		affected = append(affected, task)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM llm_profiles WHERE id=$1`, id); err != nil {
		return false, err
	}
	// Only a successor after the deleted cursor is eligible. If there is none,
	// clear the explicit chain so the task falls back to its Agent/global provider;
	// profiles before a manually selected cursor must never be revived.
	for _, task := range affected {
		if !task.wasActive {
			if _, err := tx.ExecContext(ctx, `UPDATE tasks SET llm_chain_revision=llm_chain_revision+1 WHERE id=$1`, task.id); err != nil {
				return false, err
			}
			continue
		}
		var next int64
		err := tx.QueryRowContext(ctx, `SELECT profile_id FROM task_llm_profiles
WHERE task_id=$1 AND position>$2 AND status='ready'
ORDER BY position
LIMIT 1`, task.id, task.position).Scan(&next)
		if err != nil && err != sql.ErrNoRows {
			return false, err
		}
		if err == sql.ErrNoRows {
			if _, err := tx.ExecContext(ctx, `DELETE FROM task_llm_profiles WHERE task_id=$1`, task.id); err != nil {
				return false, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE tasks
SET active_llm_profile_id=NULL, llm_profile_id=NULL, llm_chain_revision=llm_chain_revision+1
WHERE id=$1`, task.id); err != nil {
				return false, err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tasks
SET active_llm_profile_id=$2, llm_profile_id=$2, llm_chain_revision=llm_chain_revision+1
WHERE id=$1`, task.id, next); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return false, nil
}

func lockProfileReferenceRows(ctx context.Context, tx *sql.Tx, query string, profileID int64) (map[int64]struct{}, error) {
	rows, err := tx.QueryContext(ctx, query, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	locked := make(map[int64]struct{})
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		locked[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return locked, nil
}

// SetActiveProfile makes one profile the global default (single-default invariant).
func (d *DB) SetActiveProfile(id int64) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE llm_profiles SET is_default=false WHERE is_default`); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE llm_profiles SET is_default=true WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrLLMProfileNotFound
	}
	return tx.Commit()
}

// ---------- Agents / prompts ----------

type Agent struct {
	ID               int64  `json:"id"`
	Key              string `json:"key"`
	Name             string `json:"name"`
	Description      string `json:"description"`
	Role             string `json:"role"`
	Builtin          bool   `json:"builtin"`
	Enabled          bool   `json:"enabled"`
	LLMProfileID     *int64 `json:"llm_profile_id"`    // 绑定的 LLM 配置;nil=跟随任务/会话 pin,再回退全局激活
	MaxTurns         int    `json:"max_turns"`         // 单次运行最大轮次;0=不限制
	RunSecs          int    `json:"run_seconds"`       // worker 单次运行墙钟上限(秒);0=不限制
	WebSearch        bool   `json:"web_search"`        // 是否启用网络搜索(受系统全局开关门控)
	InteractiveShell bool   `json:"interactive_shell"` // 是否启用交互式 shell(持久 PTY 会话工具族)
	WrapupPrompt     string `json:"wrapup_prompt"`     // 收尾提示词(超时/步数耗尽时的 settlement 提示);空=用代码内置默认
	WrapupMaxTurns   int    `json:"wrapup_max_turns"`  // 收尾阶段自身的轮数预算;0=用代码内置默认(按 agent)
	// 任务级超时收尾词(与 per-run 两套;仅 worker/planner 用);空/0=用代码内置默认。
	TaskTimeoutWrapupPrompt   string `json:"task_timeout_wrapup_prompt"`
	TaskTimeoutWrapupMaxTurns int    `json:"task_timeout_wrapup_max_turns"`
	// P3 触发后处理策略(仅自定义 agent 有意义):
	// TriggerRunMode  serial|parallel — 串行排队 / 每次触发各自并发一个会话
	// TriggerMergeMode by_task|all|none — 仅 serial 用:同任务合并 / 全部合并 / 不合并
	// TriggerMaxParallel — 仅 parallel 用的每 agent 并发上限;0=不限
	TriggerRunMode     string `json:"trigger_run_mode"`
	TriggerMergeMode   string `json:"trigger_merge_mode"`
	TriggerMaxParallel int    `json:"trigger_max_parallel"`
	// ThinkingEffort 是 agent 级思考档位覆盖（方案 v9.2 批次 B）：
	// ''=跟随绑定 profile 的默认；'enabled'/'disabled'=强制思考开关（switch
	// 形态模型）；其余=reasoning_effort 档位（effort 形态模型）。
	ThinkingEffort string `json:"thinking_effort"`
	// ChatVisible 是会话选择器可见性（EDGE E1）：false = 不作为对话对象出现在
	// chat 页选择器（执行体 agent 用，如 edge_exec）。与 Builtin（是否平台预置）、
	// Role（扮演什么）三者正交，不复用任何一列。存量行默认 true，行为不变。
	ChatVisible bool `json:"chat_visible"`
}

const agentCols = `id,key,name,COALESCE(description,''),role,builtin,enabled,COALESCE(max_turns,0),COALESCE(run_seconds,600),COALESCE(web_search,false),COALESCE(interactive_shell,false),COALESCE(wrapup_prompt,''),COALESCE(wrapup_max_turns,0),COALESCE(task_timeout_wrapup_prompt,''),COALESCE(task_timeout_wrapup_max_turns,0),COALESCE(trigger_run_mode,'serial'),COALESCE(trigger_merge_mode,'all'),COALESCE(trigger_max_parallel,5),llm_profile_id,COALESCE(thinking_effort,''),COALESCE(chat_visible,true)`

func scanAgent(sc interface{ Scan(...any) error }) (*Agent, error) {
	var a Agent
	var prof sql.NullInt64 // llm_profile_id 可空:未绑定时为 NULL
	err := sc.Scan(&a.ID, &a.Key, &a.Name, &a.Description, &a.Role, &a.Builtin, &a.Enabled, &a.MaxTurns, &a.RunSecs, &a.WebSearch, &a.InteractiveShell, &a.WrapupPrompt, &a.WrapupMaxTurns, &a.TaskTimeoutWrapupPrompt, &a.TaskTimeoutWrapupMaxTurns, &a.TriggerRunMode, &a.TriggerMergeMode, &a.TriggerMaxParallel, &prof, &a.ThinkingEffort, &a.ChatVisible)
	if err == nil && prof.Valid {
		v := prof.Int64
		a.LLMProfileID = &v
	}
	return &a, err
}

func (d *DB) ListAgents() ([]*Agent, error) {
	rows, err := d.Query(`SELECT ` + agentCols + ` FROM agents ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (d *DB) GetAgentByKey(key string) (*Agent, error) {
	a, err := scanAgent(d.QueryRow(`SELECT `+agentCols+` FROM agents WHERE key=$1`, key))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

// AgentBindingCounts returns per-agent binding counts in a few grouped queries
// (NO N+1): visible MCP servers and skills keyed by agent id, and bound tools
// keyed by agent key (tools.agents is a JSONB array of agent keys). Missing keys
// mean zero. Used to show "MCP N · Skill N · 工具 N" on the agent cards.
func (d *DB) AgentBindingCounts() (mcp map[int64]int, skill map[int64]int, tools map[string]int, err error) {
	mcp, skill, tools = map[int64]int{}, map[int64]int{}, map[string]int{}
	byID := func(q string, into map[int64]int) error {
		rows, e := d.Query(q)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var n int
			if e := rows.Scan(&id, &n); e != nil {
				return e
			}
			into[id] = n
		}
		return rows.Err()
	}
	if err = byID(`SELECT agent_id, count(DISTINCT resource_id) FROM agent_visibility WHERE resource_kind='mcp' AND enabled GROUP BY agent_id`, mcp); err != nil {
		return
	}
	if err = byID(`SELECT agent_id, count(*) FROM agent_skill_visibility WHERE enabled GROUP BY agent_id`, skill); err != nil {
		return
	}
	rows, e := d.Query(`SELECT elem, count(*) FROM tools, jsonb_array_elements_text(agents) AS elem GROUP BY elem`)
	if e != nil {
		err = e
		return
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var n int
		if e := rows.Scan(&k, &n); e != nil {
			err = e
			return
		}
		tools[k] = n
	}
	err = rows.Err()
	return
}

// CreateAgent inserts a custom (builtin=false) conversational agent with role
// 'assistant'. Returns the new row. Callers validate key/name upstream; the DB
// enforces key charset + uniqueness and the role check constraint.
func (d *DB) CreateAgent(key, name, description string) (*Agent, error) {
	a, err := scanAgent(d.QueryRow(`
INSERT INTO agents(key, name, description, role, builtin, enabled)
VALUES ($1, $2, NULLIF($3,''), 'assistant', false, true)
RETURNING `+agentCols, key, name, description))
	if err != nil {
		return nil, err
	}
	return a, nil
}

// UpdateAgentMeta updates a custom agent's display name + description. Built-in
// agents are left untouched (guarded by the caller / the builtin flag).
func (d *DB) UpdateAgentMeta(key, name, description string) error {
	_, err := d.Exec(`UPDATE agents SET name=$2, description=NULLIF($3,'') WHERE key=$1 AND builtin=false`, key, name, description)
	return err
}

// DeleteAgent removes a custom agent. Built-in agents are protected by the
// builtin=false guard. agent_prompts / agent_prompt_vars / visibility rows cascade
// via FK; tools.agents bindings for the key are cleaned by the caller.
func (d *DB) DeleteAgent(key string) error {
	_, err := d.Exec(`DELETE FROM agents WHERE key=$1 AND builtin=false`, key)
	return err
}

// SetAgentMaxTurns updates an agent's max_turns (0 = unlimited).
func (d *DB) SetAgentMaxTurns(key string, maxTurns int) error {
	if maxTurns < 0 {
		maxTurns = 0
	}
	_, err := d.Exec(`UPDATE agents SET max_turns=$1 WHERE key=$2`, maxTurns, key)
	return err
}

// SetAgentLLMProfile binds an agent to a specific LLM profile (id != nil), or clears
// the binding (id == nil) so the agent follows the task/conversation pin, else the
// global active profile. Precedence at runtime: agent binding → task/conv pin → active.
func (d *DB) SetAgentLLMProfile(key string, id *int64) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// DeleteProfile locks reference rows before the profile row. Keep the same
	// order here so a concurrent rebind cannot form a child/profile deadlock.
	var agentID int64
	if err := tx.QueryRow(`SELECT id FROM agents WHERE key=$1 FOR UPDATE`, key).Scan(&agentID); err != nil {
		if err == sql.ErrNoRows {
			// Preserve the previous UPDATE semantics: an unknown key is a no-op.
			return tx.Commit()
		}
		return err
	}
	if err := lockLLMProfileForReference(tx, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE agents SET llm_profile_id=$1 WHERE id=$2`, id, agentID); err != nil {
		return err
	}
	return tx.Commit()
}

// lockLLMProfileForReference makes the profile side of the shared lock-order
// protocol explicit. Callers must already own the referencing child row.
func lockLLMProfileForReference(tx *sql.Tx, profileID *int64) error {
	if profileID == nil {
		return nil
	}
	var lockedID int64
	if err := tx.QueryRow(`SELECT id FROM llm_profiles WHERE id=$1 FOR KEY SHARE`, *profileID).Scan(&lockedID); err != nil {
		if err == sql.ErrNoRows {
			return ErrLLMProfileNotFound
		}
		return err
	}
	return nil
}

// SetAgentWebSearch toggles whether an agent uses network search (still gated by
// the global web-search master switch + backend/key config).
func (d *DB) SetAgentWebSearch(key string, on bool) error {
	_, err := d.Exec(`UPDATE agents SET web_search=$1 WHERE key=$2`, on, key)
	return err
}

// SetAgentThinkingEffort stores the agent-level thinking-effort override
// (方案 v9.2 批次 B). ''=跟随绑定 profile 默认；'enabled'/'disabled'=强制思考
// 开关；其余=effort 档位。合法值由服务端白名单校验后传入。
func (d *DB) SetAgentThinkingEffort(key, effort string) error {
	_, err := d.Exec(`UPDATE agents SET thinking_effort=NULLIF($1,'') WHERE key=$2`, effort, key)
	return err
}

// SetAgentChatVisible 是会话选择器可见性的写入口（EDGE E1）。语义与 builtin/role
// 正交：false 只影响「是否出现在 chat 页选择器」，不影响触发、编排与工具绑定。
func (d *DB) SetAgentChatVisible(key string, visible bool) error {
	_, err := d.Exec(`UPDATE agents SET chat_visible=$1 WHERE key=$2`, visible, key)
	return err
}

// SetAgentInteractiveShell toggles whether an agent gets the interactive shell
// (持久 PTY 会话) tool family + Bash 提示词联动(见 docs/交互式shell设计.md §14.2).
func (d *DB) SetAgentInteractiveShell(key string, on bool) error {
	_, err := d.Exec(`UPDATE agents SET interactive_shell=$1 WHERE key=$2`, on, key)
	return err
}

// SetAgentWrapupPrompt stores an agent's wrap-up (settlement) prompt. Empty string
// means "use the code built-in default" — resolved at runtime by resolveWrapup.
func (d *DB) SetAgentWrapupPrompt(key, prompt string) error {
	_, err := d.Exec(`UPDATE agents SET wrapup_prompt=$1 WHERE key=$2`, prompt, key)
	return err
}

// SetAgentWrapupMaxTurns stores the wrap-up phase's own turn budget. 0 means "use
// the code built-in default" — resolved at runtime by resolveWrapupTurns.
func (d *DB) SetAgentWrapupMaxTurns(key string, n int) error {
	_, err := d.Exec(`UPDATE agents SET wrapup_max_turns=$1 WHERE key=$2`, n, key)
	return err
}

// SetAgentTaskTimeoutWrapup stores an agent's task-timeout wrap-up prompt (empty =
// use code built-in default; only worker/planner have one) and its turn budget
// (0 = default). Resolved at runtime by resolveTaskTimeoutWrapup / …Turns.
func (d *DB) SetAgentTaskTimeoutWrapup(key, prompt string, maxTurns int) error {
	_, err := d.Exec(`UPDATE agents SET task_timeout_wrapup_prompt=$1, task_timeout_wrapup_max_turns=$2 WHERE key=$3`, prompt, maxTurns, key)
	return err
}

// SetAgentRunSeconds updates an agent's run_seconds wall-clock budget (0 = unlimited).
func (d *DB) SetAgentRunSeconds(key string, runSecs int) error {
	if runSecs < 0 {
		runSecs = 0
	}
	_, err := d.Exec(`UPDATE agents SET run_seconds=$1 WHERE key=$2`, runSecs, key)
	return err
}

// SetAgentTriggerBehavior stores an agent's P3 trigger post-processing策略:
// runMode(serial|parallel) / mergeMode(by_task|all|none) / maxParallel(parallel 用,0=不限)。
// 枚举做白名单校验,非法值回落默认,避免脏数据把调度 pump 带偏。
func (d *DB) SetAgentTriggerBehavior(key, runMode, mergeMode string, maxParallel int) error {
	switch runMode {
	case "serial", "parallel":
	default:
		runMode = "serial"
	}
	switch mergeMode {
	case "by_task", "all", "none":
	default:
		mergeMode = "by_task"
	}
	if maxParallel < 0 {
		maxParallel = 0
	}
	_, err := d.Exec(`UPDATE agents SET trigger_run_mode=$1, trigger_merge_mode=$2, trigger_max_parallel=$3 WHERE key=$4`,
		runMode, mergeMode, maxParallel, key)
	return err
}

type PromptVar struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Example     string `json:"example"`
	Source      string `json:"source"`
}

func (d *DB) PromptVars(agentID int64) ([]PromptVar, error) {
	rows, err := d.Query(`SELECT var_name,COALESCE(description,''),COALESCE(example,''),source FROM agent_prompt_vars WHERE agent_id=$1 ORDER BY var_name`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PromptVar{}
	for rows.Next() {
		var v PromptVar
		if err := rows.Scan(&v.Name, &v.Description, &v.Example, &v.Source); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// CurrentPrompt returns the agent's active template text ("" if none set yet).
func (d *DB) CurrentPrompt(agentID int64) (string, error) {
	var tmpl sql.NullString
	err := d.QueryRow(`SELECT p.template_text FROM agents a JOIN agent_prompts p ON p.id=a.current_prompt_id WHERE a.id=$1`, agentID).Scan(&tmpl)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return tmpl.String, err
}

// SeedPromptIfEmpty writes the code-default template as the agent's first prompt
// version ONLY when it has none yet (current_prompt_id IS NULL). Mirrors
// SeedTool's first-insert-only philosophy: a user's edited prompt is never
// clobbered on restart. Idempotent — a no-op once any version exists.
func (d *DB) SeedPromptIfEmpty(agentID int64, tmpl string) error {
	var cur sql.NullInt64
	if err := d.QueryRow(`SELECT current_prompt_id FROM agents WHERE id=$1`, agentID).Scan(&cur); err != nil {
		return err
	}
	if cur.Valid {
		return nil // already seeded or user-edited → leave it
	}
	_, err := d.SavePrompt(agentID, tmpl, "内置默认", "system")
	return err
}

// ResetPromptToDefault appends the code-default template as a new version and
// points current at it — the explicit "恢复为内置默认" action.
func (d *DB) ResetPromptToDefault(agentID int64, tmpl string) (int, error) {
	return d.SavePrompt(agentID, tmpl, "恢复为内置默认", "system")
}

// SavePrompt appends a new version and points current_prompt_id at it.
func (d *DB) SavePrompt(agentID int64, template, note, by string) (int, error) {
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var ver int
	if err := tx.QueryRow(`SELECT COALESCE(max(version),0)+1 FROM agent_prompts WHERE agent_id=$1`, agentID).Scan(&ver); err != nil {
		return 0, err
	}
	var pid int64
	if err := tx.QueryRow(`INSERT INTO agent_prompts(agent_id,version,template_text,note,updated_by) VALUES ($1,$2,$3,NULLIF($4,''),NULLIF($5,'')) RETURNING id`,
		agentID, ver, template, note, by).Scan(&pid); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE agents SET current_prompt_id=$1 WHERE id=$2`, pid, agentID); err != nil {
		return 0, err
	}
	return ver, tx.Commit()
}

type PromptVersion struct {
	Version   int       `json:"version"`
	Template  string    `json:"template_text"`
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"ts"`
}

func (d *DB) ListPromptVersions(agentID int64) ([]PromptVersion, error) {
	rows, err := d.Query(`SELECT version,template_text,COALESCE(note,''),created_at FROM agent_prompts WHERE agent_id=$1 ORDER BY version DESC`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PromptVersion{}
	for rows.Next() {
		var v PromptVersion
		if err := rows.Scan(&v.Version, &v.Template, &v.Note, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ---------- MCP servers ----------

type MCPServer struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Transport string          `json:"transport"`
	Command   string          `json:"command,omitempty"`
	Args      json.RawMessage `json:"args"`
	Env       json.RawMessage `json:"env"`
	URL       string          `json:"url,omitempty"`
	Enabled   bool            `json:"enabled"`
	Insecure  bool            `json:"insecure"`        // http: skip TLS cert verification (self-signed servers, issue #108)
	Tools     []string        `json:"tools,omitempty"` // cached tool names (mcp_tools_cache)
}

func (d *DB) ListMCP() ([]*MCPServer, error) {
	rows, err := d.Query(`SELECT id,name,transport,COALESCE(command,''),args,env,COALESCE(url,''),enabled,insecure FROM mcp_servers ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var out []*MCPServer
	for rows.Next() {
		var m MCPServer
		var args, env []byte
		if err := rows.Scan(&m.ID, &m.Name, &m.Transport, &m.Command, &args, &env, &m.URL, &m.Enabled, &m.Insecure); err != nil {
			rows.Close()
			return nil, err
		}
		m.Args, m.Env = json.RawMessage(args), json.RawMessage(env)
		out = append(out, &m)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close() // free the connection before the per-server tool-cache queries below
	// Attach each server's cached tool names (best-effort; empty until discovered).
	for _, m := range out {
		m.Tools, _ = d.MCPToolNames(m.ID)
	}
	return out, nil
}

// MCPToolNames returns the cached tool names for a server (empty until discovered).
func (d *DB) MCPToolNames(serverID int64) ([]string, error) {
	rows, err := d.Query(`SELECT tool_name FROM mcp_tools_cache WHERE server_id=$1 ORDER BY tool_name`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// MCPTool is one cached tool of an MCP server (name + description).
type MCPTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// MCPToolsDetailed returns the cached tools (name + description) for a server.
func (d *DB) MCPToolsDetailed(serverID int64) ([]MCPTool, error) {
	rows, err := d.Query(`SELECT tool_name, COALESCE(description,'') FROM mcp_tools_cache WHERE server_id=$1 ORDER BY tool_name`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MCPTool
	for rows.Next() {
		var t MCPTool
		if err := rows.Scan(&t.Name, &t.Description); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SaveMCPTools replaces the cached tool list for a server (called after discovery).
func (d *DB) SaveMCPTools(serverID int64, tools []MCPTool) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM mcp_tools_cache WHERE server_id=$1`, serverID); err != nil {
		return err
	}
	for _, t := range tools {
		if _, err := tx.Exec(`INSERT INTO mcp_tools_cache(server_id, tool_name, description) VALUES ($1,$2,$3)
ON CONFLICT (server_id, tool_name) DO UPDATE SET description=EXCLUDED.description`, serverID, t.Name, t.Description); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) SaveMCP(m *MCPServer) (int64, error) {
	args, env := string(m.Args), string(m.Env)
	if args == "" {
		args = "[]"
	}
	if env == "" {
		env = "{}"
	}
	if m.ID == 0 {
		var id int64
		err := d.QueryRow(`INSERT INTO mcp_servers(name,transport,command,args,env,url,enabled,insecure) VALUES ($1,$2,NULLIF($3,''),$4,$5,NULLIF($6,''),$7,$8) RETURNING id`,
			m.Name, m.Transport, m.Command, args, env, m.URL, m.Enabled, m.Insecure).Scan(&id)
		return id, err
	}
	_, err := d.Exec(`UPDATE mcp_servers SET name=$1,transport=$2,command=NULLIF($3,''),args=$4,env=$5,url=NULLIF($6,''),enabled=$7,insecure=$8 WHERE id=$9`,
		m.Name, m.Transport, m.Command, args, env, m.URL, m.Enabled, m.Insecure, m.ID)
	return m.ID, err
}

func (d *DB) DeleteMCP(id int64) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM agent_visibility WHERE resource_kind='mcp' AND resource_id=$1`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM mcp_servers WHERE id=$1`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------- Skill visibility (agent × skill_name) ----------

// AgentSkillNames returns the skill directory names visible to an agent.
func (d *DB) AgentSkillNames(agentID int64) ([]string, error) {
	rows, err := d.Query(`SELECT skill_name FROM agent_skill_visibility WHERE agent_id=$1 AND enabled ORDER BY skill_name`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// SkillAgents returns the agent IDs that can see a skill.
func (d *DB) SkillAgents(skillName string) ([]int64, error) {
	rows, err := d.Query(`SELECT agent_id FROM agent_skill_visibility WHERE skill_name=$1 AND enabled`, skillName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SetAgentSkillVisibility replaces all skill visibility for an agent.
func (d *DB) SetAgentSkillVisibility(agentID int64, names []string) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM agent_skill_visibility WHERE agent_id=$1`, agentID); err != nil {
		return err
	}
	for _, name := range names {
		if _, err := tx.Exec(`INSERT INTO agent_skill_visibility(agent_id,skill_name,enabled) VALUES ($1,$2,true)
ON CONFLICT (agent_id,skill_name) DO UPDATE SET enabled=true`, agentID, name); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ToggleSkillVisibility sets one (agent, skill_name) visibility on/off.
func (d *DB) ToggleSkillVisibility(agentID int64, skillName string, on bool) error {
	if on {
		_, err := d.Exec(`INSERT INTO agent_skill_visibility(agent_id,skill_name,enabled) VALUES ($1,$2,true)
ON CONFLICT (agent_id,skill_name) DO UPDATE SET enabled=true`, agentID, skillName)
		return err
	}
	_, err := d.Exec(`DELETE FROM agent_skill_visibility WHERE agent_id=$1 AND skill_name=$2`, agentID, skillName)
	return err
}

// DeleteSkillVisibility removes all visibility rows for a skill (called on skill delete).
func (d *DB) DeleteSkillVisibility(skillName string) error {
	_, err := d.Exec(`DELETE FROM agent_skill_visibility WHERE skill_name=$1`, skillName)
	return err
}

// ---------- Visibility (agent × mcp) ----------

// AgentVisible returns the resource ids of a kind visible to an agent.
func (d *DB) AgentVisible(agentID int64, kind string) ([]int64, error) {
	rows, err := d.Query(`SELECT resource_id FROM agent_visibility WHERE agent_id=$1 AND resource_kind=$2 AND enabled`, agentID, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ResourceAgents returns the agent ids that can see a resource.
func (d *DB) ResourceAgents(kind string, resourceID int64) ([]int64, error) {
	rows, err := d.Query(`SELECT agent_id FROM agent_visibility WHERE resource_kind=$1 AND resource_id=$2 AND enabled`, kind, resourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SetAgentVisibilityKind replaces the full set of visible resources of a kind for
// an agent (agent-side bulk write). Bidirectional with the resource-side view —
// both read/write the same agent_visibility rows.
func (d *DB) SetAgentVisibilityKind(agentID int64, kind string, resourceIDs []int64) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM agent_visibility WHERE agent_id=$1 AND resource_kind=$2`, agentID, kind); err != nil {
		return err
	}
	for _, rid := range resourceIDs {
		if _, err := tx.Exec(`INSERT INTO agent_visibility(agent_id,resource_kind,resource_id,enabled) VALUES ($1,$2,$3,true)
ON CONFLICT (agent_id,resource_kind,resource_id,mcp_tool_name) DO UPDATE SET enabled=true`, agentID, kind, rid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ToggleVisibility sets one (agent, kind, resource) visibility on/off (idempotent).
func (d *DB) ToggleVisibility(agentID int64, kind string, resourceID int64, on bool) error {
	if on {
		_, err := d.Exec(`INSERT INTO agent_visibility(agent_id,resource_kind,resource_id,enabled) VALUES ($1,$2,$3,true)
ON CONFLICT (agent_id,resource_kind,resource_id,mcp_tool_name) DO UPDATE SET enabled=true`, agentID, kind, resourceID)
		return err
	}
	_, err := d.Exec(`DELETE FROM agent_visibility WHERE agent_id=$1 AND resource_kind=$2 AND resource_id=$3`, agentID, kind, resourceID)
	return err
}

// resolveProfileAPIKey 把密文解成明文 key。
//
// 三种情形：
//   - 没有密文：历史明文行（api_key 列仍有值），照旧返回，不报错。
//   - 有密文且配了主密钥：解出明文。
//   - 有密文但没配主密钥：报错 —— 不能静默当成「无 key」，否则会表现为
//     「配了却调不通」这种难查的现象。
func resolveProfileAPIKey(p *LLMProfile) error {
	if p.APIKeyCipher == "" {
		return nil
	}
	key, err := config.MasterKey()
	if err != nil {
		return err
	}
	if len(key) == 0 {
		return fmt.Errorf("profile %q 的 api_key 已加密，但未配置 %s：无法解密", p.Name, config.MasterKeyFileEnv)
	}
	plain, err := decryptSecret(p.APIKeyCipher, key, p.Name)
	if err != nil {
		return fmt.Errorf("profile %q 的 api_key 解密失败: %w", p.Name, err)
	}
	p.APIKey = plain
	return nil
}

// reencryptProfileKey 在行名变化时用新名重加密该行的 key。
//
// AAD 绑的是行名，故改名后旧密文解不开；这里取旧名解出明文、用新名重加密。
func (d *DB) reencryptProfileKey(id int64, newName string) error {
	var oldName, cipherText string
	err := d.QueryRow(`SELECT name, COALESCE(api_key_cipher,'') FROM llm_profiles WHERE id=$1`, id).Scan(&oldName, &cipherText)
	if err != nil {
		return err
	}
	if cipherText == "" || oldName == newName {
		return nil
	}
	key, err := config.MasterKey()
	if err != nil {
		return err
	}
	if len(key) == 0 {
		return nil // 没配主密钥：明文行，无需重加密
	}
	plain, err := decryptSecret(cipherText, key, oldName)
	if err != nil {
		return fmt.Errorf("profile %q 改名失败：旧名下的密文解不开: %w", oldName, err)
	}
	newCipher, err := encryptSecret(plain, key, newName)
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE llm_profiles SET api_key_cipher=$2 WHERE id=$1`, id, newCipher)
	return err
}

// ---------- 顾问型 agent 的工具绑定策略（curator / verifier / redteam / creative / intent-normalizer） ----------
//
// 背景：这五个 agent 只【提议】，不【裁决】——「模型只提议，确定性层裁决」。所以它们
// 绝不能持有改写平台共享状态（finding / intent / scope / 流量绑定 / 目标 / 资产 /
// 任务 / 平台配置）的工具：那会让它们从顾问变成写入者，绕过确定性层直接改状态。
//
// 策略为什么定义在 db 层：工具绑定落在 tools.agents（jsonb 数组），是 db 的资产；而
// agent 包依赖 db（agent → db），db 不能反向依赖 agent。策略放 db、agent 侧引用，
// 既避免循环依赖，也避免两份策略各自漂移。
//
// 三道闸，缺一不可：
//  1. 白名单：只有显式列进 Allow 的工具才可能被绑定——「没列＝不给」。
//  2. 黑名单：Deny 与平台级「共享状态写」判定优先级高于白名单：就算有人误把
//     report_finding 写进 Allow，也绑不上。拒绝优先是本策略的核心不变量。
//  3. 能力闸：能执行命令/发请求的工具（kind=command|script/http，或 shell/Bash 系）
//     对 DenyExec 的 agent 一律不绑——仲裁者与验证者都不许动手。
//
// 尚未注册的工具：Allow 里的名字若在 tools 表里还没有对应行（策略库阶段未落地），
// 跳过并记录；那一阶段落地后由下一次协调自动绑上，不需要再改代码。

// AdvisoryToolCap 描述一个工具对顾问 agent 的能力面。
type AdvisoryToolCap int

const (
	// AdvisoryCapRead 只读：读资产/漏洞/流量/任务态势，不改任何状态。
	AdvisoryCapRead AdvisoryToolCap = iota
	// AdvisoryCapAdvisoryWrite 写顾问 agent 自己的建议通道（submit_* / propose_*）：
	// 写的是「提议表」，最终由确定性层裁决，与改写共享状态不是一回事。
	AdvisoryCapAdvisoryWrite
	// AdvisoryCapGraphWrite 改写平台共享状态机（finding/intent/scope/流量绑定/…）。
	AdvisoryCapGraphWrite
	// AdvisoryCapExec 执行命令或发起请求。
	AdvisoryCapExec
)

// AdvisoryAgentToolPolicy 是一个顾问型 agent 的工具绑定策略。
type AdvisoryAgentToolPolicy struct {
	Agent     string   // agent key
	Allow     []string // 白名单：允许绑定的工具 key（按名字；未注册则跳过并记录）
	Deny      []string // 黑名单：显式禁止的工具 key（优先级高于白名单）
	DenyExec  bool     // 禁止可执行工具（命令/请求）
	DenyWrite bool     // 禁止改写平台共享状态的工具
}

// advisoryGraphWriteTools 是【改写平台共享状态机】的工具 key。
//
// 这份清单是顾问 agent 隔离的结构性保证：白名单只负责「谁给什么」，这份清单负责
// 「顾问永远碰不到什么」。凡是会改 finding / intent / scope / 流量绑定 / 目标 /
// 约束 / 资产 / 任务 / 平台配置 / 探索图事实的工具都在这里。
var advisoryGraphWriteTools = map[string]bool{
	// 探索图与编排（agent/tools*.go）
	"add_intent": true, "add_hint": true, "steer_work": true,
	"set_goals": true, "set_constraints": true,
	"insert_assets": true, "add_company_scope": true, "add_task_scope": true,
	"report_finding": true, "record_fact": true,
	"prove_goal": true, "goal_met": true, "kill_work": true,
	// 跨任务编排（server/orchestration.go）
	"spawn_task": true, "pause_task": true, "add_task_hint": true,
	"update_finding_report": true, "bind_finding_traffic": true,
	// 平台配置（server/platform_tools.go）
	"create_skill": true, "update_skill_file": true,
	"create_custom_tool": true, "update_custom_tool": true,
	"create_mcp": true, "update_mcp": true, "delete_assets_by_host": true,
	// 漏洞复测结论（server/finding_retests.go）
	"record_finding_retest_result": true,
	// 策略库提议（agent/strategy_tools.go）：它会往【跨任务共享】的 strategies 表
	// 写一行隔离态策略 + 一条待裁决提议。归到「共享状态写」而不是「建议通道」：
	// strategies 是全平台共享的表，写进去就是写进共享状态 ⇒ 顾问 agent 默认拿不到
	// 它，要用必须显式放开。
	"strategy_propose": true,
}

// advisoryAdvisoryWriteTools 是顾问 agent 自己的建议通道工具：写的是提议表，
// 不是平台状态机，故与 advisoryGraphWriteTools 分开列。
var advisoryAdvisoryWriteTools = map[string]bool{
	"submit_decision": true, "submit_verdict": true, "submit_contradiction": true,
	"propose_strategy": true, "propose_scenario_feature": true,
	// emit_structured_intent：intent-normalizer 的唯一输出通道（结构化意图）。
	// 它写的是「规整后的动作」，不是平台状态——判定仍由确定性闸门做。
	"emit_structured_intent": true,
	// 对抗复核队列（needs_review）是 redteam 自己的通道：它读队列、结案，
	// 写的是「我对这条被拦下的调用查清没有」——不是平台共享状态机。
	//
	// 为什么【读】工具也归在写这一类：这两个工具构成同一条通道的两端
	// （读队列 → 结案），而 redteam 的策略是 DenyWrite=true。读端若被算成
	// 纯只读，读的能力面就会被别的 agent 以「只读工具」的名义顺带拿走，
	// 两条通道的两端就分属两个 agent 了。
	"list_adversarial_review_pending": true, "submit_adversarial_review_result": true,
}

// advisoryScopeQueryTools 是「查 scope」的工具 key。
//
// scope 判定属于确定性闸门（policy-gate）的职责。intent-normalizer 只做结构化
// 转换，不判范围：它一旦能查 scope，就有能力「顺手」判一个 scope 并自行放行，
// 绕过闸门——职责分离就破了。这类工具对它既不在白名单，也在黑名单里显式列出。
var advisoryScopeQueryTools = map[string]bool{
	"list_companies":    true,
	"add_task_scope":    true,
	"add_company_scope": true,
	"get_task_scope":    true,
}

// AdvisoryToolIsScopeQuery 判断某工具是否属于「查 scope」类。
func AdvisoryToolIsScopeQuery(toolKey string) bool {
	return advisoryScopeQueryTools[toolKey]
}

// advisoryExecTools 是能执行命令/发起请求的工具 key。自定义工具另按 kind 判定
// （command/script/http 一律算可执行）。
var advisoryExecTools = map[string]bool{
	"Bash": true, "bash": true,
	"shell_open": true, "shell_write": true, "shell_read": true,
	"shell_close": true, "shell_wait": true,
	"http_request": true,
}

// AdvisoryToolCapability 判定一个工具的能力面。kind 是 tools.kind 列
// （builtin / command / script / http）。判定顺序：执行 > 建议通道 > 共享状态写 > 只读。
//
// 未知工具名一律按只读处理：能不能绑给某个 agent 由白名单决定，白名单之外的工具
// 本来就绑不上，这里判成只读只是不把「没登记」误当成「有权限」。
func AdvisoryToolCapability(toolKey, toolKind string) AdvisoryToolCap {
	switch {
	case advisoryExecTools[toolKey] || toolKind == "command" || toolKind == "script" || toolKind == "http":
		return AdvisoryCapExec
	case advisoryAdvisoryWriteTools[toolKey]:
		return AdvisoryCapAdvisoryWrite
	case advisoryGraphWriteTools[toolKey]:
		return AdvisoryCapGraphWrite
	default:
		return AdvisoryCapRead
	}
}

// AdvisoryAgentToolPolicies 返回五个顾问型 agent（curator/verifier/redteam/creative/
// intent-normalizer）的工具绑定策略。
//
// Allow 里同时列了两类名字：
//   - 策略库阶段将要注册的工具（此刻尚未注册 → 协调时跳过并记录，落地后自动绑上）；
//   - 平台已有的等价只读工具（把规格里的意图映射到真实存在的 key 上，例如
//     read_finding → list_findings、read_asset_graph → list_assets + graph_overview）。
//
// 规格里给 probe/redteam 的「bash / http_request 家族」在平台上不是 tools 表的行：
// 探测能力由 SDK 的 Bash 与交互式 shell（agents 的 interactive_shell 开关）提供，
// 不经过绑定表，因此这里不给它们开可执行工具的白名单。
func AdvisoryAgentToolPolicies() []AdvisoryAgentToolPolicy {
	return []AdvisoryAgentToolPolicy{
		{
			Agent: "curator",
			Allow: []string{
				"read_strategy_library",       // 策略库读取（策略库阶段注册）
				"read_pending_contradictions", // 待处理证伪记录读取（同上）
				"run_replay_eval",             // 冻结重放评估，只读（同上）
				"submit_decision",             // 裁决写 curator_decisions：自己的建议通道
			},
			Deny:      []string{"report_finding", "add_intent"},
			DenyExec:  true,
			DenyWrite: true,
		},
		{
			Agent: "verifier",
			Allow: []string{
				"read_finding",            // 策略库阶段注册
				"read_traffic_evidence",   // 策略库阶段注册
				"read_strategy_posterior", // 策略库阶段注册
				"submit_verdict",          // 建议写 verification_verdicts：自己的建议通道
				// 平台已有的等价只读工具
				"list_findings",                                 // ← read_finding
				"get_finding_traffic",                           // ← read_traffic_evidence
				"traffic_search", "traffic_get", "traffic_blob", // ← read_traffic_evidence
			},
			Deny:      []string{"report_finding", "add_intent"},
			DenyExec:  true,
			DenyWrite: true,
		},
		{
			Agent: "redteam",
			Allow: []string{
				"submit_contradiction",    // 证伪记录写 pending_contradictions：自己的建议通道
				"read_strategy_library",   // 策略库阶段注册
				"read_strategy_posterior", // 策略库阶段注册
				"read_finding",            // 策略库阶段注册
				"read_traffic_evidence",   // 策略库阶段注册
				// 对抗复核队列（needs_review）：redteam 是这条队列的【唯一】消费者
				// ——读它、结案它。两条一起给，避免出现「只会读不会结案」或反之的
				// 半截状态。
				"list_adversarial_review_pending", "submit_adversarial_review_result",
				// 平台已有的等价只读工具
				"list_findings",                                 // ← read_finding
				"traffic_search", "traffic_get", "traffic_blob", // ← read_traffic_evidence
			},
			Deny: []string{"report_finding"},
			// 证伪者本来就要在 scope 内动手：探测能力走 SDK Bash + 交互式 shell，
			// 不在 tools 表里，所以这里不放开表内可执行工具的白名单。
			DenyExec:  false,
			DenyWrite: true,
		},
		{
			Agent: "creative",
			Allow: []string{
				"propose_strategy",         // 策略提案写 strategy_proposals：自己的建议通道
				"propose_scenario_feature", // 场景特征提案写 proposals 表：同上
				"read_strategy_library",    // 策略库阶段注册
				"read_asset_graph",         // 策略库阶段注册
				// 平台已有的等价只读工具
				"list_assets",    // ← read_asset_graph
				"graph_overview", // ← read_asset_graph（探索图锚点视角）
			},
			Deny:      []string{"report_finding"},
			DenyExec:  true,
			DenyWrite: true,
		},
		{
			// intent-normalizer（规整器）：worker 的原始动作 → 结构化意图，
			// 交给确定性闸门裁决。它是这条链上唯一新增的 LLM 环节，因此工具面
			// 收得最紧——白名单里只有一个工具，没有第二条路。
			Agent: "intent_normalizer",
			Allow: []string{
				"emit_structured_intent", // 唯一的输出通道：把原始动作转成结构化意图
			},
			Deny: []string{
				// scope 判定是确定性闸门的职责：规整器一旦能查 scope，就有能力
				// 自己判一个 scope 并放行，绕过闸门。
				"list_companies", "add_task_scope", "add_company_scope", "get_task_scope",
			},
			DenyExec:  true,
			DenyWrite: true,
		},
		{
			// 偏锋探索·方法论顾问（EDGE）：只提议——咨询方法论、查判据进度、轻量
			// 咨询、发起完整运行。下发/收尾/换方法论是执行体（edge_exec）的职责，
			// 顾问不碰。主 agent 默认不绑 edge_run（任务内角色不建新任务，见
			// server/edge_tools.go 的绑定注释）；白名单外一律绑不上。
			Agent: "edge",
			Allow: []string{
				"edge_methodologies", // 方法论注册表读取（只读）
				"edge_coverage",      // 格点覆盖与判据进度（只读）
				"edge_consult",       // 轻量咨询（留痕，不建任务不落图）
				"edge_run",           // 发起完整偏锋任务（建任务 + 启动 edge_exec）
			},
			Deny:      []string{"report_finding", "add_intent", "add_task_scope", "add_company_scope"},
			DenyExec:  true,
			DenyWrite: true,
		},
		{
			// 偏锋探索·执行体（EDGE）：总控轮——查进度、下发假设、判完成、封格、
			// 换方法论。判定/封格/收尾是确定性层职责的编排入口，不是共享状态写
			// （判据账本只它自己写）。报告漏洞与直接派生常规意图不在其职责内——
			// 下发走 edge_dispatch（带溯因门禁），不是 add_intent。
			Agent: "edge_exec",
			Allow: []string{
				"edge_methodologies", // 方法论注册表读取（只读）
				"edge_coverage",      // 格点覆盖与判据进度（只读）
				"edge_dispatch",      // 下发一条偏锋假设到指定格点
				"edge_conclude",      // 判据成立后收尾并写 stop_reason
				"edge_switch",        // 换方法论、封格（熔断动作）
			},
			Deny:      []string{"report_finding", "add_intent", "add_task_scope", "add_company_scope"},
			DenyExec:  true,
			DenyWrite: true,
		},
	}
}

// AdvisoryAgentToolPolicyFor 返回某个 agent 的绑定策略；不是顾问 agent 则返回 false。
func AdvisoryAgentToolPolicyFor(agentKey string) (AdvisoryAgentToolPolicy, bool) {
	for _, p := range AdvisoryAgentToolPolicies() {
		if p.Agent == agentKey {
			return p, true
		}
	}
	return AdvisoryAgentToolPolicy{}, false
}

// AdvisoryAgentToolKeys 返回某个顾问型 agent 被授权的工具 key 列表（白名单原序，
// 已剔除被拒绝的项）。
//
// 「授权」与「生效」是两件事，两个视图都要看得见：授权是策略层的白名单，生效是
// tools 表里真正绑上的行——白名单里尚未注册的工具在协调时跳过并记录，等注册后
// 自动绑上。只看生效会以为「没授权」，只看授权会以为「已经有工具」。
func AdvisoryAgentToolKeys(agentKey string) []string {
	p, ok := AdvisoryAgentToolPolicyFor(agentKey)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(p.Allow))
	for _, k := range p.Allow {
		if p.Allows(k, "builtin") {
			out = append(out, k)
		}
	}
	return out
}

// Allows 判断某工具是否允许绑给该 agent：先过黑名单与能力闸，再看白名单。
func (p AdvisoryAgentToolPolicy) Allows(toolKey, toolKind string) bool {
	if p.Denies(toolKey, toolKind) {
		return false
	}
	for _, k := range p.Allow {
		if k == toolKey {
			return true
		}
	}
	return false
}

// Denies 判断工具是否被策略硬性拒绝。黑名单与「共享状态写」优先于白名单：拒绝优先
// 是本策略的核心不变量——白名单是授权，黑名单是撤销，撤销永远压过授权。
func (p AdvisoryAgentToolPolicy) Denies(toolKey, toolKind string) bool {
	for _, k := range p.Deny {
		if k == toolKey {
			return true
		}
	}
	switch AdvisoryToolCapability(toolKey, toolKind) {
	case AdvisoryCapGraphWrite:
		return p.DenyWrite
	case AdvisoryCapExec:
		return p.DenyExec
	}
	return false
}

// AdvisoryToolBindingAllowed 判断 agentKey 是否允许绑定 toolKey（kind 为 tools.kind）。
// 不是顾问 agent 一律返回 false：白名单之外的 agent 不碰顾问工具。
func AdvisoryToolBindingAllowed(agentKey, toolKey, toolKind string) bool {
	p, ok := AdvisoryAgentToolPolicyFor(agentKey)
	if !ok {
		return false
	}
	return p.Allows(toolKey, toolKind)
}

// seedAdvisoryAgentToolBindings 把顾问 agent 的工具绑定策略落实到 tools 表：
//   - 白名单里的工具若已有行 → 绑上（老库补绑；新库在播种时就已绑好）。
//   - 违反策略硬拒绝的工具 → 解绑。用户在工具管理页误把 report_finding 绑给
//     verifier，也会在下一次启动被收回：拒绝是确定性层的职责，不该由 UI 说了算。
//   - 白名单里尚未注册的工具 → 跳过并记录，等策略库阶段落地后自动生效。
//
// 只动这几个 agent 自己的绑定项，别的 agent 在同一数组里的条目一律不碰（纯追加）。
func (d *DB) seedAdvisoryAgentToolBindings() error {
	rows, err := d.ListTools()
	if err != nil {
		return err
	}
	byKey := make(map[string]*Tool, len(rows))
	for _, t := range rows {
		byKey[t.Key] = t
	}
	for _, p := range AdvisoryAgentToolPolicies() {
		for _, key := range p.Allow {
			row, ok := byKey[key]
			if !ok {
				log.Printf("[tools] 顾问 agent %s 允许的工具 %s 此刻尚未在 tools 表注册（策略库阶段未落地，或由服务端稍后播种），注册后自动绑定", p.Agent, key)
				continue
			}
			if containsAgentKey(row.Agents, p.Agent) {
				continue
			}
			if err := d.AddAgentToToolBinding(p.Agent, []string{key}); err != nil {
				return fmt.Errorf("bind %s→%s: %w", key, p.Agent, err)
			}
		}
		for _, row := range rows {
			if !containsAgentKey(row.Agents, p.Agent) {
				continue
			}
			if !p.Denies(row.Key, row.Kind) {
				continue
			}
			if err := d.RemoveAgentFromTool(p.Agent, row.Key); err != nil {
				return fmt.Errorf("unbind %s→%s: %w", row.Key, p.Agent, err)
			}
		}
	}
	return nil
}

// containsAgentKey 判断 agent key 数组里是否含某个 key。
func containsAgentKey(keys []string, want string) bool {
	for _, k := range keys {
		if k == want {
			return true
		}
	}
	return false
}
