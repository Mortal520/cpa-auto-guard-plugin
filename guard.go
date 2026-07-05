package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// accountState describes the plugin's view of one auth account.
type accountState struct {
	AuthIndex     string `json:"auth_index"`
	FileName      string `json:"file_name,omitempty"`
	Provider      string `json:"provider,omitempty"`
	Account       string `json:"account,omitempty"`
	State         string `json:"state"`
	Reason        string `json:"reason,omitempty"`
	DisabledAtMS  int64  `json:"disabled_at_ms,omitempty"`
	RecoverAtMS   int64  `json:"recover_at_ms,omitempty"`
	RetryCount    int    `json:"retry_count,omitempty"`
	LastUsageMS   int64  `json:"last_usage_ms,omitempty"`
	LastProbeMS   int64  `json:"last_probe_ms,omitempty"`
	LastProbeOK   *bool  `json:"last_probe_ok,omitempty"`
	LastProbeCode *int   `json:"last_probe_code,omitempty"`
	UpdatedMS     int64  `json:"updated_ms"`
}

// guardConfig holds the runtime configuration the host pushed in.
type guardConfig struct {
	Enabled                 bool    `json:"enabled"`
	TickSeconds             float64 `json:"tick_seconds"`
	MaxResetSeconds         float64 `json:"max_reset_seconds"`
	DeleteThreshold         int     `json:"delete_threshold"`
	AutoDisableQuota        bool    `json:"auto_disable_quota"`
	AutoRecover             bool    `json:"auto_recover"`
	AutoDeleteRequestFailed bool    `json:"auto_delete_request_failed"`
	ProbeURL                string  `json:"probe_url"`
	ProbeTimeoutMS          int     `json:"probe_timeout_ms"`
	RecoverGraceSeconds     float64 `json:"recover_grace_seconds"`
	MaxStuckRetries         int     `json:"max_stuck_retries"`
}

// logEntry is one line in the in-memory log ring.
type logEntry struct {
	AtMS    int64  `json:"at_ms"`
	Level   string `json:"level"`
	AuthIdx string `json:"auth_index,omitempty"`
	Account string `json:"account,omitempty"`
	Message string `json:"message"`
}

// guardState is the singleton holding accounts, logs, config and ticker.
type guardState struct {
	mu       sync.Mutex
	cfg      guardConfig
	accounts map[string]*accountState
	logs     []logEntry
	logSeq   int64
	lastTick int64
}

const (
	stateActive           = "active"
	stateDisabledQuota    = "disabled_quota"
	stateRequestFailed    = "request_failed_probe"
	stateDisabledStuck    = "disabled_stuck"
	stateDeleted          = "deleted"
	stateSkippedExternal  = "skipped_external_disabled"
	logRingCapacity       = 500
	stateFileUnknownFlag  = "unknown"
)

var (
	guardOnce sync.Once
	guardInst *guardState
)

// guard returns the plugin singleton.
func guard() *guardState {
	guardOnce.Do(func() {
		guardInst = &guardState{
			accounts: make(map[string]*accountState),
			logs:     make([]logEntry, 0, logRingCapacity),
		}
		guardInst.cfg = configDefaults()
	})
	return guardInst
}

// applyConfig replaces the runtime config. It is safe to call from register
// or reconfigure.
func (g *guardState) applyConfig(cfg guardConfig) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if cfg.TickSeconds < 5 {
		cfg.TickSeconds = 5
	}
	if cfg.MaxResetSeconds <= 0 {
		cfg.MaxResetSeconds = 18000
	}
	if cfg.DeleteThreshold <= 0 {
		cfg.DeleteThreshold = 3
	}
	if cfg.ProbeTimeoutMS <= 0 {
		cfg.ProbeTimeoutMS = 15000
	}
	if cfg.RecoverGraceSeconds < 0 {
		cfg.RecoverGraceSeconds = 0
	}
	if cfg.MaxStuckRetries <= 0 {
		cfg.MaxStuckRetries = 5
	}
	g.cfg = cfg
}

// configSnapshot returns a copy of the current config for status output.
func (g *guardState) configSnapshot() guardConfig {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cfg
}

