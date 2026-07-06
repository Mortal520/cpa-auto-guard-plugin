package main

import (
	"encoding/json"
	"fmt"
	"io"
	"bytes"
	"net/http"
	"strings"
	"time"
)

// mgmtAuthEntry is the subset of /v0/management/auth-files records the plugin
// needs. The endpoint returns credential metadata without access_token.
type mgmtAuthEntry struct {
	AuthIndex      string `json:"auth_index"`
	Name           string `json:"name"`
	Provider       string `json:"provider"`
	Account        string `json:"account"`
	Email          string `json:"email"`
	Disabled       bool   `json:"disabled"`
	Unavailable    bool   `json:"unavailable"`
	StatusMessage  string `json:"status_message"`
	IDToken        *struct {
		ChatGPTAccountID string `json:"chatgpt_account_id"`
	} `json:"id_token"`
}

// mgmtAuthList calls the CPA management API to list all auth files. This is a
// better data source than host.auth.list when the host callback path is
// unavailable. Returns the full list when management_key is configured.
func mgmtAuthList(cfg guardConfig) ([]mgmtAuthEntry, error) {
	if cfg.ManagementKey == "" {
		return nil, fmt.Errorf("management_key not configured")
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.ManagementURL), "/")
	if base == "" {
		base = "http://127.0.0.1:8317"
	}
	url := base + "/v0/management/auth-files"
	body, err := mgmtHTTPGet(url, cfg.ManagementKey)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Files []mgmtAuthEntry `json:"files"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode auth-files response: %w", err)
	}
	return resp.Files, nil
}

// mgmtAuthDownload fetches the full auth JSON for one auth file name via the
// /v0/management/auth-files/download endpoint. Unlike the list endpoint, this
// returns the raw on-disk JSON including the access_token used for probing.
func mgmtAuthDownload(cfg guardConfig, name string) (json.RawMessage, error) {
	if cfg.ManagementKey == "" {
		return nil, fmt.Errorf("management_key not configured")
	}
	if name == "" {
		return nil, fmt.Errorf("auth file name required")
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.ManagementURL), "/")
	if base == "" {
		base = "http://127.0.0.1:8317"
	}
	// Encode the name as a query parameter to avoid path-traversal pitfalls.
	url := base + "/v0/management/auth-files/download?name=" + urlEncode(name)
	body, err := mgmtHTTPGet(url, cfg.ManagementKey)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(body), nil
}

// mgmtHTTPGet performs a direct HTTP GET to the management API, bypassing the
// host proxy so requests to localhost resolve inside the container. The CPA
// management key is sent via the X-Management-Key header.
func mgmtHTTPGet(target, managementKey string) ([]byte, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("build mgmt request %s: %w", target, err)
	}
	req.Header.Set("X-Management-Key", managementKey)
	resp, errDo := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mgmt request %s: %w", target, errDo)
	}
	defer resp.Body.Close()
	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return nil, fmt.Errorf("read mgmt response %s: %w", target, errRead)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mgmt %s status %d: %s", target, resp.StatusCode, truncateForLog(string(body), 160))
	}
	return body, nil
}

func urlEncode(s string) string {
	// Minimal safe encoding for file names passed as query values.
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' {
			b.WriteByte(c)
		} else if c == ' ' {
			b.WriteByte('+')
		} else {
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		}
	}
	return b.String()
}

// mgmtResolveTokenAndAccount downloads the full auth JSON for one auth file
// (resolved via the management API list to map auth_index -> name) and extracts
// the bearer token plus ChatGPT account id header. Returns ok=false when the
// management API path is not configured so callers can fall back gracefully.
func mgmtResolveTokenAndAccount(cfg guardConfig, authIndex string) (token, accountID string, ok bool) {
	if cfg.ManagementKey == "" || authIndex == "" {
		return "", "", false
	}
	files, err := mgmtAuthList(cfg)
	if err != nil || len(files) == 0 {
		return "", "", false
	}
	var name string
	var accIDFromList string
	for _, f := range files {
		if f.AuthIndex == authIndex {
			name = f.Name
			if f.IDToken != nil {
				accIDFromList = f.IDToken.ChatGPTAccountID
			}
			break
		}
	}
	if name == "" {
		return "", "", false
	}
	raw, err := mgmtAuthDownload(cfg, name)
	if err != nil {
		return "", "", false
	}
	t, id := extractTokenAndAccountID(raw)
	if id == "" {
		id = accIDFromList
	}
	return t, id, true
}

func mgmtResolveName(cfg guardConfig, authIndex string) string {
	if authIndex == "" {
		return ""
	}
	files, err := mgmtAuthList(cfg)
	if err != nil || len(files) == 0 {
		return ""
	}
	for _, f := range files {
		if f.AuthIndex == authIndex {
			return f.Name
		}
	}
	return ""
}

// mgmtSetDisabled toggles an auth file disabled state via the CPA management
// API (PATCH /v0/management/auth-files/status). Returns the *previous*
// disabled state observed in the listing so callers can skip no-op writes.
// Replaces the broken host.auth.get/host.auth.save path on c-shared builds.
func mgmtSetDisabled(cfg guardConfig, authIndex string, disabled bool) (bool, error) {
	if cfg.ManagementKey == "" {
		return false, fmt.Errorf("management_key not configured")
	}
	prev := false
	if files, err := mgmtAuthList(cfg); err == nil {
		for _, f := range files {
			if f.AuthIndex == authIndex {
				prev = f.Disabled
				break
			}
		}
	}
	if prev == disabled {
		return prev, nil
	}
	name := mgmtResolveName(cfg, authIndex)
	if name == "" {
		return prev, fmt.Errorf("auth file not found for index %s", authIndex)
	}
	body, _ := json.Marshal(map[string]any{"name": name, "disabled": disabled})
	base := strings.TrimRight(strings.TrimSpace(cfg.ManagementURL), "/")
	if base == "" {
		base = "http://127.0.0.1:8317"
	}
	target := base + "/v0/management/auth-files/status"
	respBody, err := mgmtHTTPPatch(target, body, cfg.ManagementKey)
	if err != nil {
		return prev, fmt.Errorf("mgmt set disabled: %w", err)
	}
	_ = respBody
	return prev, nil
}

// mgmtDeleteAuthFile removes an auth file via the CPA management API
// (DELETE /v0/management/auth-files?name=<name>). Replaces the broken
// host.auth.save tombstone path on c-shared builds.
func mgmtDeleteAuthFile(cfg guardConfig, authIndex string) error {
	if cfg.ManagementKey == "" {
		return fmt.Errorf("management_key not configured")
	}
	name := mgmtResolveName(cfg, authIndex)
	if name == "" {
		return fmt.Errorf("auth file not found for index %s", authIndex)
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.ManagementURL), "/")
	if base == "" {
		base = "http://127.0.0.1:8317"
	}
	target := base + "/v0/management/auth-files?name=" + urlEncode(name)
	return mgmtHTTPDelete(target, cfg.ManagementKey)
}

// mgmtHTTPPatch performs a direct HTTP PATCH to the management API. Bypasses
// the host proxy so localhost resolves inside the container.
func mgmtHTTPPatch(target string, body []byte, managementKey string) ([]byte, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest(http.MethodPatch, target, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build mgmt patch %s: %w", target, err)
	}
	req.Header.Set("X-Management-Key", managementKey)
	req.Header.Set("Content-Type", "application/json")
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("mgmt patch %s: %w", target, errDo)
	}
	defer resp.Body.Close()
	respBody, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return nil, fmt.Errorf("read mgmt patch response %s: %w", target, errRead)
	}
	if resp.StatusCode != http.StatusOK {
		return respBody, fmt.Errorf("mgmt patch %s status %d: %s", target, resp.StatusCode, truncateForLog(string(respBody), 160))
	}
	return respBody, nil
}

// mgmtHTTPDelete performs a direct HTTP DELETE to the management API.
func mgmtHTTPDelete(target, managementKey string) error {
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest(http.MethodDelete, target, nil)
	if err != nil {
		return fmt.Errorf("build mgmt delete %s: %w", target, err)
	}
	req.Header.Set("X-Management-Key", managementKey)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return fmt.Errorf("mgmt delete %s: %w", target, errDo)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("mgmt delete %s status %d: %s", target, resp.StatusCode, truncateForLog(string(respBody), 160))
	}
	return nil
}
