package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
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
	path := strings.TrimRight(strings.TrimSpace(req.Path), "/")
	if path == "" || path == "/" {
		return okEnvelope(managementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"content-type": []string{"text/html; charset=utf-8"}},
			Body:       renderConsole(req),
		})
	}
	// Resource page sub-paths render the same console; SPA-style routing is done client-side.
	if strings.HasPrefix(path, "/view") || strings.HasPrefix(path, "/about") {
		return okEnvelope(managementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"content-type": []string{"text/html; charset=utf-8"}},
			Body:       renderConsole(req),
		})
	}
	// API routes under /v0/management/cpa-auto-guard/...
	if strings.HasPrefix(path, "/cpa-auto-guard/") {
		return dispatchAPI(req, strings.TrimPrefix(path, "/cpa-auto-guard/"))
	}
	return okEnvelope(managementResponse{
		StatusCode: http.StatusNotFound,
		Headers:    http.Header{"content-type": []string{"text/plain; charset=utf-8"}},
		Body:       []byte("not found"),
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
	files, err := hostAuthList()
	internal := guard().snapshot()
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
	go guard().tick()
	return jsonResponse(map[string]any{"ok": true, "message": "tick scheduled"})
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
	if err := guard().deleteAccount(authIndex, account); err != nil {
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
func renderConsole(req managementRequest) []byte {
	const tpl = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>cpa-auto-guard 控制台</title>
<style>
:root{--bg:#0f172a;--panel:#111827;--text:#e5e7eb;--muted:#94a3b8;--accent:#38bdf8;--warn:#fbbf24;--err:#f87171;--ok:#34d399;--border:#1f2937}
*{box-sizing:border-box}body{margin:0;background:linear-gradient(135deg,#0b1224 0%,#0f172a 60%,#111827 100%);color:var(--text);font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif;min-height:100vh;padding:1.25rem}
h1{font-size:1.25rem;margin:0 0 .5rem;display:flex;align-items:center;gap:.5rem}.badge{font-size:.7rem;padding:.15rem .5rem;border-radius:9999px;background:#1e293b;color:var(--muted);border:1px solid var(--border)}
.grid{display:grid;gap:1rem;grid-template-columns:1fr;max-width:1100px;margin:0 auto}
.card{background:rgba(17,24,39,.7);border:1px solid var(--border);border-radius:12px;padding:1rem;backdrop-filter:blur(4px)}
.row{display:flex;gap:.75rem;flex-wrap:wrap;align-items:center}.muted{color:var(--muted)}.small{font-size:.78rem}
button{background:#1e293b;color:var(--text);border:1px solid var(--border);padding:.45rem .8rem;border-radius:8px;cursor:pointer;font-size:.85rem;transition:all .15s}button:hover{border-color:var(--accent);color:var(--accent)}button.warn:hover{border-color:var(--warn);color:var(--warn)}button.err:hover{border-color:var(--err);color:var(--err)}button:disabled{opacity:.5;cursor:not-allowed}
.toggle{display:inline-flex;align-items:center;gap:.4rem}.switch{width:44px;height:24px;background:#1e293b;border-radius:9999px;position:relative;transition:background .2s}.switch.on{background:var(--ok)}.switch::after{content:"";position:absolute;width:18px;height:18px;border-radius:50%;background:#e5e7eb;top:3px;left:3px;transition:left .2s}.switch.on::after{left:23px}
table{width:100%;border-collapse:collapse;font-size:.78rem}th,td{padding:.4rem .5rem;text-align:left;border-bottom:1px solid var(--border)}th{color:var(--muted);font-weight:600}.tag{padding:.1rem .4rem;border-radius:6px;font-size:.7rem;background:#1e293b}.tag.q{color:var(--warn)}.tag.f{color:var(--err)}.tag.a{color:var(--ok)}.tag.s{color:var(--muted)}
#log{max-height:300px;overflow-y:auto;background:#0b1224;border:1px solid var(--border);border-radius:8px;padding:.5rem;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:.75rem;line-height:1.5}.line{display:flex;gap:.5rem;padding:.15rem 0;border-bottom:1px dashed #1e293b}.t{color:var(--muted);min-width:90px}.lvl{min-width:50px;font-weight:600}.lvl.info{color:var(--accent)}.lvl.warn{color:var(--warn)}.lvl.error{color:var(--err)}.acc{color:var(--muted);min-width:140px}.msg{flex:1;word-break:break-word}
.stat{display:flex;flex-direction:column;gap:.2rem}.stat .n{font-size:1.5rem;font-weight:700}.stats{display:grid;grid-template-columns:repeat(auto-fit,minmax(140px,1fr));gap:.75rem}
</style></head><body><div class="grid">
<div class="card"><div class="row" style="justify-content:space-between"><h1><span>🛡️ cpa-auto-guard</span><span id="verBadge" class="badge">v` + pluginVer + `</span><span id="enBadge" class="badge">加载中</span></h1>
<div class="row"><label class="toggle"><div id="switch" class="switch" onclick="togglePlugin()"></div><span id="switchLabel" class="small muted">关闭</span></label>
<button onclick="runTick()">⚡ 立即执行</button>
<button class="warn" onclick="clearLogs()">清空日志</button></div></div></div>
<div class="card"><div class="stats" id="stats"></div></div>
<div class="card"><div class="row" style="justify-content:space-between"><h3 style="margin:0">账号</h3><span class="small muted" id="accCount"></span></div><div style="overflow-x:auto"><table id="accTable"><thead><tr><th>状态</th><th>账号</th><th>file</th><th>CPA disabled</th><th>guard</th><th>重置剩余</th><th>retry</th><th>操作</th></tr></thead><tbody id="accBody"><tr><td colspan="8" class="muted">加载中…</td></tr></tbody></table></div></div>
<div class="card"><div class="row" style="justify-content:space-between"><h3 style="margin:0">日志</h3><span class="small muted" id="logCount"></span></div><div id="log"><div class="muted">加载中…</div></div></div>
<div class="card small muted">cpa-auto-guard 在 CPA 进程内自驱动管理 Codex 账号：限额禁用、到期探测恢复、request_failed 连续失败删除。日志仅在内存中保留最近 500 条。</div>
</div>
<script>
const API = "/v0/management/cpa-auto-guard";
let lastLogMs = 0;
async function api(path, opts) {
  opts = opts || {};
  const r = await fetch(API + "/" + path, {
    method: opts.method || "GET",
    headers: {"content-type": "application/json"},
    body: opts.body ? JSON.stringify(opts.body) : undefined
  });
  try { return await r.json(); } catch (e) { return {ok: false, error: "bad json"}; }
}
async function loadState() {
  const s = await api("state");
  if (!s || !s.ok) return;
  const d = s.result || {};
  const sw = document.getElementById("switch");
  sw.classList.toggle("on", !!d.enabled);
  document.getElementById("switchLabel").textContent = d.enabled ? "开启" : "关闭";
  document.getElementById("enBadge").textContent = d.enabled ? "运行中" : "已停用";
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
loadState(); loadLogs();
setInterval(loadState, 5000);
setInterval(loadLogs, 3000);
</script></body></html>`
	return []byte(tpl)
}
