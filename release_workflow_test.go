package main

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A retry must publish the original tag, not rebuild the current default branch.
func TestReleaseRetryPreservesTagAndRepository(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	type step struct {
		Name string            `yaml:"name"`
		Uses string            `yaml:"uses"`
		With map[string]string `yaml:"with"`
		Env  map[string]string `yaml:"env"`
		Run  string            `yaml:"run"`
	}
	var workflow struct {
		On   map[string]interface{} `yaml:"on"`
		Env  map[string]string      `yaml:"env"`
		Jobs map[string]struct {
			Steps []step `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	if _, ok := workflow.On["workflow_dispatch"]; !ok {
		t.Fatal("release workflow must allow retrying an existing tag")
	}
	if workflow.Env["RELEASE_TAG"] != "${{ inputs.tag || github.ref_name }}" {
		t.Fatal("tag push and manual retry must select the same release tag")
	}
	build := workflow.Jobs["build"].Steps
	if len(build) == 0 || !strings.HasPrefix(build[0].Uses, "actions/checkout@") ||
		build[0].With["ref"] != "refs/tags/${{ env.RELEASE_TAG }}" {
		t.Fatal("release builds must check out the exact selected tag")
	}
	var commitResolved, commitPackaged, repositoryBound bool
	for _, s := range build {
		if s.Name == "Resolve plugin version" {
			commitResolved = s.Env["PLUGIN_VERSION"] == "${{ env.RELEASE_TAG }}" &&
				strings.Contains(s.Run, "git rev-parse HEAD")
		}
		if s.Name == "Package archive" {
			commitPackaged = s.Env["GITHUB_SHA"] == "${{ steps.version.outputs.commit }}"
		}
	}
	for _, s := range workflow.Jobs["publish"].Steps {
		if s.Name == "Publish GitHub Release" {
			repositoryBound = s.Env["GH_REPO"] == "${{ github.repository }}" &&
				s.Env["TAG"] == "${{ env.RELEASE_TAG }}" &&
				strings.Contains(s.Run, "gh release create") &&
				strings.Contains(s.Run, "--repo \"$GH_REPO\"") &&
				strings.Contains(s.Run, "--verify-tag") &&
				!strings.Contains(s.Run, "--clobber")
		}
	}
	if !commitResolved || !commitPackaged || !repositoryBound {
		t.Fatalf("release identity lost: resolved=%v packaged=%v repository=%v",
			commitResolved, commitPackaged, repositoryBound)
	}
}
