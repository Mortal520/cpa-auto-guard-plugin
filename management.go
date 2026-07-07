package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)
// managementRequest mirrors the request the host delivers to management.handle.
type managementRequest struct {
	Method         string      `json:"Method"`
	Path           string      `json:"Path"`
	Headers        http.Header `json:"Headers"`
	Query          url.Values  `json:"Query"`
	Body           []byte      `json:"Body"`
	HostCallbackID string      `json:"host_callback_id,omitempty"`
}

// managementResponse mirrors the response expected by the host.
type managementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

// handleManagement dispatches a single management request to a route or resource.
func handleManagement(raw []byte) ([]byte, error) {
	var req managementRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, fmt.Errorf("decode management request: %w", err)
		}
	}
	// The host delivers the full request path: either /v0/management/cpa-auto-guard/<action>
	// for authenticated API calls, or /v0/resource/plugins/cpa-auto-guard/<sub> for
	// unauthenticated browser resource pages. We normalise both.
	path := strings.TrimSpace(req.Path)
	const resourcePrefix = "/v0/resource/plugins/" + pluginID + "/"
	if strings.HasPrefix(path, resourcePrefix) {
		return serveResource(req, strings.TrimPrefix(path, resourcePrefix))
	}
	const mgmtPrefix = "/v0/management/" + pluginID + "/"
	if strings.HasPrefix(path, mgmtPrefix) {
		return dispatchAPI(req, strings.TrimPrefix(path, mgmtPrefix))
	}
	// Legacy fallback: some hosts may deliver the path already stripped.
	if strings.HasPrefix(path, "/cpa-auto-guard/") {
		return dispatchAPI(req, strings.TrimPrefix(path, "/cpa-auto-guard/"))
	}
	return okEnvelope(managementResponse{
		StatusCode: http.StatusNotFound,
		Headers:    http.Header{"content-type": []string{"text/plain; charset=utf-8"}},
		Body:       []byte("not found"),
	})
}

// serveResource renders the HTML console for any resource sub-path. The page
// is a self-contained SPA, so /index.html, /logs.html, /view, /about all share
// the same shell; client-side JS decides which view to render.
func serveResource(req managementRequest, sub string) ([]byte, error) {
	src := strings.TrimRight(strings.TrimSpace(sub), "/")
	if src == "" {
		src = "index.html"
	}
	return okEnvelope(managementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"content-type": []string{"text/html; charset=utf-8"}},
		Body:       renderConsole(req),
	})
}

// dispatchAPI routes one Management API call to a handler.
func dispatchAPI(req managementRequest, action string) ([]byte, error) {
	switch action {
	case "state":
		return stateResponse(req)
	case "accounts":
		return accountsResponse(req)
	case "logs":
		return logsResponse(req)
	case "logs/clear":
		return clearLogsResponse(req)
	case "run":
		return runResponse(req)
	case "toggle":
		return toggleResponse(req)
	case "recover":
		return recoverResponse(req)
	case "delete":
		return deleteResponse(req)
	case "inject":
		return injectResponse(req)
	case "settings":
		return configResponse(req)
	default:
		return okEnvelope(managementResponse{
			StatusCode: http.StatusNotFound,
			Headers:    http.Header{"content-type": []string{"text/plain; charset=utf-8"}},
			Body:       []byte("unknown route: " + action),
		})
	}
}

func stateResponse(req managementRequest) ([]byte, error) {
	g := guard()
	cfg := g.configSnapshot()
	accounts := g.snapshot()
	quotaCount, recoverDue, stuckCount := 0, 0, 0
	now := time.Now().UnixMilli()
	for _, a := range accounts {
		switch a.State {
		case stateDisabledQuota:
			quotaCount++
			if a.RecoverAtMS > 0 && now >= a.RecoverAtMS {
				recoverDue++
			}
		case stateDisabledStuck:
			stuckCount++
		}
	}
	state := map[string]any{
		"plugin_id":      pluginID,
		"version":        pluginVer,
		"enabled":        cfg.Enabled,
		"config":         cfg,
		"now_ms":         now,
		"last_tick_ms":   g.lastTick,
		"accounts":       accounts,
		"summary": map[string]any{
			"total":         len(accounts),
			"disabled_quota": quotaCount,
			"recover_due":   recoverDue,
			"disabled_stuck": stuckCount,
		},
	}
	return jsonResponse(state)
}

