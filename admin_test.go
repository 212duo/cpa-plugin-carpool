package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func adminCall(t *testing.T, method, path string, body []byte) pluginapi.ManagementResponse {
	t.Helper()
	return adminRequest(t, pluginapi.ManagementRequest{Method: method, Path: path, Body: body})
}

func adminRequest(t *testing.T, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	out, err := handleMethod(pluginabi.MethodManagementHandle, raw)
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	if err := json.Unmarshal(out, &env); err != nil || !env.OK {
		t.Fatalf("bad envelope %s: %v", out, err)
	}
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func managementRegistration(t *testing.T) pluginapi.ManagementRegistrationResponse {
	t.Helper()
	out, err := handleMethod(pluginabi.MethodManagementRegister, nil)
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	var reg pluginapi.ManagementRegistrationResponse
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestManagementRegisterListsAdminRoutes(t *testing.T) {
	reg := managementRegistration(t)

	resources := map[string]bool{}
	for _, r := range reg.Resources {
		resources[r.Path] = true
	}
	if !resources[RouteAdmin] {
		t.Error("admin page is not registered as a resource")
	}

	routes := map[string]bool{}
	for _, r := range reg.Routes {
		// CPA turns a GET management route with a Menu into an unauthenticated resource.
		if r.Menu != "" {
			t.Errorf("management route %s has a menu", r.Path)
		}
		routes[r.Method+" "+r.Path] = true
	}
	want := map[string]bool{
		http.MethodGet + " " + RouteAdminOverview:    true,
		http.MethodGet + " " + RouteAdminAuths:       true,
		http.MethodPost + " " + RouteAdminValidate:   true,
		http.MethodGet + " " + RouteAdminConfig:      true,
		http.MethodPut + " " + RouteAdminConfig:      true,
		http.MethodGet + " " + RouteAdminAPIKeys:     true,
		http.MethodPut + " " + RouteAdminAPIKeys:     true,
		http.MethodGet + " " + RouteAdminSave:        true,
		http.MethodPost + " " + RouteAdminSave:       true,
		http.MethodPost + " " + RouteAdminRetry:      true,
		http.MethodGet + " " + RouteAdminDiagnostics: true,
	}
	if !reflect.DeepEqual(routes, want) {
		t.Fatalf("management routes = %v, want %v", routes, want)
	}
}

func TestAdminPageHoldsNoData(t *testing.T) {
	mustConfigure(t, testConfig)

	got := resource(t, base+RouteAdmin, nil, nil)
	if got.StatusCode != http.StatusOK || !strings.Contains(got.Headers.Get("Content-Type"), "text/html") {
		t.Fatalf("admin page: %d %v", got.StatusCode, got.Headers)
	}
	if got.Headers.Get("Content-Security-Policy") == "" || got.Headers.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("admin page headers %v", got.Headers)
	}
	for _, leak := range []string{"sk-alice", "TeamA-1", "codex-alice"} {
		if strings.Contains(string(got.Body), leak) {
			t.Fatalf("admin page embeds %q", leak)
		}
	}
}

func TestAdminAPIIsOnlyReachableAsManagementRoute(t *testing.T) {
	mustConfigure(t, testConfig)
	installFakeHost(t)

	// The same path under the unauthenticated resource prefix must not serve data.
	if got := resource(t, base+RouteAdminOverview, nil, nil); got.StatusCode != http.StatusNotFound {
		t.Fatalf("overview through the resource prefix returned %d", got.StatusCode)
	}
	if got := adminCall(t, http.MethodGet, managementBasePath+"/carpool/nope", nil); got.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown management route returned %d", got.StatusCode)
	}
	if got := adminCall(t, http.MethodPost, managementBasePath+RouteAdminOverview, nil); got.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST overview returned %d", got.StatusCode)
	}
}

