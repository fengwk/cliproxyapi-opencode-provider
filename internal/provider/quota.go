package provider

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// quotaDisplayName is the user-facing label published to management clients.
const quotaDisplayName = "OpenCode Go"

// endpointUsage is the official OpenCode Go subscription usage endpoint. It is
// joined to the configured base URL so a reverse-proxy base path is preserved.
const endpointUsage = "/usage"

// quotaWindowNames fixes the published bucket order to the official windows.
var quotaWindowNames = []string{"rolling", "weekly", "monthly"}

// rpcQuotaRequest mirrors the host quota.fetch / quota.reset wire shape. The
// embedded request carries the selected credential; host_callback_id authorizes
// the matching host.http.do call.
type rpcQuotaRequest struct {
	pluginapi.QuotaFetchRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// quotaDescription advertises the single supported provider. Reset is never
// supported, so the host can reject it without any upstream request.
func quotaDescription() pluginapi.QuotaDescribeResponse {
	return pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{ProviderID},
		DisplayName:        quotaDisplayName,
		SupportsReset:      false,
	}
}

// upstreamUsage is the official usage payload. Each window is a pointer so a
// missing or null window is distinguishable from a present one.
type upstreamUsage struct {
	Usage *upstreamWindows `json:"usage"`
}

type upstreamWindows struct {
	Rolling *upstreamWindow `json:"rolling"`
	Weekly  *upstreamWindow `json:"weekly"`
	Monthly *upstreamWindow `json:"monthly"`
}

type upstreamWindow struct {
	Status   string   `json:"status"`
	Percent  *float64 `json:"percent"`
	ResetsAt string   `json:"resetsAt"`
}

func (w *upstreamWindows) window(name string) *upstreamWindow {
	switch name {
	case "rolling":
		return w.Rolling
	case "weekly":
		return w.Weekly
	case "monthly":
		return w.Monthly
	default:
		return nil
	}
}

// quotaHeaders builds the fixed upstream header set for the usage probe. No
// session, cache, or client headers are forwarded.
func quotaHeaders(key string) http.Header {
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+key)
	headers.Set("Accept", "application/json")
	headers.Set("User-Agent", PluginID+"/"+Version)
	return headers
}

// fetchQuota performs exactly one read-only usage GET for the selected
// credential. A foreign provider is rejected before any key is resolved or sent.
func (m *Manager) fetchQuota(req rpcQuotaRequest) (pluginapi.QuotaFetchResponse, error) {
	if provider := strings.TrimSpace(req.Provider); provider != "" && provider != ProviderID {
		return pluginapi.QuotaFetchResponse{}, &ProviderError{
			Code:       "unsupported_provider",
			Message:    "quota is only available for opencode-go",
			HTTPStatus: http.StatusBadRequest,
		}
	}
	key, errKey := resolveKey(req.Attributes, req.Metadata, req.StorageJSON)
	if errKey != nil {
		return pluginapi.QuotaFetchResponse{}, &ProviderError{
			Code:       "auth_unavailable",
			Message:    errKey.Error(),
			HTTPStatus: http.StatusUnauthorized,
		}
	}
	cfg := m.config()
	resp, errDo := m.bridge.HTTPDo(http.MethodGet, joinURL(cfg.BaseURL, endpointUsage), quotaHeaders(key), nil, req.HostCallbackID)
	if errDo != nil {
		return pluginapi.QuotaFetchResponse{}, errDo
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return pluginapi.QuotaFetchResponse{}, upstreamError(resp.StatusCode)
	}
	return normalizeQuota(resp.Body)
}

// quotaInvalid reports an unusable upstream payload without echoing any of it.
func quotaInvalid() error {
	return &ProviderError{
		Code:       "invalid_upstream",
		Message:    "upstream quota response is invalid",
		HTTPStatus: http.StatusBadGateway,
	}
}

// normalizeQuota validates all three official windows and maps usage percent to
// a remaining fraction. Every failure is sanitized; raw upstream data is never
// reflected.
func normalizeQuota(body []byte) (pluginapi.QuotaFetchResponse, error) {
	if !json.Valid(body) {
		return pluginapi.QuotaFetchResponse{}, quotaInvalid()
	}
	var payload upstreamUsage
	if err := json.Unmarshal(body, &payload); err != nil || payload.Usage == nil {
		return pluginapi.QuotaFetchResponse{}, quotaInvalid()
	}
	buckets := make([]pluginapi.QuotaBucket, 0, len(quotaWindowNames))
	for _, name := range quotaWindowNames {
		window := payload.Usage.window(name)
		if window == nil || window.Percent == nil {
			return pluginapi.QuotaFetchResponse{}, quotaInvalid()
		}
		if window.Status != "ok" && window.Status != "rate-limited" {
			return pluginapi.QuotaFetchResponse{}, quotaInvalid()
		}
		if *window.Percent < 0 || *window.Percent > 100 {
			return pluginapi.QuotaFetchResponse{}, quotaInvalid()
		}
		if _, errReset := time.Parse(time.RFC3339, window.ResetsAt); errReset != nil {
			return pluginapi.QuotaFetchResponse{}, quotaInvalid()
		}
		remaining := 1 - *window.Percent/100
		if window.Status == "rate-limited" {
			remaining = 0
		}
		buckets = append(buckets, pluginapi.QuotaBucket{
			Window:            name,
			RemainingFraction: remaining,
			ResetTime:         window.ResetsAt,
		})
	}
	return pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{Plan: quotaDisplayName},
		Groups:       []pluginapi.QuotaGroup{{DisplayName: quotaDisplayName, Buckets: buckets}},
	}, nil
}