func accountsResponse(req managementRequest) ([]byte, error) {
	// Merge live list with internal state so the panel always reflects reality.
	files, _ := hostAuthList()
	internal := guard().snapshot()
	// host.auth.list may return empty on some hosts; fall back to the CPA
	// management API (when configured) to enumerate the full auth set, then to
	// internal state so usage-driven accounts still appear in the panel.
	if len(files) == 0 {
		cfg := guard().configSnapshot()
		if cfg.ManagementKey != "" {
			if mgmtFiles, err := mgmtAuthList(cfg); err == nil {
				for _, mf := range mgmtFiles {
					files = append(files, pluginapi.HostAuthFileEntry{
						AuthIndex:     mf.AuthIndex,
						Name:          mf.Name,
						Provider:      mf.Provider,
						Email:         mf.Email,
						Account:       mf.Account,
						Disabled:      mf.Disabled,
						Unavailable:   mf.Unavailable,
						StatusMessage: mf.StatusMessage,
					})
				}
			}
		}
	}
	if len(files) == 0 {
		for authIndex, st := range internal {
			files = append(files, pluginapi.HostAuthFileEntry{
				AuthIndex: authIndex,
				Name:      st.FileName,
				Provider:  st.Provider,
				Email:     st.Account,
			})
		}
	}
	merged := make([]map[string]any, 0, len(files))
	for _, f := range files {
		if !isCodexLikeProvider(f.Provider) && f.Provider != "" {
			continue
		}
		row := map[string]any{
			"auth_index":         f.AuthIndex,
			"name":               f.Name,
			"provider":           f.Provider,
			"account":            f.Account,
			"email":              f.Email,
			"disabled":           f.Disabled,
			"unavailable":        f.Unavailable,
			"status_message":     f.StatusMessage,
			"success":            f.Success,
			"failed":             f.Failed,
		}
		if st, ok := internal[f.AuthIndex]; ok {
			row["guard_state"] = st.State
			row["guard_reason"] = st.Reason
			row["recover_at_ms"] = st.RecoverAtMS
			row["disabled_at_ms"] = st.DisabledAtMS
			row["retry_count"] = st.RetryCount
			row["last_usage_ms"] = st.LastUsageMS
			row["last_probe_ms"] = st.LastProbeMS
		}
		merged = append(merged, row)
	}
	return jsonResponse(map[string]any{"accounts": merged})
}

func logsResponse(req managementRequest) ([]byte, error) {
	sinceMs := int64(0)
	if v := strings.TrimSpace(req.Query.Get("since")); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
			sinceMs = parsed
		}
	}
	logs := guard().logsSince(sinceMs)
	return jsonResponse(map[string]any{"logs": logs})
}

func clearLogsResponse(req managementRequest) ([]byte, error) {
	guard().clearLogs()
	return jsonResponse(map[string]any{"ok": true})
}

func runResponse(req managementRequest) ([]byte, error) {
	if !guard().configSnapshot().Enabled {
		return jsonResponse(map[string]any{"ok": false, "error": "plugin disabled"})
	}
	// Manual run: recover due cooldowns, then proactively probe every known
	// account (including those with no usage signal) so quota state is
	// discovered even without a prior inspection result.
	go func() {
		guard().tick()
		guard().probeSweep(false)
	}()
	guard().pushLog("info", "", "", "手动触发: 已调度恢复检查 + 全量额度巡检")
	return jsonResponse(map[string]any{"ok": true, "message": "tick + sweep scheduled"})
}

func toggleResponse(req managementRequest) ([]byte, error) {
	g := guard()
	cfg := g.configSnapshot()
	want := !cfg.Enabled
	if body := strings.TrimSpace(string(req.Body)); body != "" {
		var parsed struct {
			Enabled *bool `json:"enabled"`
		}
		if err := json.Unmarshal(req.Body, &parsed); err == nil && parsed.Enabled != nil {
			want = *parsed.Enabled
		}
	}
	cfg.Enabled = want
	g.applyConfig(cfg)
	g.pushLog("info", "", "", fmt.Sprintf("插件开关已切为 %v", want))
	return jsonResponse(map[string]any{"ok": true, "enabled": want})
}

