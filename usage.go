package main

import (
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// usageEvent is the plugin-side view of a pluginapi.UsageRecord. It keeps only
// the fields the auto-guard needs.
type usageEvent struct {
	AuthIndex    string `json:"auth_index"`
	Provider     string `json:"provider"`
	AuthID       string `json:"auth_id,omitempty"`
	Account      string `json:"account,omitempty"`
	Model        string `json:"model,omitempty"`
	Failed       bool   `json:"failed"`
	StatusCode   int    `json:"status_code,omitempty"`
	LimitReached bool   `json:"limit_reached,omitempty"`
	ResetAtMS    int64  `json:"reset_at_ms,omitempty"`
	FailureBody  string `json:"failure_body,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// handleUsageEvent decodes a pluginapi.UsageRecord and feeds it into the state
// machine. The host passes the record directly in the request body.
func handleUsageEvent(request []byte) ([]byte, error) {
	if len(request) == 0 {
		return okEnvelopeJSON("{}")
	}
	var record pluginapi.UsageRecord
	if err := json.Unmarshal(request, &record); err != nil {
		return errorEnvelope("decode_usage", err.Error()), nil
	}
	ev := usageEventFromRecord(record)
	guard().recordUsage(ev)
	return okEnvelopeJSON("{}")
}

// usageEventFromRecord maps a host UsageRecord into the plugin's compact shape.
// It parses the failure body for usage_limit_reached signals so the state
// machine receives a usable ResetAtMS even when the host only reports a 429
// with a JSON error payload.
func usageEventFromRecord(r pluginapi.UsageRecord) usageEvent {
	reason := ""
	if r.Failed {
		reason = r.Failure.Body
	}
	ev := usageEvent{
		AuthIndex:   r.AuthIndex,
		Provider:    r.Provider,
		AuthID:      r.AuthID,
		Model:       r.Model,
		Failed:      r.Failed,
		StatusCode:  r.Failure.StatusCode,
		FailureBody:  r.Failure.Body,
		Reason:      truncateReason(reason, 240),
	}
	bodyLower := strings.ToLower(r.Failure.Body)
	if r.Failed && (r.Failure.StatusCode == 429 || strings.Contains(bodyLower, "usage_limit_reached") || strings.Contains(bodyLower, "limit reached")) {
		ev.LimitReached = true
		ev.ResetAtMS = extractResetAt([]byte(r.Failure.Body), nowTime())
	}
	return ev
}

func truncateReason(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