// pushLog appends to the in-memory log ring. Strings are stored verbatim;
// callers must redact any token/key/cookie values.
func (g *guardState) pushLog(level, authIdx, account, message string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.logSeq++
	entry := logEntry{
		AtMS:    time.Now().UnixMilli(),
		Level:   level,
		AuthIdx: authIdx,
		Account: account,
		Message: message,
	}
	g.logs = append(g.logs, entry)
	if len(g.logs) > logRingCapacity {
		// drop oldest, keep ring bounded
		g.logs = g.logs[len(g.logs)-logRingCapacity:]
	}
}

// logsSince returns log entries with AtMS strictly greater than since.
func (g *guardState) logsSince(sinceMs int64) []logEntry {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]logEntry, 0)
	for _, entry := range g.logs {
		if entry.AtMS > sinceMs {
			out = append(out, entry)
		}
	}
	return out
}

func (g *guardState) clearLogs() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.logs = g.logs[:0]
}

// snapshot returns a deep copy of accounts for status output.
func (g *guardState) snapshot() map[string]*accountState {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]*accountState, len(g.accounts))
	for k, v := range g.accounts {
		copy := *v
		out[k] = &copy
	}
	return out
}

// lookupAccount returns the state record, creating a fresh one if absent.
func (g *guardState) lookupAccount(authIndex string) *accountState {
	g.mu.Lock()
	defer g.mu.Unlock()
	rec, ok := g.accounts[authIndex]
	if !ok {
		rec = &accountState{AuthIndex: authIndex, State: stateActive}
		g.accounts[authIndex] = rec
	}
	return rec
}

// updateAccount applies an updater fn while holding the lock.
func (g *guardState) updateAccount(authIndex string, fn func(*accountState)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	rec, ok := g.accounts[authIndex]
	if !ok {
		rec = &accountState{AuthIndex: authIndex, State: stateActive}
		g.accounts[authIndex] = rec
	}
	fn(rec)
	rec.UpdatedMS = time.Now().UnixMilli()
}

// recordUsage merges a usage.handle record into the state machine. This is
// the real-time signal path; probing and ticks are layered on top.
func (g *guardState) recordUsage(rec usageEvent) {
	cfg := g.configSnapshot()
	if !cfg.Enabled {
		return
	}
	authIndex := strings.TrimSpace(rec.AuthIndex)
	if authIndex == "" || strings.EqualFold(rec.Provider, "codex") == false && rec.Provider != "" && !isCodexLikeProvider(rec.Provider) {
		// Only auto-manage codex-class accounts to avoid touching other providers.
		return
	}
	if !isCodexLikeProvider(rec.Provider) {
		return
	}
	g.updateAccount(authIndex, func(a *accountState) {
		a.LastUsageMS = time.Now().UnixMilli()
	})
	// Failed signals are the trigger. Success resets retry counters but does
	// not auto-enable (enable needs an explicit probe to avoid fake recoveries).
	if !rec.Failed {
		return
	}
	status := rec.StatusCode
	if status == 429 || rec.LimitReached {
		g.onQuotaReached(authIndex, rec)
	} else if status >= 500 || status == 0 || status == 401 || strings.Contains(strings.ToLower(rec.FailureBody), "request failed") {
		g.onRequestFailed(authIndex, rec)
	}
}

// onQuotaReached handles a 429 usage_limit_reached usage event.
func (g *guardState) onQuotaReached(authIndex string, rec usageEvent) {
	cfg := g.configSnapshot()
	if !cfg.AutoDisableQuota {
		g.pushLog("info", authIndex, rec.Account, "限额信号收到但自动禁用已关闭")
		return
	}
	resetAt := rec.ResetAtMS
	if resetAt <= 0 {
		// No explicit reset time: do not blind-disable, just record.
		g.pushLog("warn", authIndex, rec.Account, "限额信号缺少重置时间，仅记录不动作")
		g.updateAccount(authIndex, func(a *accountState) {
			a.Reason = "quota_no_reset_time"
		})
		return
	}
	now := time.Now().UnixMilli()
	resetSec := float64(resetAt-now) / 1000.0
	if resetSec > cfg.MaxResetSeconds {
		g.pushLog("warn", authIndex, rec.Account, fmt.Sprintf("重置时间过长 (%.0fs)，仅记录不动作", resetSec))
		g.updateAccount(authIndex, func(a *accountState) {
			a.Reason = "quota_reset_too_far"
			a.RecoverAtMS = resetAt
		})
		return
	}
	if _, err := setAuthDisabled(authIndex, true); err != nil {
		g.pushLog("error", authIndex, rec.Account, fmt.Sprintf("限额禁用失败: %v", err))
		return
	}
	g.updateAccount(authIndex, func(a *accountState) {
		a.State = stateDisabledQuota
		a.Reason = rec.Reason
		a.DisabledAtMS = now
		a.RecoverAtMS = resetAt
		a.LastProbeCode = nil
		a.LastProbeOK = nil
	})
	g.pushLog("warn", authIndex, rec.Account, fmt.Sprintf("已禁用(限额)，将在 %s 后探测恢复", humanizeMS(resetAt-now)))
	hostLog("warn", fmt.Sprintf("cpa-auto-guard: 禁用限额账号 %s, 重置于 %s", describeAccount(authIndex, rec.Account), humanizeMS(resetAt-now)))
}

