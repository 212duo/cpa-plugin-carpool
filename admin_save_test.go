package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

type saveHost struct {
	mu                                 sync.Mutex
	config                             json.RawMessage
	keys                               []string
	writes                             []string
	readFail, neverRevoke, neverReload bool
	addFailures                        int
	gate                               <-chan struct{}
}

const oldSaveConfig = `{"enabled":true,"priority":100,"users":[{"name":"Alice","api-keys":["sk-old"],"primary":["alice.json"]}]}`
const newSaveConfig = `{"enabled":true,"priority":100,"users":[{"name":"Alice","api-keys":["sk-new"],"primary":["alice.json"]}]}`

func newSaveHost(t *testing.T) *saveHost {
	t.Helper()
	previous, interval, attempts := saveJobs, savePollInterval, savePollAttempts
	saveJobs = &saveManager{jobs: map[string]*saveJob{}}
	savePollInterval, savePollAttempts = time.Millisecond, 8
	h := &saveHost{config: json.RawMessage(oldSaveConfig), keys: []string{"sk-owner", "sk-old"}}
	var doc map[string]any
	json.Unmarshal(h.config, &doc)
	raw, _ := yaml.Marshal(doc)
	mustConfigure(t, string(raw))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.gate != nil {
			<-h.gate
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if slices.Contains(h.keys, key) || (h.neverRevoke && key == "sk-old") {
				io.WriteString(w, `{"data":[]}`)
			} else {
				w.WriteHeader(401)
				io.WriteString(w, `{}`)
			}
			return
		}
		if r.Method == http.MethodGet {
			if h.readFail {
				w.WriteHeader(503)
				io.WriteString(w, `{}`)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"plugins": map[string]any{"configs": map[string]any{"cpa-carpool": h.config}}, "access": map[string]any{"api-keys": h.keys}})
			return
		}
		body, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/api-keys") {
			var keys []string
			json.Unmarshal(body, &keys)
			if slices.Contains(keys, "sk-new") && h.addFailures > 0 {
				h.addFailures--
				w.WriteHeader(500)
				io.WriteString(w, `{}`)
				return
			}
			if slices.Contains(keys, "sk-new") && activeTable.Load().byKey["sk-new"] == nil {
				t.Error("authorized a key before its routing binding was active")
			}
			h.keys = keys
		} else {
			if slices.Contains(h.keys, "sk-old") || h.neverRevoke {
				t.Error("removed a binding before old-key revocation")
			}
			h.config = slices.Clone(body)
			if !h.neverReload {
				var d map[string]any
				json.Unmarshal(body, &d)
				raw, _ := yaml.Marshal(d)
				mustConfigure(t, string(raw))
			}
		}
		h.writes = append(h.writes, r.URL.Path+" "+string(body))
		io.WriteString(w, `{"status":"ok"}`)
	}))
	t.Setenv("CARPOOL_HOST_URL", server.URL)
	t.Cleanup(func() {
		configMutationMu.Lock()
		configMutationMu.Unlock()
		server.Close()
		saveJobs, savePollInterval, savePollAttempts = previous, interval, attempts
	})
	return h
}

func submitSave(t *testing.T, id, revision, config string) pluginapi.ManagementResponse {
	t.Helper()
	body, _ := json.Marshal(saveInput{RequestID: id, ExpectedRevision: revision, Config: json.RawMessage(config)})
	return configCall(t, http.MethodPost, RouteAdminSave, string(body))
}

