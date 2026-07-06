package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
)

// callHost invokes a host callback method and returns the Result field.
func callHost(method string, payload any) (json.RawMessage, error) {
	rawPayload, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal host callback payload %s: %w", method, errMarshal)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(rawPayload) > 0 {
		cPayload := C.CBytes(rawPayload)
		if cPayload == nil {
			return nil, fmt.Errorf("allocate host callback payload %s", method)
		}
		defer C.free(cPayload)
		requestPtr = (*C.uint8_t)(cPayload)
	}
	callCode := C.call_host_api(cMethod, requestPtr, C.size_t(len(rawPayload)), &response)
	var rawResponse []byte
	if response.ptr != nil && response.len > 0 {
		rawResponse = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
	if len(rawResponse) == 0 {
		return nil, fmt.Errorf("host callback %s returned no response, code=%d", method, int(callCode))
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(rawResponse, &env); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host callback envelope %s: %w", method, errUnmarshal)
	}
	if callCode != 0 {
		return nil, fmt.Errorf("host callback %s returned code=%d", method, int(callCode))
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	return append(json.RawMessage(nil), env.Result...), nil
}

// hostAuthList lists all auth files known to the host.
func hostAuthList() ([]pluginapi.HostAuthFileEntry, error) {
	result, err := callHost(pluginabi.MethodHostAuthList, map[string]any{})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, fmt.Errorf("decode host.auth.list result: %w", err)
	}
	return resp.Files, nil
}

// hostAuthGet returns the physical JSON for one auth index.
func hostAuthGet(authIndex string) (pluginapi.HostAuthGetResponse, error) {
	result, err := callHost(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if err != nil {
		return pluginapi.HostAuthGetResponse{}, err
	}
	var resp pluginapi.HostAuthGetResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		return pluginapi.HostAuthGetResponse{}, fmt.Errorf("decode host.auth.get result: %w", err)
	}
	return resp, nil
}

// hostAuthGetRuntime returns the runtime entry for one auth index.
func hostAuthGetRuntime(authIndex string) (pluginapi.HostAuthGetRuntimeResponse, error) {
	result, err := callHost(pluginabi.MethodHostAuthGetRuntime, pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if err != nil {
		return pluginapi.HostAuthGetRuntimeResponse{}, err
	}
	var resp pluginapi.HostAuthGetRuntimeResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		return pluginapi.HostAuthGetRuntimeResponse{}, fmt.Errorf("decode host.auth.get_runtime result: %w", err)
	}
	return resp, nil
}

// hostAuthSave writes physical JSON back to the auth file.
func hostAuthSave(name string, rawJSON json.RawMessage) (pluginapi.HostAuthSaveResponse, error) {
	result, err := callHost(pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{Name: name, JSON: rawJSON})
	if err != nil {
		return pluginapi.HostAuthSaveResponse{}, err
	}
	var resp pluginapi.HostAuthSaveResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		return pluginapi.HostAuthSaveResponse{}, fmt.Errorf("decode host.auth.save result: %w", err)
	}
	return resp, nil
}

// hostLog writes a message to the CPA logger.
func hostLog(level, message string) {
	_, _ = callHost(pluginabi.MethodHostLog, map[string]any{
		"level":   level,
		"message": message,
	})
}

// hostHTTPDo performs an upstream HTTP request through the host.
func hostHTTPDo(method, target string, headers http.Header, body []byte) (pluginapi.HTTPResponse, error) {
	if strings.TrimSpace(method) == "" {
		method = http.MethodGet
	}
	req := pluginapi.HTTPRequest{
		Method:  method,
		URL:     target,
		Headers: headers,
		Body:    body,
	}
	result, err := callHost(pluginabi.MethodHostHTTPDo, req)
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	var resp pluginapi.HTTPResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		return pluginapi.HTTPResponse{}, fmt.Errorf("decode host.http.do result: %w", err)
	}
	return resp, nil
}

// setAuthDisabled fetches the auth JSON, toggles the disabled field, and saves.
// When disabled=false the account is re-enabled. Returns the *previous* disabled
// state observed in the stored JSON (so callers can skip no-op writes).
func setAuthDisabled(authIndex string, disabled bool) (bool, error) {
	getResp, err := hostAuthGet(authIndex)
	if err != nil {
		return false, fmt.Errorf("get auth for disable toggle: %w", err)
	}
	var current map[string]any
	if err := json.Unmarshal(getResp.JSON, &current); err != nil {
		return false, fmt.Errorf("decode auth json: %w", err)
	}
	prev, _ := current["disabled"].(bool)
	if prev == disabled {
		return prev, nil
	}
	current["disabled"] = disabled
	newJSON, err := json.Marshal(current)
	if err != nil {
		return prev, fmt.Errorf("encode auth json: %w", err)
	}
	name := strings.TrimSpace(getResp.Name)
	if name == "" {
		return prev, fmt.Errorf("auth file name missing for index %s", authIndex)
	}
	if _, err := hostAuthSave(name, newJSON); err != nil {
		return prev, fmt.Errorf("save auth file: %w", err)
	}
	return prev, nil
}

// probeUpstream issues a GET against the configured probe URL using the given
func probeUpstream(cfg guardConfig, probeURL, token string, headers http.Header) (pluginapi.HTTPResponse, error) {
	if headers == nil {
		headers = http.Header{}
	}
	if token != "" && headers.Get("Authorization") == "" {
		headers.Set("Authorization", "Bearer "+token)
	}
	// When the host.http.do callback is unreliable on some CPA c-shared builds,
	// bypass it with a direct HTTP client using a user-configured proxy (socks5/http).
	// mgmtOK=true but host.http.do code=1 means we need the direct path.
	if strings.TrimSpace(cfg.ProxyURL) != "" {
		transport, _, errBuild := proxyutil.BuildHTTPTransport(cfg.ProxyURL)
		if errBuild == nil && transport != nil {
			client := &http.Client{Timeout: time.Duration(cfg.ProbeTimeoutMS) * time.Millisecond, Transport: transport}
			req, errReq := http.NewRequest(http.MethodGet, probeURL, nil)
			if errReq != nil {
				return pluginapi.HTTPResponse{}, errReq
			}
			req.Header = headers
			resp, errDo := client.Do(req)
			if errDo != nil {
				return pluginapi.HTTPResponse{}, errDo
			}
			defer resp.Body.Close()
			body, errRead := io.ReadAll(resp.Body)
			if errRead != nil {
				return pluginapi.HTTPResponse{}, errRead
			}
			return pluginapi.HTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Header.Clone(), Body: body}, nil
		}
	}
	return hostHTTPDo(http.MethodGet, probeURL, headers, nil)
}
