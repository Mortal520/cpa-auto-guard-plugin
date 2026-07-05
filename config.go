package main

import (
	"encoding/json"
	"strconv"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// configDefaults returns the initial runtime configuration used when the host
// has not provided explicit plugin config.
func configDefaults() guardConfig {
	return guardConfig{
		Enabled:                  false,
		TickSeconds:              30,
		MaxResetSeconds:          18000,
		DeleteThreshold:          3,
		AutoDisableQuota:         true,
		AutoRecover:              true,
		AutoDeleteRequestFailed:  true,
		ProbeURL:                 "https://chatgpt.com/backend-api/wham/usage",
		ProbeTimeoutMS:           15000,
		RecoverGraceSeconds:      60,
		MaxStuckRetries:          5,
	}
}

// configFields declares plugin-owned configuration fields for the management UI.
func configFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{Name: "enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "插件总开关"},
		{Name: "tick_seconds", Type: pluginapi.ConfigFieldTypeNumber, Description: "主动探测周期(秒)"},
		{Name: "max_reset_seconds", Type: pluginapi.ConfigFieldTypeNumber, Description: "视为正常的重置时长上限(秒)"},
		{Name: "delete_threshold", Type: pluginapi.ConfigFieldTypeInteger, Description: "request_failed 连续失败删除阈值"},
		{Name: "auto_disable_quota", Type: pluginapi.ConfigFieldTypeBoolean, Description: "自动禁用限额账号"},
		{Name: "auto_recover", Type: pluginapi.ConfigFieldTypeBoolean, Description: "到期自动恢复探测"},
		{Name: "auto_delete_request_failed", Type: pluginapi.ConfigFieldTypeBoolean, Description: "request_failed 达阈值删除"},
		{Name: "probe_url", Type: pluginapi.ConfigFieldTypeString, Description: "探测端点"},
		{Name: "probe_timeout_ms", Type: pluginapi.ConfigFieldTypeInteger, Description: "探测超时(ms)"},
		{Name: "recover_grace_seconds", Type: pluginapi.ConfigFieldTypeNumber, Description: "重置时间到期前的探测预留(秒)"},
		{Name: "max_stuck_retries", Type: pluginapi.ConfigFieldTypeInteger, Description: "恢复探测连续失败上限,超过进入 disabled_stick"},
	}
}

// parseConfigFromReconfigure extracts plugin config from a plugin.reconfigure
// RPC payload. The host wraps the plugin's config subtree inside the request.
func parseConfigFromReconfigure(request []byte) guardConfig {
	cfg := configDefaults()
	if len(request) == 0 {
		return cfg
	}
	var raw map[string]any
	if err := json.Unmarshal(request, &raw); err != nil {
		return cfg
	}
	// Host sends {"config": {...}} or the config object directly.
	configMap := raw
	if nested, ok := raw["config"].(map[string]any); ok {
		configMap = nested
	}
	applyConfigMap(&cfg, configMap)
	return cfg
}

func applyConfigMap(cfg *guardConfig, m map[string]any) {
	if m == nil {
		return
	}
	if v, ok := takeBool(m, "enabled"); ok {
		cfg.Enabled = v
	}
	if v, ok := takeNumber(m, "tick_seconds"); ok {
		cfg.TickSeconds = v
	}
	if v, ok := takeNumber(m, "max_reset_seconds"); ok {
		cfg.MaxResetSeconds = v
	}
	if v, ok := takeInt(m, "delete_threshold"); ok {
		cfg.DeleteThreshold = v
	}
	if v, ok := takeBool(m, "auto_disable_quota"); ok {
		cfg.AutoDisableQuota = v
	}
	if v, ok := takeBool(m, "auto_recover"); ok {
		cfg.AutoRecover = v
	}
	if v, ok := takeBool(m, "auto_delete_request_failed"); ok {
		cfg.AutoDeleteRequestFailed = v
	}
	if v, ok := takeString(m, "probe_url"); ok && v != "" {
		cfg.ProbeURL = v
	}
	if v, ok := takeInt(m, "probe_timeout_ms"); ok {
		cfg.ProbeTimeoutMS = v
	}
	if v, ok := takeNumber(m, "recover_grace_seconds"); ok {
		cfg.RecoverGraceSeconds = v
	}
	if v, ok := takeInt(m, "max_stuck_retries"); ok {
		cfg.MaxStuckRetries = v
	}
}

func takeBool(m map[string]any, key string) (bool, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return false, false
	}
	switch t := v.(type) {
	case bool:
		return t, true
	case string:
		b, err := strconv.ParseBool(t)
		return b, err == nil
	case float64:
		return t != 0, true
	}
	return false, false
}

func takeNumber(m map[string]any, key string) (float64, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false
	}
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case string:
		n, err := strconv.ParseFloat(t, 64)
		return n, err == nil
	}
	return 0, false
}

func takeInt(m map[string]any, key string) (int, bool) {
	n, ok := takeNumber(m, key)
	if !ok {
		return 0, false
	}
	return int(n), true
}

func takeString(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", false
	}
	if s, ok := v.(string); ok {
		return s, true
	}
	return "", false
}