func TestAdminOverview(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		create table usage_events (api_key_hash text, input_tokens integer, output_tokens integer, cached_tokens integer, total_tokens integer);
		insert into usage_events values (?, 10, 1, 0, 11), (?, 20, 2, 0, 22), (?, 5, 5, 5, 15);`,
		usageKeyHash("sk-alice"), usageKeyHash("sk-alice-2"), usageKeyHash("sk-bob"))
	db.Close()
	if err != nil {
		t.Fatal(err)
	}

	mustConfigure(t, `
enabled: true
db-path: `+dbPath+`
rpm: 10
users:
  - name: TeamA-1
    api-keys: ["sk-alice", "sk-alice-2"]
    primary: ["Codex-Alice@Example.com-team.json"]
    max-concurrency: 2
  - name: Missing
    api-keys: ["sk-bob"]
    primary: ["codex-gone@example.com.json"]
    expire-at: "2020-01-01T00:00:00Z"
`)
	host := installFakeHost(t)
	host.cooling["1"] = true
	resetLimiter(t)
	intercept(t, "r1", bearer("sk-alice-2"))

	got := adminCall(t, http.MethodGet, managementBasePath+RouteAdminOverview, nil)
	if got.StatusCode != http.StatusOK {
		t.Fatalf("overview %d: %s", got.StatusCode, got.Body)
	}
	if strings.Contains(string(got.Body), "sk-alice") {
		t.Fatalf("overview returns keys: %s", got.Body)
	}
	var ov AdminOverview
	if err := json.Unmarshal(got.Body, &ov); err != nil {
		t.Fatal(err)
	}
	if !ov.Enabled || ov.ConfigError != "" || !ov.UsageDatabase || !ov.UsageAvailable || ov.PluginVersion != PluginVersion || len(ov.Seats) != 2 {
		t.Fatalf("overview %+v", ov)
	}

	a := ov.Seats[0]
	if a.Name != "TeamA-1" || a.Keys != 2 || a.Expired {
		t.Fatalf("seat %+v", a)
	}
	if a.Auth == nil || a.Auth.ID != "codex-alice@example.com-team.json" || a.Auth.Provider != "codex" || !a.Auth.Cooling {
		t.Fatalf("seat auth %+v", a.Auth)
	}
	if want := (SeatLimits{MaxConcurrency: 2, RPM: 10, Active: 1, RecentRequests: 1}); a.Limits != want {
		t.Fatalf("limits %+v, want %+v", a.Limits, want)
	}
	if want := (TokenStats{Calls: 2, InputTokens: 30, OutputTokens: 3, TotalTokens: 33}); a.Usage != want {
		t.Fatalf("usage %+v, want %+v (both keys summed)", a.Usage, want)
	}

	m := ov.Seats[1]
	if m.Auth != nil || !m.Expired || m.ExpiresAt == nil || m.Usage.Calls != 1 {
		t.Fatalf("seat %+v", m)
	}
}

func TestAdminOverviewReportsConfigError(t *testing.T) {
	mustConfigure(t, "enabled: true\nusers:\n  - api_keys: [sk-a]\n")
	got := adminCall(t, http.MethodGet, managementBasePath+RouteAdminOverview, nil)
	var ov AdminOverview
	if err := json.Unmarshal(got.Body, &ov); err != nil {
		t.Fatal(err)
	}
	if got.StatusCode != http.StatusOK || !strings.Contains(ov.ConfigError, "api_keys") {
		t.Fatalf("overview %d %+v", got.StatusCode, ov)
	}
}

func TestAdminAuthsListsBindings(t *testing.T) {
	mustConfigure(t, testConfig)
	host := installFakeHost(t)
	host.entries[0].Email = "malice@example.com"

	got := adminCall(t, http.MethodGet, managementBasePath+RouteAdminAuths, nil)
	if got.StatusCode != http.StatusOK {
		t.Fatalf("auths %d: %s", got.StatusCode, got.Body)
	}
	var body struct {
		Auths []AdminAuth `json:"auths"`
	}
	if err := json.Unmarshal(got.Body, &body); err != nil {
		t.Fatal(err)
	}
	byID := map[string]AdminAuth{}
	for _, a := range body.Auths {
		byID[a.ID] = a
	}
	if len(byID) != 3 {
		t.Fatalf("auths %+v", body.Auths)
	}
	if a := byID["codex-alice@example.com-team.json"]; !reflect.DeepEqual(a.Seats, []string{"TeamA-1"}) || a.Provider != "codex" {
		t.Fatalf("alice %+v", a)
	}
	if a := byID["codex-malice@example.com-team.json"]; len(a.Seats) != 0 || a.Email != "malice@example.com" {
		t.Fatalf("malice %+v", a)
	}
	for _, leak := range []string{"tok-alice", "acct-alice"} {
		if strings.Contains(string(got.Body), leak) {
			t.Fatalf("auths leak %q", leak)
		}
	}

	hostCaller = nil
	if got := adminCall(t, http.MethodGet, managementBasePath+RouteAdminAuths, nil); got.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("auths without host returned %d", got.StatusCode)
	}
}

func TestAdminValidate(t *testing.T) {
	mustConfigure(t, testConfig)

	valid := `{"enabled":true,"priority":100,"store":{"version":"0.4.0"},"rpm":30,"public-base-url":"https://api.example.com",
		"users":[{"name":"A","api-keys":["sk-a"],"primary":["a.json"],"expire-at":"2026-11-15T23:59:59+08:00","max-concurrency":2}]}`
	cases := []struct {
		name      string
		body      string
		wantValid bool
		wantErr   string
	}{
		{name: "valid", body: valid, wantValid: true},
		{name: "misspelled field", body: `{"enabled":true,"users":[{"name":"A","api_keys":["sk-a"],"primary":["a.json"]}]}`, wantErr: "api_keys"},
		{name: "duplicate key", body: `{"enabled":true,"users":[{"api-keys":["sk-a"],"primary":["a.json"]},{"api-keys":["sk-a"],"primary":["b.json"]}]}`, wantErr: "also assigned"},
		{name: "negative limit", body: `{"enabled":true,"users":[{"api-keys":["sk-a"],"primary":["a.json"],"rpm":-1}]}`, wantErr: "rpm"},
		{name: "bad expire-at", body: `{"enabled":true,"users":[{"api-keys":["sk-a"],"primary":["a.json"],"expire-at":"2026-11-15"}]}`, wantErr: "expire-at"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := adminCall(t, http.MethodPost, managementBasePath+RouteAdminValidate, []byte(tt.body))
			var res struct {
				Valid bool   `json:"valid"`
				Error string `json:"error"`
				Seats int    `json:"seats"`
			}
			if err := json.Unmarshal(got.Body, &res); err != nil {
				t.Fatalf("%d %s: %v", got.StatusCode, got.Body, err)
			}
			if got.StatusCode != http.StatusOK || res.Valid != tt.wantValid || !strings.Contains(res.Error, tt.wantErr) {
				t.Fatalf("validate %d %+v", got.StatusCode, res)
			}
			if tt.wantValid && res.Seats != 1 {
				t.Fatalf("seats = %d", res.Seats)
			}
		})
	}

	// Validation never touches the running configuration.
	if len(activeTable.Load().byKey) != 3 {
		t.Fatal("validate replaced the active configuration")
	}
	if got := adminCall(t, http.MethodPost, managementBasePath+RouteAdminValidate, []byte(`[1,2]`)); got.StatusCode != http.StatusBadRequest {
		t.Fatalf("non-object body returned %d", got.StatusCode)
	}
}

func TestConfigFieldsMatchPluginConfig(t *testing.T) {
	known := map[string]bool{}
	typ := reflect.TypeOf(PluginConfig{})
	for i := 0; i < typ.NumField(); i++ {
		known[strings.Split(typ.Field(i).Tag.Get("yaml"), ",")[0]] = true
	}
	for _, f := range pluginRegistration(nil).Metadata.ConfigFields {
		if !known[f.Name] {
			t.Errorf("config field %q is not a PluginConfig field", f.Name)
		}
		if f.Description == "" {
			t.Errorf("config field %q has no description", f.Name)
		}
	}
}

func TestAdminConfigRevisionTracksRoutingChanges(t *testing.T) {
	// Host-owned fields and YAML formatting do not affect the routing revision.
	a := register(t, testConfig)
	if !a.Capabilities.ManagementAPI {
		t.Fatal("management API is unavailable")
	}
	first := activeTable.Load().configRevision
	if len(first) != 64 {
		t.Fatalf("missing configuration revision: %q", first)
	}
	metadataChanged := strings.ReplaceAll(testConfig, "priority: 100", "priority: 123")
	metadataChanged = strings.ReplaceAll(metadataChanged, `version: "0.2.0"`, `version: "0.4.0"`)
	register(t, metadataChanged)
	if got := activeTable.Load().configRevision; got != first {
		t.Fatalf("host metadata changed routing revision: %s != %s", got, first)
	}
	register(t, strings.ReplaceAll(testConfig, "sk-alice", "sk-replaced"))
	if activeTable.Load().configRevision == first {
		t.Fatal("key changes did not change the routing revision")
	}
	got := adminCall(t, http.MethodPost, managementBasePath+RouteAdminValidate, []byte(`{"enabled":true,"users":[{"name":"A","api-keys":["sk-a"],"primary":["a.json"]}]}`))
	var validated struct {
		ConfigRevision string `json:"config_revision"`
	}
	if err := json.Unmarshal(got.Body, &validated); err != nil {
		t.Fatal(err)
	}
	register(t, "enabled: true\nusers:\n  - name: A\n    api-keys: [sk-a]\n    primary: [a.json]\n")
	if validated.ConfigRevision != activeTable.Load().configRevision {
		t.Fatal("validated revision differs from the applied revision")
	}
}
