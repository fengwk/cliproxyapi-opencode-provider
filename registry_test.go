package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/fengwk/cliproxyapi-opencode-provider/internal/provider"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginstore"
)

// Validate the public source with CPA's parser so SDK updates check the contract.
func TestCustomRegistryUsesCPAStoreContract(t *testing.T) {
	data, err := os.ReadFile("registry.json")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/registry.json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	defer server.Close()

	sourceURL := server.URL + "/registry.json"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	registry, err := pluginstore.NewClient(server.Client(), sourceURL).FetchRegistry(ctx)
	if err != nil {
		t.Fatalf("CPA rejected registry: %v", err)
	}
	if registry.SchemaVersion != pluginstore.SchemaVersion || len(registry.Plugins) != 1 {
		t.Fatalf("unexpected registry: %#v", registry)
	}
	plugin := registry.Plugins[0]
	if plugin.ID != provider.PluginID {
		t.Fatalf("registry ID %q differs from plugin ID %q", plugin.ID, provider.PluginID)
	}
	if plugin.Name != provider.PluginName {
		t.Fatalf("registry name %q differs from plugin display name %q", plugin.Name, provider.PluginName)
	}
	if plugin.Name == plugin.ID {
		t.Fatalf("registry name must be a human-readable display name, not the technical ID %q", plugin.ID)
	}
	if plugin.Repository != "https://github.com/fengwk/cliproxyapi-opencode-provider" {
		t.Fatalf("unexpected repository %q", plugin.Repository)
	}
	if pluginstore.PluginInstallType(plugin) != pluginstore.InstallTypeGitHubRelease || plugin.Version != "" {
		t.Fatal("source must resolve the latest GitHub Release without a pinned version")
	}
	sources, err := pluginstore.NormalizeSources([]string{sourceURL})
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 || sources[0].ID != pluginstore.DefaultSourceID || sources[1].URL != sourceURL {
		t.Fatalf("custom source must preserve the official source: %#v", sources)
	}
}
