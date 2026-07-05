# cpa-auto-guard

CLIProxyAPI (CPA) native Go 插件：在 CPA 进程内自驱动管理 Codex 账号，集成 Web 控制台与 Management API。

## 功能

- 实时监听每个请求的成败与限额信号 (`usage.handle`)
- 触发限额的账号：自动临时禁用，到期后主动探测，健康则重新启用，仍限额则保持禁用并刷新重置时间
- `request_failed` 类账号 (网络错误/5xx/401)：重查额度，连续达到阈值 (`delete_threshold`) 后删除
- Web 控制台：插件开关、立即执行按钮、日志窗口、账号列表、冷却列表、新鲜度
- Management API：`/v0/management/cpa-auto-guard/*` 供外部脚本调用
- 安全降级：插件关闭时只读；host 回调失败只记录不动作；删除前必须连续 N 次失败

## 架构

```
┌──────────────── CPA Main Process ────────────────┐
│  请求流水线 ── usage.handle ──► cpa-auto-guard ───► 状态机
│  插件宿主          │              │
│    host.auth.*  ◄──┘              │
│    host.http.do  ◄─ 探测 ◄────────┘
│  Management API ├── /v0/management/cpa-auto-guard/*
│  资源页        ├── /v0/resource/plugins/cpa-auto-guard/
└───────────────────────────────────────────────────┘
```

插件不依赖 CPA-Manager-Plus (CPAMP)。单独部署 CPA 也能用。

## 构建

需要 Go 1.26+ (与本插件 SDK `replace` 指向的 CLIProxyAPI 一致)。

```bash
cd F:\project\cpa-auto-guard-plugin
# go.mod 已用 replace 指向 F:\project\_scraps\empty_dirs\team注册机\CLIProxyAPI-main
# 若你的 CLIProxyAPI 在别处,请修改 go.mod 的 replace 行
go mod tidy
# Linux/macOS: 产物 cpa-auto-guard.so
go build -buildmode=c-shared -o bin/cpa-auto-guard.so .
# Windows: 产物 cpa-auto-guard.dll
GOOS=windows go build -buildmode=c-shared -o bin/cpa-auto-guard.dll .
# 或在 Windows PowerShell:
# go build -buildmode=c-shared -o bin/cpa-auto-guard.dll .
```

`go mod tidy` 会把 CLIProxyAPI 的依赖拉到本插件模块。构建后会同时生成二进制和 `bin/*.h` 头文件 (可忽略,由 CPA 自动加载)。

## 安装

1. 将 `bin/cpa-auto-guard.so` 或 `bin/cpa-auto-guard.dll` 放到 CPA `plugins.dir` (默认 `plugins/`)
2. 在 CPA `config.yaml` 启用插件系统并配置本插件:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cpa-auto-guard:
      enabled: true
      priority: 1
      # 以下字段可在面板里改,也可直接写在 yaml 里
      tick_seconds: 30
      max_reset_seconds: 18000
      delete_threshold: 3
      auto_disable_quota: true
      auto_recover: true
      auto_delete_request_failed: true
      probe_url: "https://chatgpt.com/backend-api/wham/usage"
      probe_timeout_ms: 15000
      recover_grace_seconds: 60
      max_stuck_retries: 5
```

3. 重启 CPA (`刷新浏览器不够`,见官方插件文档)。

## 控制台

浏览器访问 CPA 管理面板,在"插件管理"里可以看到 `cpa-auto-guard` 菜单项,点击即打开控制台:

```
GET /v0/resource/plugins/cpa-auto-guard/
```

控制台直接调本插件自己的 Management API,需要带 CPA Management Key 认证 (面板代理会自动处理)。

## Management API

| 方法 | 路径 | 用途 |
|------|------|------|
| GET  | `/v0/management/cpa-auto-guard/state`     | 完整状态 (config + accounts + summary) |
| GET  | `/v0/management/cpa-auto-guard/accounts`   | 账号视图 (实时合并 host.auth.list) |
| GET  | `/v0/management/cpa-auto-guard/logs?since=`| 增量日志 |
| POST | `/v0/management/cpa-auto-guard/run`        | 手动触发一轮 tick |
| POST | `/v0/management/cpa-auto-guard/toggle`     | 开关 (body: `{"enabled":true}`) |
| POST | `/v0/management/cpa-auto-guard/recover`     | 强制恢复 (`{"auth_index":"..."}`) |
| POST | `/v0/management/cpa-auto-guard/delete`      | 强制删除 (`{"auth_index":"..."}`) |
| POST | `/v0/management/cpa-auto-guard/logs/clear`  | 清空内存日志 |

## 状态机

```
active
  │ 429 usage_limit_reached + resets_at
  ▼
disabled_quota ──到期→ probe ──OK──► active (auth.save disabled=false)
  │                          └──仍限额──► disabled_quota (更新 resets_at)
  │                          └──失败──► retry++; ≥max_stuck → disabled_stuck
  │
  │ request_failed (网络错/5xx/401)
  ▼
request_failed_probe
  │ 重查额度
  ├──限额──► disabled_quota
  ├──健康──► active (重置 retry)
  └──失败≥delete_threshold──► deleteAuth
```

## 安全

- 不输出 token / key / cookie。日志只写账号名、auth_index、状态码、原因、reset 倒计时。
- `host.auth.save` 写回时只改 `disabled` (及删除时的 `status_message`/`note`),其它字段保持不变。
- 删除前必须连续 `delete_threshold` 次失败且重查探测也失败。
- 插件关闭时所有写动作停止,只读展示。

## 与 CPAMP 的关系

本插件运行在 CPA 进程内,完全自驱动,不需要 CPAMP。如果你同时部署了 CPAMP,它的 `codex-inspection` / `quota-cooldown` 仍然独立工作,两者不冲突:
- CPAMP 的 `RateLimitAutoDisableWorker` 处理实时 429;本插件处理更广的账号生命周期。
- 同一账号被任意一方禁用后,本插件不会重复禁用 (setAuthDisabled 检测 prev==disabled 跳过)。

## 开发说明

- 本插件只依赖标准库 + CLIProxyAPI sdk (pluginapi, pluginabi)。不引入第三方包。
- go.mod 用 `replace` 指向本地 CLIProxyAPI 源码,便于跟随后续 SDK 变更。
- 源文件组织:
  - `main.go`        C ABI 导出 + 方法分发
  - `config.go`      ConfigFields 与解析
  - `guard.go`       状态机、日志环、usage 事件、tick
  - `usage.go`       usage.handle 解析
  - `host.go`        host 回调封装 (auth/http/log)
  - `management.go`  Management API + HTML 控制台
  - `helper.go`      小工具

## 已知局限 / 待验证

- 本机开发环境暂未安装 Go 工具链,未在本地执行 `go build` 实测。请在具备 Go 1.26+ 的环境执行构建,首次构建如有少量类型不匹配可在该环境当面修正。
- 删除账号通过 `host.auth.save` 写入 disabled + status_message 等价标记;CPA 若提供专用删除 RPC 在未来 SDK 版本可用,可替换为更彻底的删除。
- 探测端点默认与 CPAMP codex 巡检一致 (`https://chatgpt.com/backend-api/wham/usage`),需要 CPA 主机能直连或经 host.http.do 的 transport 策略可达。

## 文档

设计文档: `C:\Users\mortal\Documents\CPA\docs\AUTO_GUARD_DESIGN.md`
