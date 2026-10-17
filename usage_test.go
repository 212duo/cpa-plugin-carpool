package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsageAPIReportsAvailabilityAndKeepsKeyScope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`create table usage_events (api_key_hash text, input_tokens integer, output_tokens integer, cached_tokens integer, total_tokens integer);
		insert into usage_events values (?, 10, 1, 0, 11), (?, 20, 2, 0, 22), (?, 90, 9, 0, 99);`,
		usageKeyHash("sk-first"), usageKeyHash("sk-second"), usageKeyHash("sk-other"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	mustConfigure(t, "enabled: true\ndb-path: "+path+`
users:
  - name: shared
    api-keys: [sk-first, sk-second, sk-unused]
    primary: [claude-carol@example.com.json]
  - name: other
    api-keys: [sk-other]
    primary: [claude-carol@example.com.json]
`)
	installFakeHost(t)
	query := func(key string) BuyerStatusResponse {
		t.Helper()
		got := resource(t, base+RouteAPIStatus, nil, keyHeader(key))
		var status BuyerStatusResponse
		if got.StatusCode != http.StatusOK {
			t.Fatalf("status %d: %s", got.StatusCode, got.Body)
		}
		if err := json.Unmarshal(got.Body, &status); err != nil {
			t.Fatal(err)
		}
		return status
	}
	if got := query("sk-first"); !got.UsageAvailable || got.LocalCalls != 1 || got.TotalTokens != 11 {
		t.Fatalf("Portal must show only the current key: %+v", got)
	}
	if got := query("sk-unused"); !got.UsageAvailable || got.LocalCalls != 0 {
		t.Fatalf("real zero usage: %+v", got)
	}
	got := adminCall(t, http.MethodGet, managementBasePath+RouteAdminOverview, nil)
	var ov AdminOverview
	if err := json.Unmarshal(got.Body, &ov); err != nil {
		t.Fatal(err)
	}
	if !ov.UsageAvailable || ov.Seats[0].Usage.Calls != 2 || ov.Seats[0].Usage.TotalTokens != 33 {
		t.Fatalf("admin must aggregate the seat's keys only: %+v", ov)
	}
	if _, err := db.Exec("drop table usage_events"); err != nil {
		t.Fatal(err)
	}
	if got := query("sk-first"); got.UsageAvailable {
		t.Fatal("a missing table was reported as valid usage")
	}
	response := adminCall(t, http.MethodGet, managementBasePath+RouteAdminOverview, nil)
	if err := json.Unmarshal(response.Body, &ov); err != nil {
		t.Fatal(err)
	}
	if ov.UsageAvailable || !ov.UsageDatabase {
		t.Fatalf("found but unreadable database: %+v", ov)
	}
	if strings.Contains(string(response.Body), path) {
		t.Fatal("database errors must not expose file paths")
	}
}
