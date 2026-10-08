//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// Exercise the UI's exact validate/PATCH path on a real host with failed discovery.
func TestManualModelSettingsRegisterAfterDiscoveryFailure(t *testing.T) {
	h := newHarness(t)
	h.mock.setModelsResponse(http.StatusServiceUnavailable, []byte(`{"error":"fake-unavailable"}`))
	payload := mustMarshal(map[string]any{"keys": []string{keyAlpha}})
	status, _ := h.doJSON(t, http.MethodPost, "/v0/management/plugins/"+pluginID+"/keys", payload, managementHeaders())
	if status != 200 {
		t.Fatal("import failed")
	}
	waitForProbeIncrease(t, h.mock, 0, 10*time.Second)
	assertCatalogExactly(t, h.catalogIDs(t))
	prefix := "/v0/management/plugins/" + pluginID
	patch := []byte(`{"manual-models":[{"id":"glm-5.2"}]}`)
	if status, _, err := h.rawRequest(http.MethodPost, prefix+"/validate", patch, nil); err != nil || status != 401 && status != 403 {
		t.Fatal("manual validation is not authenticated")
	}
	if status, _ := h.doJSON(t, http.MethodPost, prefix+"/validate", patch, managementHeaders()); status != 200 {
		t.Fatal("manual validation failed")
	}
	if status, _ := h.doJSON(t, http.MethodPatch, prefix+"/config", patch, managementHeaders()); status != 200 {
		t.Fatal("manual config patch failed")
	}
	h.waitForCatalog(t, 10*time.Second, nativeGLM)
	h.waitForServing(t, nativeGLM, 10*time.Second)
	status, raw := h.doJSON(t, http.MethodGet, prefix+"/config", nil, managementHeaders())
	var saved map[string]any
	json.Unmarshal(raw, &saved)
	if status != 200 || saved["base-url"] != h.mock.baseURL()+"/v1" || saved["enabled"] != true {
		t.Fatal("shallow model patch overwrote unrelated configuration")
	}
	// Successful empty discovery plus clearing explicit models must revoke routing.
	h.mock.setModelsResponse(http.StatusOK, emptyCatalogBody())
	baseline := len(h.mock.callsForPath("/v1/models"))
	if status, _ := h.doJSON(t, http.MethodPatch, prefix+"/config", []byte(`{"manual-models":[]}`), managementHeaders()); status != 200 {
		t.Fatal("clear patch failed")
	}
	waitForCatalogExactly(t, h, h.mock, baseline)
	h.mock.requireClean(t)
}
