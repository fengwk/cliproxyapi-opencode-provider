package provider

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"gopkg.in/yaml.v3"
)

// DefaultBaseURL is the OpenCode Go upstream base URL.
const DefaultBaseURL = "https://opencode.ai/zen/go/v1"

// Upstream endpoint paths. The base URL already carries the /v1 prefix, so
// joins strip the duplicated prefix while preserving any reverse-proxy base path.
const (
	endpointChat      = "/v1/chat/completions"
	endpointMessages  = "/v1/messages"
	endpointResponses = "/v1/responses"
)

// Config is the immutable plugin configuration snapshot decoded from the
// lifecycle config_yaml payload.
type Config struct {
	// BaseURL is the validated upstream base URL (no query/fragment/userinfo).
	BaseURL string
	// Routes maps a native model id to a deterministic upstream protocol.
	Routes map[string]translator.Format
}

// rawConfig mirrors the flattened plugin config YAML. Unknown keys (including
// host-owned enabled/priority) are ignored.
type rawConfig struct {
	BaseURL string     `yaml:"base-url"`
	Models  []rawModel `yaml:"models"`
}

type rawModel struct {
	ID       string `yaml:"id"`
	Protocol string `yaml:"protocol"`
}

// parseConfig decodes and validates the lifecycle config payload. An empty
// payload yields the defaults.
func parseConfig(configYAML []byte) (Config, error) {
	cfg := Config{BaseURL: DefaultBaseURL, Routes: map[string]translator.Format{}}
	trimmed := strings.TrimSpace(string(configYAML))
	if trimmed == "" || trimmed == "null" || trimmed == "{}" {
		return cfg, nil
	}
	var raw rawConfig
	if err := yaml.Unmarshal(configYAML, &raw); err != nil {
		return Config{}, fmt.Errorf("invalid config yaml")
	}
	if base := strings.TrimSpace(raw.BaseURL); base != "" {
		normalized, err := validateBaseURL(base)
		if err != nil {
			return Config{}, err
		}
		cfg.BaseURL = normalized
	}
	for _, item := range raw.Models {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			return Config{}, fmt.Errorf("models entry is missing id")
		}
		if strings.ContainsAny(id, " \t\r\n") {
			return Config{}, fmt.Errorf("models entry id must not contain whitespace")
		}
		format, err := parseProtocol(item.Protocol)
		if err != nil {
			return Config{}, fmt.Errorf("models entry %q: %w", id, err)
		}
		cfg.Routes[id] = format
	}
	return cfg, nil
}

// parseProtocol maps a configured protocol name to a translator format.
func parseProtocol(raw string) (translator.Format, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "openai", "chat-completions", "chat_completions":
		return translator.FormatOpenAI, nil
	case "claude", "anthropic", "messages":
		return translator.FormatClaude, nil
	case "openai-response", "openai_response", "responses":
		return translator.FormatOpenAIResponse, nil
	default:
		return "", fmt.Errorf("unsupported protocol %q", strings.TrimSpace(raw))
	}
}

// protocolEndpoint returns the upstream path for an upstream format.
func protocolEndpoint(format translator.Format) (string, bool) {
	switch format {
	case translator.FormatOpenAI:
		return endpointChat, true
	case translator.FormatClaude:
		return endpointMessages, true
	case translator.FormatOpenAIResponse:
		return endpointResponses, true
	default:
		return "", false
	}
}

// validateBaseURL enforces the transport policy: http(s) only, http restricted
// to loopback, and no userinfo/query/fragment. The path is preserved so reverse
// proxies keep their base path.
func validateBaseURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid base-url")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("base-url must use http or https")
	}
	if parsed.User != nil {
		return "", fmt.Errorf("base-url must not contain userinfo")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("base-url must not contain query or fragment")
	}
	if strings.TrimSpace(parsed.Host) == "" {
		return "", fmt.Errorf("base-url must include a host")
	}
	if scheme == "http" && !isLoopbackHost(parsed.Hostname()) {
		return "", fmt.Errorf("base-url http is only allowed for loopback hosts")
	}
	parsed.Scheme = scheme
	return strings.TrimRight(parsed.String(), "/"), nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// joinURL appends an upstream endpoint to the validated base URL, avoiding a
// duplicated /v1 segment.
func joinURL(base, endpoint string) string {
	base = strings.TrimRight(base, "/")
	endpoint = "/" + strings.TrimLeft(endpoint, "/")
	if strings.HasSuffix(base, "/v1") && strings.HasPrefix(endpoint, "/v1/") {
		endpoint = strings.TrimPrefix(endpoint, "/v1")
	}
	return base + endpoint
}