// onRequestFailed handles a request-failed usage event (network/5xx/401).
func (g *guardState) onRequestFailed(authIndex string, rec usageEvent) {
	cfg := g.configSnapshot()
	if !cfg.AutoDeleteRequestFailed {
		g.pushLog("info", authIndex, rec.Account, "request_failed 信号收到但自动删除已关闭")
		return
	}
	// Increment retry counter and, if threshold reached, probe before deletion.
	g.updateAccount(authIndex, func(a *accountState) {
		if a.State != stateRequestFailed {
			a.State = stateRequestFailed
			a.RetryCount = 0
		}
		a.RetryCount++
		a.Reason = rec.Reason
	})
	rec2 := g.snapshot()[authIndex]
	if rec2 == nil {
		return
	}
	if rec2.RetryCount < cfg.DeleteThreshold {
		g.pushLog("warn", authIndex, rec.Account, fmt.Sprintf("request_failed 计数 %d/%d", rec2.RetryCount, cfg.DeleteThreshold))
		return
	}
	// Re-probe: if it is actually a quota limit, move to disabled flow.
	g.pushLog("warn", authIndex, rec.Account, fmt.Sprintf("request_failed 达阈值 %d，开始重查额度", cfg.DeleteThreshold))
	outcome, err := g.probeAccount(authIndex, rec.Account)
	if err != nil {
		g.pushLog("error", authIndex, rec.Account, fmt.Sprintf("重查额度探测失败: %v", err))
		g.updateAccount(authIndex, func(a *accountState) { a.RetryCount++ })
		return
	}
	switch outcome.kind {
	case probeQuota:
		g.pushLog("warn", authIndex, rec.Account, "重查额度确认为限额，转禁用流程")
		g.onQuotaReached(authIndex, usageEvent{
			AuthIndex:   authIndex,
			Account:     rec.Account,
			Provider:    rec.Provider,
			Failed:      true,
			StatusCode:  outcome.statusCode,
			LimitReached: true,
			ResetAtMS:   outcome.resetAtMS,
			Reason:      "probe_quota",
		})
	case probeOK:
		// Recovered on probe: reset retry count, keep disabled state unchanged.
		g.updateAccount(authIndex, func(a *accountState) {
			a.RetryCount = 0
		})
		g.pushLog("info", authIndex, rec.Account, "重查额度健康，重置失败计数")
	case probeFailed:
		if rec2.RetryCount >= cfg.DeleteThreshold {
			g.pushLog("error", authIndex, rec.Account, fmt.Sprintf("连续失败 %d 次且探测仍失败，删除账号", rec2.RetryCount))
			if err := g.deleteAccount(authIndex, rec.Account); err != nil {
				g.pushLog("error", authIndex, rec.Account, fmt.Sprintf("删除账号失败: %v", err))
			}
		}
	}
}

// tick runs a single maintenance pass: recover due cooldowns, prune deleted.
func (g *guardState) tick() {
	cfg := g.configSnapshot()
	if !cfg.Enabled {
		return
	}
	g.mu.Lock()
	g.lastTick = time.Now().UnixMilli()
	accounts := make([]*accountState, 0, len(g.accounts))
	for _, a := range g.accounts {
		copy := *a
		accounts = append(accounts, &copy)
	}
	g.mu.Unlock()

	now := time.Now().UnixMilli()
	for _, a := range accounts {
		if a.State == stateDisabledQuota && cfg.AutoRecover && a.RecoverAtMS > 0 {
			graceMS := int64(cfg.RecoverGraceSeconds * 1000)
			if now+graceMS < a.RecoverAtMS {
				continue // not due yet
			}
			g.recoverProbe(a, cfg)
		}
	}
}

