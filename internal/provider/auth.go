package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Errors returned by the provider dispatch. They deliberately carry no key
// material or credential payload.
var (
	errMissingAuthID   = errors.New("selected auth is missing an id")
	errMissingSession  = errors.New("canonical session id is required")
	errSessionScope    = errors.New("failed to derive session scope")
	errMissingKey      = errors.New("selected auth has no api key")
	errAuthDisabled    = errors.New("selected auth is disabled")
	errUnsupportedPair = errors.New("unsupported protocol translation pair")
)

// authRecord is the physical auth JSON understood by this provider.
type authRecord struct {
	Type     string `json:"type"`
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Label    string `json:"label"`
	APIKey   string `json:"api_key"`
	Disabled bool   `json:"disabled"`
	Weight   *int   `json:"weight"`
	Prefix   string `json:"prefix"`
	ProxyURL string `json:"proxy_url"`
}

// metadataFields lists the non-secret native auth fields preserved in metadata.
var metadataFields = []string{"type", "id", "label", "disabled", "weight", "prefix", "proxy_url"}

// parseAuth recognizes an OpenCode Go physical auth record and maps it to host
// auth data. The stable ID mirrors the host path-based identity (file name).
func parseAuth(req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	var raw map[string]any
	if err := json.Unmarshal(req.RawJSON, &raw); err != nil {
		if req.Provider == ProviderID {
			return pluginapi.AuthParseResponse{}, fmt.Errorf("opencode-go auth record has invalid JSON")
		}
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	record := decodeAuthRecord(raw)
	if (req.Provider != "" && req.Provider != ProviderID) ||
		(record.Type != ProviderID && record.Provider != ProviderID) {
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	key := strings.TrimSpace(record.APIKey)
	if key == "" {
		return pluginapi.AuthParseResponse{}, fmt.Errorf("opencode-go auth record has no api key")
	}
	id := strings.TrimSpace(req.FileName)
	if id == "" {
		id = strings.TrimSpace(record.ID)
	}
	if id == "" {
		return pluginapi.AuthParseResponse{}, fmt.Errorf("opencode-go auth record has no stable id")
	}
	return pluginapi.AuthParseResponse{
		Handled: true,
		Auth: pluginapi.AuthData{
			Provider:    ProviderID,
			ID:          id,
			FileName:    req.FileName,
			Label:       record.Label,
			Prefix:      record.Prefix,
			ProxyURL:    record.ProxyURL,
			Disabled:    record.Disabled,
			StorageJSON: req.RawJSON,
			Metadata:    nativeMetadata(raw),
			Attributes:  map[string]string{"api_key": key},
		},
	}, nil
}

func decodeAuthRecord(raw map[string]any) authRecord {
	record := authRecord{}
	if value, ok := raw["type"].(string); ok {
		record.Type = value
	}
	if value, ok := raw["provider"].(string); ok {
		record.Provider = value
	}
	if value, ok := raw["id"].(string); ok {
		record.ID = value
	}
	if value, ok := raw["label"].(string); ok {
		record.Label = value
	}
	if value, ok := raw["api_key"].(string); ok {
		record.APIKey = value
	}
	if value, ok := raw["disabled"].(bool); ok {
		record.Disabled = value
	}
	if value, ok := raw["weight"].(float64); ok {
		weight := int(value)
		record.Weight = &weight
	}
	if value, ok := raw["prefix"].(string); ok {
		record.Prefix = value
	}
	if value, ok := raw["proxy_url"].(string); ok {
		record.ProxyURL = value
	}
	return record
}

// nativeMetadata preserves non-secret native auth fields across reloads.
func nativeMetadata(raw map[string]any) map[string]any {
	metadata := make(map[string]any, len(metadataFields))
	for _, field := range metadataFields {
		if value, ok := raw[field]; ok {
			metadata[field] = value
		}
	}
	if len(metadata) == 0 {
		return nil
	}
	return metadata
}

// resolveKey reads the credential with the documented fallback order:
// AuthAttributes, then AuthMetadata, then StorageJSON. host.auth.save initially
// exposes newly imported keys as metadata-only, so metadata must be consulted.
func resolveKey(attributes map[string]string, metadata map[string]any, storage []byte) (string, error) {
	if key := strings.TrimSpace(attributes["api_key"]); key != "" {
		return key, nil
	}
	for _, field := range []string{"api_key", "apiKey"} {
		if value, ok := metadata[field].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value), nil
		}
	}
	if len(storage) > 0 {
		var record authRecord
		if err := json.Unmarshal(storage, &record); err == nil {
			if key := strings.TrimSpace(record.APIKey); key != "" {
				return key, nil
			}
		}
	}
	return "", errMissingKey
}

// authDisabled reports whether host metadata marks the credential disabled.
func authDisabled(metadata map[string]any) bool {
	value, ok := metadata["disabled"].(bool)
	return ok && value
}

// refreshAuth echoes the credential payload unchanged. Manual keys have no
// refresh flow, so no data may be invented or dropped.
func refreshAuth(req pluginapi.AuthRefreshRequest) pluginapi.AuthRefreshResponse {
	return pluginapi.AuthRefreshResponse{
		Auth: pluginapi.AuthData{
			Provider:    ProviderID,
			ID:          req.AuthID,
			StorageJSON: req.StorageJSON,
			Metadata:    req.Metadata,
			Attributes:  req.Attributes,
		},
	}
}