// injectResponse feeds a synthetic usageEvent into the state machine. This is
// useful for operators/tests to drive the quota-disable and request_failed-delete
// flows without waiting for a real upstream failure. Requires auth_index and a
// kind: "quota" (429 limit reached), "failed" (5xx/network/401 request failed),
// or "ok" (success). Optional reset_at_ms (for quota) and reason are honored.
func injectResponse(req managementRequest) ([]byte, error) {
	if req.Method != http.MethodPost {
		return jsonResponse(map[string]any{"ok": false, "error": "POST required"})
	}
	var parsed struct {
		AuthIndex string `json:"auth_index"`
		Kind      string `json:"kind"`
		StatusCode int   `json:"status_code"`
		ResetAtMS  int64 `json:"reset_at_ms"`
		Reason     string `json:"reason"`
	}
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &parsed)
	}
	authIndex := strings.TrimSpace(parsed.AuthIndex)
	if authIndex == "" {
		return jsonResponse(map[string]any{"ok": false, "error": "auth_index required"})
	}
	kind := strings.ToLower(strings.TrimSpace(parsed.Kind))
	if kind == "" {
		kind = "failed"
	}
	// Resolve account name from internal state if present.
	account := ""
	if v := guard().snapshot()[authIndex]; v != nil {
		account = v.Account
	}
	ev := usageEvent{AuthIndex: authIndex, Account: account, Provider: "codex"}
	switch kind {
	case "quota", "429":
		ev.Failed = true
		ev.StatusCode = 429
		ev.LimitReached = true
		if parsed.StatusCode == 429 || parsed.StatusCode == 0 {
			ev.StatusCode = 429
		} else {
			ev.StatusCode = parsed.StatusCode
		}
		ev.ResetAtMS = parsed.ResetAtMS
		if ev.ResetAtMS == 0 {
			// default 5-minute window so the recover path is exercisable
			ev.ResetAtMS = time.Now().UnixMilli() + 5*60*1000
		}
		if parsed.Reason != "" {
			ev.Reason = parsed.Reason
		} else {
			ev.Reason = "usage_limit_reached"
		}
	case "failed", "error":
		ev.Failed = true
		if parsed.StatusCode == 0 {
			// default 401 to exercise the auth-failure path
			ev.StatusCode = 401
		} else {
			ev.StatusCode = parsed.StatusCode
		}
		if parsed.Reason != "" {
			ev.Reason = parsed.Reason
		} else {
			ev.Reason = "request failed"
		}
	case "ok":
		ev.Failed = false
		ev.StatusCode = 200
	default:
		return jsonResponse(map[string]any{"ok": false, "error": "kind must be quota/failed/ok"})
	}
	guard().recordUsage(ev)
	guard().pushLog("info", authIndex, account, fmt.Sprintf("注入事件: kind=%s status=%d reset_at_ms=%d", kind, ev.StatusCode, ev.ResetAtMS))
	return jsonResponse(map[string]any{"ok": true, "injected": kind, "status_code": ev.StatusCode})
}

func recoverResponse(req managementRequest) ([]byte, error) {
	authIndex := strings.TrimSpace(req.Query.Get("auth_index"))
	if authIndex == "" && len(req.Body) > 0 {
		var parsed struct {
			AuthIndex string `json:"auth_index"`
		}
		_ = json.Unmarshal(req.Body, &parsed)
		authIndex = strings.TrimSpace(parsed.AuthIndex)
	}
	if authIndex == "" {
		return jsonResponse(map[string]any{"ok": false, "error": "auth_index required"})
	}
	internal := guard().snapshot()
	rec, ok := internal[authIndex]
	if !ok {
		return jsonResponse(map[string]any{"ok": false, "error": "no state for auth_index"})
	}
	cfg := guard().configSnapshot()
	go guard().recoverProbe(rec, cfg)
	return jsonResponse(map[string]any{"ok": true, "message": "recover probe scheduled"})
}

