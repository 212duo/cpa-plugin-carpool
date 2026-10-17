package main

import (
	"encoding/json"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The rules below mirror CLIProxyAPI's internal/pluginstore validation (registry.go,
// github.go, install.go), which plugins cannot import.

var storePluginIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func TestPluginStoreEntry(t *testing.T) {
	raw, err := os.ReadFile("plugin-store/registry-entry.json")
	if err != nil {
		t.Fatal(err)
	}
	var entry map[string]any
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatal(err)
	}

	allowed := map[string]bool{"id": true, "name": true, "description": true, "author": true, "version": true,
		"repository": true, "logo": true, "homepage": true, "license": true, "tags": true}
	for k := range entry {
		if !allowed[k] {
			t.Errorf("field %q is not part of the store registry format", k)
		}
	}
	for _, k := range []string{"id", "name", "description", "author", "repository"} {
		if s, _ := entry[k].(string); strings.TrimSpace(s) == "" {
			t.Errorf("required field %q is missing", k)
		}
	}
	if _, ok := entry["version"]; ok {
		t.Error("version is a legacy field; the latest release tag is the source of truth")
	}

	id, _ := entry["id"].(string)
	if !storePluginIDPattern.MatchString(id) {
		t.Errorf("invalid plugin id %q", id)
	}
	// CPA derives the plugin ID from the library file name, and the release zip
	// must contain <id>.so at its root, so all three have to agree.
	if id != "cpa-carpool" {
		t.Errorf("store id %q does not match the library name cpa-carpool.so", id)
	}

	meta := pluginRegistration(nil).Metadata
	repo, _ := entry["repository"].(string)
	u, err := url.Parse(repo)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || len(strings.Split(strings.Trim(u.Path, "/"), "/")) != 2 || u.RawQuery != "" {
		t.Errorf("repository must be https://github.com/{owner}/{repo}, got %q", repo)
	}
	if repo != meta.GitHubRepository {
		t.Errorf("repository %q differs from plugin metadata %q", repo, meta.GitHubRepository)
	}
	if logo, _ := entry["logo"].(string); logo != PluginLogoURL {
		t.Errorf("logo %q differs from PluginLogoURL %q", logo, PluginLogoURL)
	}
	if _, err := os.Stat("docs/images/logo.png"); err != nil {
		t.Errorf("logo file: %v", err)
	}
}

func TestReleaseWorkflowShipsStoreAssets(t *testing.T) {
	raw, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	wf := string(raw)
	// CPA looks for <id>_<version>_<goos>_<goarch>.zip next to checksums.txt.
	for _, want := range []string{
		`name="cpa-carpool_${GITHUB_REF_NAME#v}_linux_${{ matrix.arch }}"`,
		`zip -qr "../$name.zip" .`,
		`grep -qx 'cpa-carpool.so'`,
		`> checksums.txt`,
		`dist/*.zip`,
		`dist/checksums.txt`,
	} {
		if !strings.Contains(wf, want) {
			t.Errorf("release.yml does not contain %q", want)
		}
	}
}
