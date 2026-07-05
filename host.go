package main

/*
#include <stdint.h>
#include <stdlib.h>
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
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
