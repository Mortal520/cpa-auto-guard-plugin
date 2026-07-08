package main

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"gopkg.in/yaml.v3"

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
		SweepSeconds:             300,
		ManagementURL:             "",
		ProxyURL:                  "",
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
		{Name: "sweep_seconds", Type: pluginapi.ConfigFieldTypeNumber, Description: "主动额度巡检周期(秒)"},
		{Name: "management_url", Type: pluginapi.ConfigFieldTypeString, Description: "CPA 管理 API 基址 (用于拿账号凭据)"},
		{Name: "management_key", Type: pluginapi.ConfigFieldTypeString, Description: "CPA X-Management-Key (敏感, 不回显)"},
		{Name: "proxy_url", Type: pluginapi.ConfigFieldTypeString, Description: "probe 出站代理(socks5/http), 留空则直连"},
	}
}

// parseConfigFromReconfigure extracts plugin config from a plugin.reconfigure
// RPC payload. The host wraps the plugin's config subtree inside the request.
func parseConfigFromReconfigure(request []byte) guardConfig {
	cfg := configDefaults()
	if len(request) == 0 {
	
		return cfg
	}

	// Host sends {"config_yaml": <YAML bytes>, "schema_version": N}.
	var raw map[string]any
	if err := json.Unmarshal(request, &raw); err != nil {

		return cfg
	}
	// Case 1: native c-shared bridge delivers config_yaml as a YAML string.
	if yamlBytes, ok := extractYAMLBytes(raw); ok {

		applyYAMLConfig(&cfg, yamlBytes)

		return cfg
	}
	// Case 2: host sends {"config": {...}} or the config object directly as JSON.
	configMap := raw
	if nested, ok := raw["config"].(map[string]any); ok {
		configMap = nested
	}
	applyConfigMap(&cfg, configMap)
	return cfg
}

// extractYAMLBytes pulls the YAML config payload from the reconfigure request.
// The c-shared bridge encodes ConfigYAML as a base64 string or a raw string.
func extractYAMLBytes(raw map[string]any) ([]byte, bool) {
	v, ok := raw["config_yaml"]
	if !ok || v == nil {
		return nil, false
	}
	switch t := v.(type) {
	case string:
		// The host serialises ConfigYAML ([]byte) as a base64 string in JSON.
		// When unmarshalled into map[string]any it stays a string, so try to
		// base64-decode first; if that fails, treat it as raw YAML text.
		if decoded, err := base64.StdEncoding.DecodeString(t); err == nil {
			return decoded, true
		}
		return []byte(t), true
	case []byte:
		return t, true
	}
	return nil, false
}

// applyYAMLConfig parses a YAML config document and applies known keys to cfg.
func applyYAMLConfig(cfg *guardConfig, yamlBytes []byte) {
	if len(yamlBytes) == 0 {
		return
	}
	var m map[string]any
	if err := yaml.Unmarshal(yamlBytes, &m); err != nil {
		return
	}
	applyConfigMap(cfg, m)
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
	if v, ok := takeNumber(m, "sweep_seconds"); ok {
		cfg.SweepSeconds = v
	}
	if v, ok := takeString(m, "management_url"); ok && v != "" {
		cfg.ManagementURL = v
	}
	if v, ok := takeString(m, "management_key"); ok && v != "" {
		cfg.ManagementKey = v
	}
	if v, ok := takeString(m, "proxy_url"); ok {
		cfg.ProxyURL = v
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