// recoverProbe probes one due cooldown and re-enables if healthy.
func (g *guardState) recoverProbe(a *accountState, cfg guardConfig) {
	outcome, err := g.probeAccount(a.AuthIndex, a.Account)
	if err != nil {
		g.pushLog("error", a.AuthIndex, a.Account, fmt.Sprintf("恢复探测失败: %v", err))
		g.updateAccount(a.AuthIndex, func(s *accountState) {
			s.LastProbeMS = time.Now().UnixMilli()
			s.RetryCount++
		})
		return
	}
	switch outcome.kind {
	case probeOK:
		if _, err := setAuthDisabled(a.AuthIndex, false); err != nil {
			g.pushLog("error", a.AuthIndex, a.Account, fmt.Sprintf("恢复启用失败: %v", err))
			return
		}
		g.updateAccount(a.AuthIndex, func(s *accountState) {
			ok := true
			s.State = stateActive
			s.DisabledAtMS = 0
			s.RecoverAtMS = 0
			s.RetryCount = 0
			s.LastProbeMS = time.Now().UnixMilli()
			s.LastProbeOK = &ok
			code := outcome.statusCode
			s.LastProbeCode = &code
		})
		g.pushLog("info", a.AuthIndex, a.Account, "恢复探测健康，已重新启用")
		hostLog("info", fmt.Sprintf("cpa-auto-guard: 恢复启用账号 %s", describeAccount(a.AuthIndex, a.Account)))
	case probeQuota:
		g.updateAccount(a.AuthIndex, func(s *accountState) {
			s.RecoverAtMS = outcome.resetAtMS
			s.LastProbeMS = time.Now().UnixMilli()
			code := outcome.statusCode
			s.LastProbeCode = &code
			f := false
			s.LastProbeOK = &f
			s.RetryCount = 0
		})
		g.pushLog("warn", a.AuthIndex, a.Account, fmt.Sprintf("恢复探测仍限额，重置到期至 %s", humanizeMS(outcome.resetAtMS-time.Now().UnixMilli())))
	case probeFailed:
		g.updateAccount(a.AuthIndex, func(s *accountState) {
			s.RetryCount++
			s.LastProbeMS = time.Now().UnixMilli()
			code := outcome.statusCode
			s.LastProbeCode = &code
			f := false
			s.LastProbeOK = &f
			if s.RetryCount >= cfg.MaxStuckRetries {
				s.State = stateDisabledStuck
			}
		})
		g.pushLog("warn", a.AuthIndex, a.Account, fmt.Sprintf("恢复探测失败 retry=%d/%d", g.snapshot()[a.AuthIndex].RetryCount, cfg.MaxStuckRetries))
	}
}

// probeKind classifies one probe outcome.
type probeKind int

const (
	probeOK probeKind = iota
	probeQuota
	probeFailed
)

type probeResult struct {
	kind        probeKind
	statusCode  int
	resetAtMS   int64
	usedPercent *float64
}

// probeAccount re-queries the upstream quota endpoint for one auth file.
func (g *guardState) probeAccount(authIndex, account string) (probeResult, error) {
	cfg := g.configSnapshot()
	get, err := hostAuthGet(authIndex)
	if err != nil {
		return probeResult{}, err
	}
	token, accID := extractTokenAndAccountID(get.JSON)
	headers := http.Header{}
	headers.Set("User-Agent", "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal")
	if accID != "" {
		headers.Set("Chatgpt-Account-Id", accID)
	}
	resp, err := probeUpstream(cfg.ProbeURL, token, headers)
	if err != nil {
		return probeResult{kind: probeFailed}, nil
	}
	return classifyProbe(resp.StatusCode, resp.Body), nil
}

// classifyProbe inspects a usage endpoint response and returns the result.
func classifyProbe(statusCode int, body []byte) probeResult {
	bodyLower := strings.ToLower(string(body))
	if statusCode == 429 || strings.Contains(bodyLower, "usage_limit_reached") || strings.Contains(bodyLower, "limit reached") {
		reset := extractResetAt(body, time.Now())
		return probeResult{kind: probeQuota, statusCode: statusCode, resetAtMS: reset}
	}
	if statusCode >= 200 && statusCode < 300 {
		return probeResult{kind: probeOK, statusCode: statusCode}
	}
	if statusCode == 402 && strings.Contains(bodyLower, "workspace") {
		// Deactivated workspace: treat as failed (will not auto-delete unless threshold hit).
		return probeResult{kind: probeFailed, statusCode: statusCode}
	}
	return probeResult{kind: probeFailed, statusCode: statusCode}
}

