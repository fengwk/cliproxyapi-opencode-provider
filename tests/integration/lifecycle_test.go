//go:build integration

package integration

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A failed native replacement must roll back to the same quiesced instance and
// restore both execution modes, without restarting the host.
func TestFailedPluginReplacementRestoresInference(t *testing.T) {
	h := newHarness(t)
	if status, body := h.importKeys(t, []string{keyAlpha}, "rollback"); status != http.StatusOK {
		t.Fatalf("import status %d body %s", status, truncate(body, 400))
	}

	invalidDir := filepath.Join(h.dir, "invalid-plugins")
	invalidLib := filepath.Join(invalidDir, runtime.GOOS, runtime.GOARCH, pluginLibName)
	if err := os.MkdirAll(filepath.Dir(invalidLib), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(invalidLib, []byte("not a shared library\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := hostConfig(h.port, h.authDir, invalidDir, h.mock.baseURL())
	if err := os.WriteFile(filepath.Join(h.dir, "config.yaml"), config, 0o600); err != nil {
		t.Fatal(err)
	}

	// Wait for evidence of the actual replacement attempt, not just a request
	// served before the config watcher reloaded the invalid library.
	deadline := time.Now().Add(12 * time.Second)
	attempted := false
	for time.Now().Before(deadline) {
		logs := h.stdout.String() + h.stderr.String()
		if strings.Contains(logs, "failed to load plugin "+pluginID) && strings.Contains(logs, invalidLib) {
			attempted = true
			break
		}
		select {
		case <-h.done:
			t.Fatalf("host exited during rollback: %v", h.waitError())
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !attempted {
		t.Fatalf("host did not attempt the invalid replacement\n%s\n%s", h.stdout.String(), h.stderr.String())
	}

	h.waitForCatalog(t, 10*time.Second, nativeGLM, nativeMinimax, nativeGPT)
	h.waitForServing(t, nativeGLM, 10*time.Second)
	for _, stream := range []bool{false, true} {
		body, headers := clientChat.request(nativeGLM, "rollback-session", "full", stream)
		status, raw := h.doJSON(t, http.MethodPost, clientChat.path, body, headers)
		if status != http.StatusOK {
			t.Fatalf("rollback stream=%t status %d body %s", stream, status, truncate(raw, 400))
		}
		assertObserved(t, clientChat.observe(t, raw, stream))
	}
	h.mock.requireClean(t)
}