func awaitSave(t *testing.T) saveStatus {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r := configCall(t, http.MethodGet, RouteAdminSave, "")
		if strings.Contains(string(r.Body), "sk-") || strings.Contains(string(r.Body), "Bearer") {
			t.Fatalf("job leaked credentials: %s", r.Body)
		}
		var body struct {
			Job saveStatus `json:"job"`
		}
		json.Unmarshal(r.Body, &body)
		if body.Job.State == "failed" || body.Job.State == "succeeded" {
			configMutationMu.Lock()
			configMutationMu.Unlock()
			return body.Job
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("save did not finish")
	return saveStatus{}
}

func TestSaveJobOrdersWritesAndDeduplicates(t *testing.T) {
	h := newSaveHost(t)
	revision := persistedRevision(json.RawMessage(oldSaveConfig), true)
	if r := submitSave(t, "job-one", revision, newSaveConfig); r.StatusCode != 202 {
		t.Fatalf("submit %d %s", r.StatusCode, r.Body)
	}
	if job := awaitSave(t); job.State != "succeeded" {
		t.Fatalf("job %+v", job)
	}
	r := submitSave(t, "job-one", revision, newSaveConfig)
	if r.StatusCode != 200 {
		t.Fatalf("duplicate %d %s", r.StatusCode, r.Body)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.writes) != 3 || !strings.Contains(h.writes[0], `["sk-owner"]`) || !strings.Contains(h.writes[1], "/plugins/configs/cpa-carpool") || !strings.Contains(h.writes[2], `["sk-owner","sk-new"]`) {
		t.Fatalf("write order %v", h.writes)
	}
}

func TestSaveJobRejectsStaleAndChangedRequestIDs(t *testing.T) {
	h := newSaveHost(t)
	submitSave(t, "stale", "wrong", newSaveConfig)
	if job := awaitSave(t); job.ErrorCode != "conflict" || job.Retryable {
		t.Fatalf("job %+v", job)
	}
	if r := submitSave(t, "stale", "different", newSaveConfig); r.StatusCode != 409 {
		t.Fatalf("changed request %d", r.StatusCode)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.writes) != 0 {
		t.Fatal(h.writes)
	}
}

func TestSaveJobFailClosedStages(t *testing.T) {
	for _, scenario := range []string{"read", "last-key", "revoke", "reload"} {
		t.Run(scenario, func(t *testing.T) {
			h := newSaveHost(t)
			h.readFail = scenario == "read"
			h.neverRevoke = scenario == "revoke"
			h.neverReload = scenario == "reload"
			if scenario == "last-key" {
				h.keys = []string{"sk-old"}
			}
			submitSave(t, "failure", persistedRevision(json.RawMessage(oldSaveConfig), true), newSaveConfig)
			job := awaitSave(t)
			if job.State != "failed" {
				t.Fatalf("job %+v", job)
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			if slices.Contains(h.keys, "sk-new") {
				t.Fatal("new key authorized after a failure")
			}
			if (scenario == "read" || scenario == "last-key") && len(h.writes) != 0 {
				t.Fatal(h.writes)
			}
			if scenario == "revoke" && len(h.writes) != 1 {
				t.Fatal(h.writes)
			}
		})
	}
}

func TestSaveJobRetryAfterPartialSave(t *testing.T) {
	h := newSaveHost(t)
	h.addFailures = 1
	submitSave(t, "partial", persistedRevision(json.RawMessage(oldSaveConfig), true), newSaveConfig)
	if job := awaitSave(t); job.State != "failed" || !job.ConfigSaved || !job.Retryable {
		t.Fatalf("job %+v", job)
	}
	r := configCall(t, http.MethodPost, RouteAdminRetry, `{"request_id":"partial"}`)
	if r.StatusCode != 202 {
		t.Fatalf("retry %d %s", r.StatusCode, r.Body)
	}
	if job := awaitSave(t); job.State != "succeeded" {
		t.Fatalf("job %+v", job)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if slices.Contains(h.keys, "sk-old") || !slices.Contains(h.keys, "sk-new") || len(h.writes) != 3 {
		t.Fatalf("keys %v, writes %v", h.keys, h.writes)
	}
}

func TestSaveJobContinuesWithoutBrowserAndSerializesWriters(t *testing.T) {
	h := newSaveHost(t)
	gate := make(chan struct{})
	h.gate = gate
	r := submitSave(t, "background", persistedRevision(json.RawMessage(oldSaveConfig), true), newSaveConfig)
	if r.StatusCode != 202 {
		t.Fatalf("submit %d %s", r.StatusCode, r.Body)
	}
	if r := submitSave(t, "another", persistedRevision(json.RawMessage(oldSaveConfig), true), newSaveConfig); r.StatusCode != 409 {
		t.Fatalf("concurrent job %d", r.StatusCode)
	}
	if r := configCall(t, http.MethodPut, RouteAdminConfig, newSaveConfig); r.StatusCode != 409 {
		t.Fatalf("concurrent PUT %d", r.StatusCode)
	}
	close(gate)
	// No client polling is required for the background job to make progress.
	configMutationMu.Lock()
	configMutationMu.Unlock()
	if job := awaitSave(t); job.State != "succeeded" {
		t.Fatalf("job %+v", job)
	}
}

func TestSaveJobRetryDetectsExternalConfigurationChange(t *testing.T) {
	h := newSaveHost(t)
	h.addFailures = 1
	submitSave(t, "external", persistedRevision(json.RawMessage(oldSaveConfig), true), newSaveConfig)
	awaitSave(t)
	h.mu.Lock()
	h.config = json.RawMessage(`{"enabled":true,"users":[],"rpm":42}`)
	h.mu.Unlock()
	configCall(t, http.MethodPost, RouteAdminRetry, `{"request_id":"external"}`)
	if job := awaitSave(t); job.ErrorCode != "conflict" {
		t.Fatalf("job %+v", job)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if slices.Contains(h.keys, "sk-new") {
		t.Fatal("authorized against externally changed bindings")
	}
}

func TestPausedSeatKeepsBindingAndRejectsAllBuyerPaths(t *testing.T) {
	mustConfigure(t, `enabled: true
users:
  - name: Paused
    paused: true
    api-keys: [sk-paused]
    primary: [alice.json]
`)
	if activeTable.Load().byKey["sk-paused"] == nil || !activeTable.Load().limited {
		t.Fatal("paused seat lost its binding or request hooks")
	}
	got := pick(t, pluginapi.SchedulerOptions{Headers: bearer("sk-paused")}, "alice.json", "other.json")
	if !got.Handled || !got.Reject || got.RejectCode != "seat_paused" {
		t.Fatalf("pick %+v", got)
	}
	if got := intercept(t, "paused", bearer("sk-paused")); !got.Terminate || got.StatusCode != 403 {
		t.Fatalf("intercept %+v", got)
	}
	for _, route := range []string{RouteAPIStatus, RouteAPIReset, RouteAPIMonitor} {
		r := resource(t, base+route, nil, keyHeader("sk-paused"))
		// Reset requires confirmation before looking up the seat.
		if route == RouteAPIReset {
			continue
		}
		if r.StatusCode != 403 {
			t.Fatalf("%s: %d %s", route, r.StatusCode, r.Body)
		}
	}
	if got := pick(t, pluginapi.SchedulerOptions{Headers: bearer("sk-owner")}, "other.json"); got.Handled {
		t.Fatal("owner unexpectedly intercepted")
	}
}

func TestDiagnosticsDoesNotExposeKeys(t *testing.T) {
	newSaveHost(t)
	r := configCall(t, http.MethodGet, RouteAdminDiagnostics, "")
	if r.StatusCode != 200 || strings.Contains(string(r.Body), "sk-old") || !strings.Contains(string(r.Body), "配置生效") {
		t.Fatalf("diagnostics %d %s", r.StatusCode, r.Body)
	}
	_, err := readConfigSnapshot(context.Background(), pluginapi.ManagementRequest{})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSaveRequiresConfirmationBeforeEnablingAuthentication(t *testing.T) {
	h := newSaveHost(t)
	empty := json.RawMessage(`{"enabled":true,"users":[]}`)
	h.config, h.keys = empty, []string{}
	revision := persistedRevision(empty, true)
	submitSave(t, "unconfirmed", revision, newSaveConfig)
	if job := awaitSave(t); job.ErrorCode != "auth_confirmation" {
		t.Fatalf("job %+v", job)
	}
	body, _ := json.Marshal(saveInput{RequestID: "confirmed", ExpectedRevision: revision, Config: json.RawMessage(newSaveConfig), AllowEnableAuth: true})
	if r := configCall(t, http.MethodPost, RouteAdminSave, string(body)); r.StatusCode != 202 {
		t.Fatalf("submit %d %s", r.StatusCode, r.Body)
	}
	if job := awaitSave(t); job.State != "succeeded" {
		t.Fatalf("job %+v", job)
	}
}

func TestPausedSeatResumeRestoresOriginalBinding(t *testing.T) {
	cfg := "enabled: true\nusers:\n  - name: A\n    paused: true\n    api-keys: [sk-a]\n    primary: [alice.json]\n"
	mustConfigure(t, cfg)
	if got := pick(t, pluginapi.SchedulerOptions{Headers: bearer("sk-a")}, "alice.json"); !got.Reject {
		t.Fatal("paused key was accepted")
	}
	mustConfigure(t, strings.Replace(cfg, "paused: true", "paused: false", 1))
	if got := pick(t, pluginapi.SchedulerOptions{Headers: bearer("sk-a")}, "alice.json", "other.json"); !got.Handled || got.Reject || got.AuthID != "alice.json" {
		t.Fatalf("resume %+v", got)
	}
}