func configResponse(req managementRequest) ([]byte, error) {
	g := guard()
	cfg := g.configSnapshot()
	if req.Method == http.MethodPost {
		var parsed struct {
			ManagementURL *string `json:"management_url,omitempty"`
			ManagementKey *string `json:"management_key,omitempty"`
			ProxyURL      *string `json:"proxy_url,omitempty"`
		}
		if len(req.Body) > 0 {
			_ = json.Unmarshal(req.Body, &parsed)
		}
		if parsed.ManagementURL != nil {
			cfg.ManagementURL = strings.TrimSpace(*parsed.ManagementURL)
		}
		if parsed.ManagementKey != nil {
			mk := strings.TrimSpace(*parsed.ManagementKey)
			// An empty string preserves the existing key when the UI sends a
			// masked placeholder; a non-empty value replaces it.
			if mk != "" {
				cfg.ManagementKey = mk
			}
		}
		if parsed.ProxyURL != nil {
			pu := strings.TrimSpace(*parsed.ProxyURL)
			// Empty string clears the proxy; otherwise replace. URL may carry
			// embedded credentials and is never echoed on GET.
			cfg.ProxyURL = pu
		}
		g.applyConfig(cfg)
		g.pushLog("info", "", "", "管理 API 配置已更新")
		return jsonResponse(map[string]any{"ok": true})
	}
	// GET: return current config but never expose the management key. The UI
	// only needs to know whether one is configured.
	resp := map[string]any{
		"management_url":      cfg.ManagementURL,
		"management_key_set":  cfg.ManagementKey != "",
		"proxy_url_configured":  cfg.ProxyURL != "",
	}
	return jsonResponse(resp)
}
func deleteResponse(req managementRequest) ([]byte, error) {
	authIndex := strings.TrimSpace(req.Query.Get("auth_index"))
	if authIndex == "" && len(req.Body) > 0 {
		var parsed struct {
			AuthIndex string `json:"auth_index"`
		}
		_ = json.Unmarshal(req.Body, &parsed)
		authIndex = strings.TrimSpace(parsed.AuthIndex)
	}
	if authIndex == "" {
		return jsonResponse(map[string]any{"ok": false, "error": "auth_index required"})
	}
	account := ""
	if v := guard().snapshot()[authIndex]; v != nil {
		account = v.Account
	}
	if err := guard().deleteAccount(guard().configSnapshot(), authIndex, account); err != nil {
		return jsonResponse(map[string]any{"ok": false, "error": err.Error()})
	}
	return jsonResponse(map[string]any{"ok": true})
}

