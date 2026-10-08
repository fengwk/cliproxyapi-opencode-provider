package provider

import (
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// Explicit models supplement discovery without changing the no-config contract.
func TestManualModelsDiscovery(t *testing.T) {
	for _, status := range []int{200, 401, 503, 0} {
		host := newFakeHost()
		host.httpResponses[modelsDiscoveryURL] = pluginapi.HTTPResponse{StatusCode: status,
			Body: []byte(`{"data":[{"id":"glm-manual","created":123}]}`)}
		if status == 0 {
			host.httpErrors[modelsDiscoveryURL] = errors.New("fake network failure")
		}
		m := configuredManager(t, host, "manual-models:\n - id: opencode-go/glm-manual\n - id: novel-manual\n   protocol: claude\n")
		var result pluginapi.ModelResponse
		decodeResult(t, callModelDiscovery(t, m, modelDiscoveryPayload(t, map[string]string{"api_key": "fake-key"}, nil, "")), &result)
		if !reflect.DeepEqual(modelIDs(result.Models), []string{"opencode-go/glm-manual", "opencode-go/novel-manual"}) {
			t.Fatal("manual catalog missing or duplicated")
		}
		if status == 200 && (result.Models[0].Created != 123 || result.Models[0].UserDefined) {
			t.Fatal("official metadata replaced by manual declaration")
		}
		if !result.Models[1].UserDefined {
			t.Fatal("manual entry not user-defined")
		}
		if format, ok := routeNativeModel(m.config(), "novel-manual"); !ok || format != translator.FormatClaude {
			t.Fatal("explicit protocol lost")
		}
		if got := decodeError(t, callModelDiscovery(t, m, modelDiscoveryPayload(t, nil, nil, ""))).StatusCode(); got != http.StatusUnauthorized {
			t.Fatal("manual registration bypassed missing credentials")
		}
	}
	for _, body := range []string{`{"data":[]}`, `{"data":{}}`, `not-json`} {
		host := newFakeHost()
		host.httpResponses[modelsDiscoveryURL] = pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(body)}
		m := configuredManager(t, host, "manual-models:\n - id: glm-manual\n")
		var result pluginapi.ModelResponse
		decodeResult(t, callModelDiscovery(t, m, modelDiscoveryPayload(t, map[string]string{"api_key": "fake-key"}, nil, "")), &result)
		if !reflect.DeepEqual(modelIDs(result.Models), []string{"opencode-go/glm-manual"}) {
			t.Fatal("empty/invalid upstream removed explicit model")
		}
	}
}

// Validation never writes settings and rejects malformed or ambiguous drafts.
func TestManualModelsSettingsAndValidation(t *testing.T) {
	m := configuredManager(t, newFakeHost(), "manual-models:\n - id: glm-manual\n")
	settings := callManagement(t, m, http.MethodGet, managementPath+settingsRoute, nil)
	if settings.StatusCode != 200 || string(settings.Body) != `{"manual-models":[{"id":"glm-manual"}]}` {
		t.Fatal("effective manual settings mismatch")
	}
	for _, raw := range []string{
		`{"manual-models":[]}`,
		`{"manual-models":[{"id":"glm-5.2"}]}`,
		`{"manual-models":[{"id":"novel","protocol":"claude"}]}`,
	} {
		if got := callManagement(t, m, http.MethodPost, managementPath+validateRoute, []byte(raw)); got.StatusCode != 200 {
			t.Fatalf("valid patch rejected: %s", raw)
		}
	}
	for _, raw := range []string{
		`{}`, `{"manual-models":null}`, `{"manual-models":[],"other":true}`,
		`{"manual-models":[],"manual-models":[]}`, `{"manual-models":[]} {}`,
		`{"manual-models":[{"id":"novel"}]}`,
		`{"manual-models":[{"id":"glm-x","protocol":"bad"}]}`,
		`{"manual-models":[{"id":"glm x"}]}`,
		`{"manual-models":[{"id":"glm-x"},{"id":"opencode-go/glm-x"}]}`,
		`{"manual-models":[{"id":"opencode-go/opencode-go/glm-x"}]}`,
		`{"manual-models":[{"id":"glm-x","extra":true}]}`,
	} {
		if got := callManagement(t, m, http.MethodPost, managementPath+validateRoute, []byte(raw)); got.StatusCode != 400 {
			t.Fatalf("invalid patch accepted: %s", raw)
		}
	}
	if len(m.config().ManualModels) != 1 {
		t.Fatal("validation mutated configuration")
	}
}