// extractResetAt parses reset timestamps from a quota response.
func extractResetAt(body []byte, now time.Time) int64 {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0
	}
	// Look for resets_at (unix ms) or resets_in_seconds (relative).
	if v, ok := parsed["resets_at"]; ok {
		switch t := v.(type) {
		case float64:
			if t > 1e12 {
				return int64(t)
			}
			if t > 1e9 {
				return int64(t) * 1000
			}
		case string:
			if ts, err := strconv.ParseFloat(t, 64); err == nil {
				if ts > 1e12 {
					return int64(ts)
				}
				if ts > 1e9 {
					return int64(ts) * 1000
				}
			}
		}
	}
	if v, ok := parsed["resets_in_seconds"]; ok {
		switch t := v.(type) {
		case float64:
			return now.UnixMilli() + int64(t*1000)
		case string:
			if ts, err := strconv.ParseFloat(t, 64); err == nil {
				return now.UnixMilli() + int64(ts*1000)
			}
		}
	}
	// Dig into error sub-object.
	if errObj, ok := parsed["error"].(map[string]any); ok {
		if v, ok := errObj["resets_at"]; ok {
			if ts, ok := v.(float64); ok && ts > 1e12 {
				return int64(ts)
			}
		}
		if v, ok := errObj["resets_in_seconds"]; ok {
			if ts, ok := v.(float64); ok {
				return now.UnixMilli() + int64(ts*1000)
			}
		}
	}
	return 0
}

// extractTokenAndAccountID reads a physical auth JSON and returns a usable
// bearer token plus the ChatGPT account id header (if present).
func extractTokenAndAccountID(rawJSON json.RawMessage) (string, string) {
	var parsed map[string]any
	if err := json.Unmarshal(rawJSON, &parsed); err != nil {
		return "", ""
	}
	token := firstNonEmptyStr(parsed, "access_token", "id_token", "api_key")
	accID := firstNonEmptyStr(parsed, "account_id", "chatgpt_account_id", "id")
	return token, accID
}

// deleteAccount removes the auth file by overwriting it with disabled=true and
// then deleting via host.auth.save of a tombstone. CPA does not expose a
// dedicated delete RPC for plugins, so we mark the file as disabled and log;
// the panel or the user must clean up the file. If the backend supports a
// "removed via management api" status message we adopt that convention.
func (g *guardState) deleteAccount(authIndex, account string) error {
	get, err := hostAuthGet(authIndex)
	if err != nil {
		return err
	}
	var current map[string]any
	if err := json.Unmarshal(get.JSON, &current); err != nil {
		return err
	}
	current["disabled"] = true
	current["status_message"] = "removed via management api"
	current["note"] = "cpa-auto-guard: request_failed delete"
	newJSON, err := json.Marshal(current)
	if err != nil {
		return err
	}
	if _, err := hostAuthSave(get.Name, newJSON); err != nil {
		return err
	}
	g.updateAccount(authIndex, func(a *accountState) {
		a.State = stateDeleted
		a.Reason = "request_failed_delete"
	})
	g.pushLog("error", authIndex, account, "账号已标记删除 (disabled + status_message)")
	hostLog("error", fmt.Sprintf("cpa-auto-guard: 删除账号 %s (%s)", describeAccount(authIndex, account), authIndex))
	return nil
}

// shutdown is invoked when the plugin is unloaded.
func (g *guardState) shutdown() {}

// isCodexLikeProvider filters non-codex accounts from auto-management.
func isCodexLikeProvider(provider string) bool {
	p := strings.ToLower(strings.TrimSpace(provider))
	if p == "" {
		return false
	}
	return p == "codex" || strings.Contains(p, "codex") || strings.Contains(p, "chatgpt")
}

func firstNonEmptyStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return ""
}

func describeAccount(authIndex, account string) string {
	if a := strings.TrimSpace(account); a != "" {
		return a
	}
	return authIndex
}

func humanizeMS(ms int64) string {
	if ms <= 0 {
		return "已到期"
	}
	sec := ms / 1000
	switch {
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm %ds", sec/60, sec%60)
	default:
		return fmt.Sprintf("%dh %dm", sec/3600, (sec%3600)/60)
	}
}