func jsonResponse(v any) ([]byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return okEnvelope(managementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"content-type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	})
}

// renderConsole renders the single-page HTML control panel. The page is
// self-contained: it fetches the Management API (served under the same
// /v0/management/cpa-auto-guard/ prefix) and updates the UI in place.

// renderConsole renders the single-page HTML control panel. The page is
// self-contained: it fetches the Management API (served under the same
// /v0/management/cpa-auto-guard/ prefix) and updates the UI in place.
// Light theme so version/status badges stay readable on a white background.
func renderConsole(req managementRequest) []byte {
	const tpl = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>cpa-auto-guard 控制台</title>
<style>
:root{--bg:#f8fafc;--card:#ffffff;--text:#0f172a;--muted:#64748b;--accent:#0284c7;--warn:#b45309;--err:#b91c1c;--ok:#047857;--border:#e2e8f0}
*{box-sizing:border-box}body{margin:0;background:linear-gradient(135deg,#eff6ff 0%,#f8fafc 60%,#f1f5f9 100%);color:var(--text);font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif;min-height:100vh;padding:1.25rem}
h1{font-size:1.25rem;margin:0 0 .5rem;display:flex;align-items:center;gap:.5rem}.badge{font-size:.72rem;padding:.18rem .55rem;border-radius:9999px;background:#e0f2fe;color:var(--accent);border:1px solid #bae6fd;font-weight:600}.badge.muted{background:#f1f5f9;color:var(--muted);border-color:var(--border)}
.grid{display:grid;gap:1rem;grid-template-columns:1fr;max-width:1100px;margin:0 auto}
.card{background:var(--card);border:1px solid var(--border);border-radius:12px;padding:1rem;box-shadow:0 1px 3px rgba(15,23,42,.06)}
.row{display:flex;gap:.75rem;flex-wrap:wrap;align-items:center}.muted{color:var(--muted)}.small{font-size:.8rem}
button{background:#f1f5f9;color:var(--text);border:1px solid var(--border);padding:.45rem .85rem;border-radius:8px;cursor:pointer;font-size:.85rem;transition:all .15s}button:hover{border-color:var(--accent);color:var(--accent);background:#e0f2fe}button.warn:hover{border-color:var(--warn);color:var(--warn);background:#fef3c7}button.err:hover{border-color:var(--err);color:var(--err);background:#fee2e2}button:disabled{opacity:.5;cursor:not-allowed}
.toggle{display:inline-flex;align-items:center;gap:.4rem}.switch{width:46px;height:26px;background:#cbd5e1;border-radius:9999px;position:relative;transition:background .2s;cursor:pointer;user-select:none}.switch.on{background:var(--ok)}.switch::after{content:"";position:absolute;width:20px;height:20px;border-radius:50%;background:#ffffff;top:3px;left:3px;transition:left .2s;box-shadow:0 1px 3px rgba(15,23,42,.25)}.switch.on::after{left:23px}
table{width:100%;border-collapse:collapse;font-size:.8rem}th,td{padding:.45rem .55rem;text-align:left;border-bottom:1px solid var(--border);color:var(--text)}th{color:var(--muted);font-weight:600}.tag{padding:.12rem .45rem;border-radius:6px;font-size:.72rem;background:#f1f5f9;color:var(--text)}.tag.q{color:var(--warn);background:#fef3c7}.tag.f{color:var(--err);background:#fee2e2}.tag.a{color:var(--ok);background:#d1fae5}.tag.s{color:var(--muted);background:#f1f5f9}
.log-card{background:var(--card)}.log-wrap{max-height:320px;overflow-y:auto;background:#0f172a;border:1px solid var(--border);border-radius:8px;padding:.6rem;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:.78rem;line-height:1.6;color:#e2e8f0}.line{display:flex;gap:.5rem;padding:.18rem 0;border-bottom:1px dashed #1e293b}.line:last-child{border-bottom:none}.t{color:#94a3b8;min-width:92px}.lvl{min-width:52px;font-weight:600}.lvl.info{color:#38bdf8}.lvl.warn{color:#fbbf24}.lvl.error{color:#f87171}.acc{color:#94a3b8;min-width:148px}.msg{flex:1;word-break:break-word;color:#e2e8f0}
a.badge{cursor:pointer;color:var(--accent) !important}.stat{display:flex;flex-direction:column;gap:.2rem}.stat .n{font-size:1.6rem;font-weight:700;color:var(--text)}.stats{display:grid;grid-template-columns:repeat(auto-fit,minmax(140px,1fr));gap:.75rem}
</style></head><body><div class="grid">
<div class="card"><div class="row" style="justify-content:space-between"><h1><span>🛡️ cpa-auto-guard</span><span id="verBadge" class="badge">v` + pluginVer + `</span><span id="enBadge" class="badge muted">加载中</span></h1>
<div class="row"><label class="toggle"><div id="switch" class="switch" role="switch" tabindex="0" aria-label="插件开关"></div><span id="switchLabel" class="small muted">关闭</span></label>
<button id="btnRun">⚡ 立即执行</button>
<button class="warn" id="btnClear">清空日志</button>
<a class="badge" href="/v0/management/plugins" target="_blank" rel="noopener">插件总览</a>
</div></div></div>
<div class="card"><div class="stats" id="stats"></div></div>
<div class="card"><div class="row" style="justify-content:space-between"><h3 style="margin:0">管理 API 配置</h3><span class="small muted" id="cfgStatus"></span></div><div id="cfgGuide" class="guide" style="display:none;flex-direction:column;gap:.4rem;margin-bottom:.6rem;padding:.6rem .8rem;background:#fef3c7;border:1px solid #fbbf24;border-radius:8px"><div style="font-weight:600;color:#92400e">未检测到 Management Key</div><div class="small" style="color:#78350f">请在下方填入与 CPA <code style="background:#fffbeb;padding:.1rem .3rem;border-radius:4px">management_key</code> 一致的密钥，点击"保存配置"后浏览器会自动同步鉴权，无需额外授权操作。</div></div><div class="row" style="margin-top:.5rem;align-items:flex-end">
<label class="small" style="display:flex;flex-direction:column;gap:.2rem;flex:1;min-width:200px">CPA 管理 API 基址<input id="cfgURL" type="text" placeholder="http://127.0.0.1:8317" style="padding:.4rem;border:1px solid var(--border);border-radius:6px;font-size:.85rem"></label>
<label class="small" style="display:flex;flex-direction:column;gap:.2rem;flex:1;min-width:200px">X-Management-Key<input id="cfgKey" type="password" placeholder="留空表示未配置" style="padding:.4rem;border:1px solid var(--border);border-radius:6px;font-size:.85rem"></label>
<label class="small" style="display:flex;flex-direction:column;gap:.2rem;flex:1;min-width:200px">代理 URL（socks5/http，含凭据）<input id="cfgProxy" type="text" placeholder="socks5://user:pass@host:port" style="padding:.4rem;border:1px solid var(--border);border-radius:6px;font-size:.85rem"></label>
<button id="btnSaveCfg">保存配置</button></div>
<div class="small muted" style="margin-top:.25rem"><span id="cfgProxyStatus">代理状态未知</span></div>
<div class="small muted" style="margin-top:.5rem">配置后插件可直接通过管理 API 拿取账号凭据进行主动探查，绕过失效的 host 回调。Key 仅存内存，不回显、不写日志。</div></div>
<div class="card"><div class="row" style="justify-content:space-between"><h3 style="margin:0">账号</h3><span class="small muted" id="accCount"></span></div><div style="overflow-x:auto"><table id="accTable"><thead><tr><th>状态</th><th>账号</th><th>file</th><th>CPA disabled</th><th>guard</th><th>重置剩余</th><th>retry</th><th>操作</th></tr></thead><tbody id="accBody"><tr><td colspan="8" class="muted">加载中…</td></tr></tbody></table></div></div>
<div class="card log-card"><div class="row" style="justify-content:space-between"><h3 style="margin:0">日志</h3><span class="small muted" id="logCount"></span></div><div class="log-wrap" id="log"><div class="muted" style="color:#94a3b8">加载中…</div></div></div>
<div class="card small muted">cpa-auto-guard 在 CPA 进程内自驱动管理 Codex 账号：限额禁用、到期探测恢复、request_failed 连续失败删除。日志仅在内存中保留最近 500 条。</div>
</div>
<script>
const API = "/v0/management/cpa-auto-guard";
let lastLogMs = 0;
// Browser requests to CPA management endpoints require X-Management-Key.
// The key is stored in localStorage and populated from the single cfgKey
// input in the settings card (saveCfg syncs it automatically). Without a
// key CPA returns 401 and the UI shows a guide banner.
function mgmtKey() { return localStorage.getItem("cpaAgKey") || ""; }
function setMgmtKey(v) { localStorage.setItem("cpaAgKey", v || ""); }
async function api(path, opts) {
  opts = opts || {};
  const hdrs = {"content-type": "application/json"};
  const k = mgmtKey();
  if (k) hdrs["X-Management-Key"] = k;
  const r = await fetch(API + "/" + path, {
    method: opts.method || "GET",
    headers: hdrs,
    body: opts.body ? JSON.stringify(opts.body) : undefined
  });
  if (r.status === 401) {
    showKeyNeeded();
    return {ok: false, error: "missing management key"};
  }
  let j;
  try { j = await r.json(); } catch (e) { return {ok: false, error: "bad json"}; }
  // CPA host may return either {ok,result} envelope or the bare payload.
  if (j && typeof j === "object" && "result" in j && "ok" in j) return j;
  return {ok: true, result: j};
}
// When CPA returns 401 or no key is stored, guide the user to the
// settings card instead of blocking. The single key input (cfgKey)
// serves both browser auth and plugin config.
function showKeyNeeded() {
  const enb = document.getElementById("enBadge");
  if (enb) { enb.textContent = "请配置 Management Key"; enb.classList.add("muted"); }
  const guide = document.getElementById("cfgGuide");
  if (guide) guide.style.display = "flex";
  const cfg = document.getElementById("cfgKey");
  if (cfg) { cfg.focus(); cfg.scrollIntoView({behavior:"smooth", block:"center"}); }
}

async function loadState() {
  const s = await api("state");
  if (!s || !s.ok) return;
  const d = s.result || {};
  const sw = document.getElementById("switch");
  sw.classList.toggle("on", !!d.enabled);
  document.getElementById("switchLabel").textContent = d.enabled ? "开启" : "关闭";
  const enb = document.getElementById("enBadge");
  enb.textContent = d.enabled ? "运行中" : "已停用";
  enb.classList.toggle("muted", !d.enabled);
  const sum = d.summary || {};
  const stats = document.getElementById("stats");
  stats.innerHTML = [
    ["账号", sum.total || 0],
    ["限额禁用", sum.disabled_quota || 0],
    ["到期待恢复", sum.recover_due || 0],
    ["长期失败", sum.disabled_stuck || 0]
  ].map(function (kv) {
    return "<div class=\"stat\"><div class=\"n\">" + kv[1] + "</div><div class=\"small muted\">" + kv[0] + "</div></div>";
  }).join("");
  document.getElementById("accCount").textContent = (d.accounts ? Object.keys(d.accounts).length : 0) + " 个内部状态";
  loadAccounts();
}
async function loadAccounts() {
  const r = await api("accounts");
  if (!r || !r.ok) return;
  const d = r.result || {};
  const acc = d.accounts || [];
  document.getElementById("accCount").textContent = acc.length + " 个账号";
  const tbody = document.getElementById("accBody");
  if (!acc.length) { tbody.innerHTML = "<tr><td colspan=\"8\" class=\"muted\">无 Codex 账号</td></tr>"; return; }
  tbody.innerHTML = acc.map(function (a) {
    const g = a.guard_state;
    const tq = stateTag(g);
    const flag = a.disabled ? "⛔" : "✅";
    const reset = a.recover_at_ms ? (a.recover_at_ms - Date.now()) : 0;
    const rest = reset > 0 ? fmtMs(reset) : (reset <= 0 && a.guard_state === "disabled_quota" ? "已到期" : "-");
    const idx = esc(a.auth_index || "");
    return "<tr><td><span class=\"tag " + tq + "\">" + stateLabel(g) + "</span></td>" +
      "<td>" + esc(a.account || a.email || "-") + "</td>" +
      "<td class=\"small muted\">" + esc(a.name || "-") + "</td>" +
      "<td>" + flag + "</td>" +
      "<td class=\"small\">" + (esc(a.status_message || "-")).slice(0, 60) + "</td>" +
      "<td class=\"small\">" + rest + "</td>" +
      "<td>" + (a.retry_count || 0) + "</td>" +
      "<td><button onclick=\"recover('" + idx + "')\">恢复</button> <button class=\"err\" onclick=\"del('" + idx + "')\">删除</button></td></tr>";
  }).join("");
}
function stateTag(s) {
  return ({active:"a", disabled_quota:"q", request_failed_probe:"f", disabled_stuck:"s", deleted:"s", skipped_external_disabled:"s"}[s] || "s");
}
function stateLabel(s) {
  return ({active:"正常", disabled_quota:"限额禁用", request_failed_probe:"失败重查", disabled_stuck:"长期失败", deleted:"已删除", skipped_external_disabled:"外部禁用"}[s] || s || "-");
}
function fmtMs(ms) {
  const s = Math.floor(ms / 1000);
  if (s < 60) return s + "s";
  if (s < 3600) return Math.floor(s / 60) + "m";
  return Math.floor(s / 3600) + "h " + Math.floor((s % 3600) / 60) + "m";
}
function esc(s) {
  return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) {
    return ({ "&": "&", "<": "<", ">": ">", '"': """, "'": "&#39;" })[c];
  });
}
async function loadLogs() {
  const r = await api("logs?since=" + lastLogMs);
  if (!r || !r.ok) return;
  const d = r.result || {};
  const logs = d.logs || [];
  if (logs.length) lastLogMs = logs[logs.length - 1].at_ms;
  const el = document.getElementById("log");
  if (logs.length) {
    el.innerHTML = logs.map(function (l) {
      return "<div class=\"line\"><span class=\"t\">" + new Date(l.at_ms).toLocaleTimeString() + "</span>" +
        "<span class=\"lvl " + l.level + "\">" + l.level + "</span>" +
        "<span class=\"acc\">" + esc(l.auth_index || "") + "</span>" +
        "<span class=\"msg\">" + esc(l.message) + "</span></div>";
    }).join("");
  }
  document.getElementById("logCount").textContent = logs.length + " 条新";
}
async function togglePlugin() {
  const s = await api("state");
  const d = s.result || {};
  const want = !d.enabled;
  const r = await api("toggle", {method: "POST", body: {enabled: want}});
  if (r && r.ok) loadState();
}
async function runTick() {
  await api("run", {method: "POST"});
  setTimeout(function () { loadState(); }, 300);
}
async function recover(idx) {
  await api("recover", {method: "POST", body: {auth_index: idx}});
  setTimeout(loadState, 400);
}
async function del(idx) {
  if (!confirm("确认删除？此操作不可撤销")) return;
  await api("delete", {method: "POST", body: {auth_index: idx}});
  setTimeout(loadState, 400);
}
async function clearLogs() { await api("logs/clear", {method: "POST"}); loadLogs(); }
async function loadCfg() {
  const r = await api("settings");
  if (!r || !r.ok) return;
  const d = r.result || {};
  document.getElementById("cfgURL").value = d.management_url || "";
  const st = document.getElementById("cfgStatus");
  if (st) st.textContent = d.management_key_set ? "Key 已配置 ✓" : "未配置 Key";
  const pst = document.getElementById("cfgProxyStatus");
  if (pst) pst.textContent = d.proxy_url_configured ? "代理已配置 ✓" : "未配置代理";
  // cfgProxy field stays empty on load: sending empty preserves current value,
  // so the user typing nothing means "keep as-is".
}
async function saveCfg() {
  const url = document.getElementById("cfgURL").value.trim();
  let key = document.getElementById("cfgKey").value;
  if (!url) { alert("请填写管理 API 基址"); return; }
  const body = {management_url: url};
  // Only send key when user typed something (empty = keep current).
  if (key && key.trim() !== "") body.management_key = key.trim();
  // Only send proxy_url when the user typed something non-empty; an empty
  // field means "keep current proxy". Send literal "" only when the user
  // wants to clear it, which we signal by them typing a single space and
  // trimming to "" — but the simplest safe rule: proxy field non-empty =
  // replace; leave it blank on load and it won't be sent.
  const proxy = document.getElementById("cfgProxy").value.trim();
  if (proxy !== "") body.proxy_url = proxy;
  const r = await api("settings", {method: "POST", body: body});
  if (r && r.ok) {
    if (key && key.trim() !== "") setMgmtKey(key.trim());
    document.getElementById("cfgKey").value = "";
    const guide = document.getElementById("cfgGuide");
    if (guide) guide.style.display = "none";
    loadState(); loadLogs(); loadCfg();
    alert("配置已保存" + (key && key.trim() ? "，浏览器鉴权已同步" : ""));
  }
  else { alert("保存失败: " + (r && r.error ? r.error.message : "")); }
}
document.getElementById("btnSaveCfg").addEventListener("click", saveCfg);
document.getElementById("switch").addEventListener("click", togglePlugin);
document.getElementById("switch").addEventListener("keydown", function (e) {
  if (e.key === "Enter" || e.key === " ") { e.preventDefault(); togglePlugin(); }
});
document.getElementById("btnRun").addEventListener("click", runTick);
document.getElementById("btnClear").addEventListener("click", clearLogs);
loadState(); loadLogs(); loadCfg();
setInterval(loadState, 5000);
setInterval(loadLogs, 3000);
</script></body></html>`
	return []byte(tpl)
}
