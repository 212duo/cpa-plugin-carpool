package main

import (
	"database/sql"
	"encoding/json"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func monitorFixture(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage #monitor.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`create table usage_events (
		id integer primary key, timestamp_ms integer, api_key_hash text,
		model text, requested_model text, failed integer, fail_status_code integer,
		input_tokens integer default 0, output_tokens integer default 0, cached_tokens integer default 0, total_tokens integer default 0,
		normalized_total_input_tokens integer, normalized_cache_read_tokens integer,
		reasoning_effort text, service_tier text, latency_ms integer, ttft_ms integer,
		account_snapshot text, auth_index text, source text, raw_json text, fail_body text, fail_summary text,
		client_ip text, header_trace_id text, response_metadata_json text);
		create index scope_time on usage_events(api_key_hash,timestamp_ms);`)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(-time.Minute).UnixMilli()
	for i, key := range []string{"sk-first", "sk-second", "sk-other"} {
		_, err = db.Exec(`insert into usage_events (id,timestamp_ms,api_key_hash,model,requested_model,failed,fail_status_code,
			input_tokens,output_tokens,cached_tokens,total_tokens,normalized_total_input_tokens,normalized_cache_read_tokens,
			reasoning_effort,service_tier,latency_ms,ttft_ms,account_snapshot,auth_index,source,raw_json,fail_body,fail_summary,client_ip,header_trace_id,response_metadata_json)
			values (?,?,?,?,?, ?,429, 10,2,70,82,80,50, 'high','priority',1000,100,
			'PRIVATE-ACCOUNT','PRIVATE-AUTH','PRIVATE-SOURCE','PRIVATE-RAW','PRIVATE-BODY','PRIVATE-ERROR','PRIVATE-IP','PRIVATE-TRACE','PRIVATE-METADATA')`,
			i+1, now+int64(i), usageKeyHash(key), "PRIVATE-RESOLVED", []string{"gpt-demo", "OTHER-KEY-MODEL", "OTHER-SEAT-MODEL"}[i], i%2)
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = db.Exec(`insert into usage_events (timestamp_ms,api_key_hash,requested_model,failed,input_tokens,output_tokens,cached_tokens,total_tokens,latency_ms,ttft_ms,reasoning_effort,service_tier,fail_status_code)
		values (?,?,'gpt-demo',1,20,0,30,50,-1,999999999,'PRIVATE-REASONING','PRIVATE-TIER',999);`, now-1000, usageKeyHash("sk-first"))
	if err != nil {
		t.Fatal(err)
	}
	mustConfigure(t, "enabled: true\ndb-path: "+strconv.Quote(path)+`
users:
  - name: shared
    api-keys: [sk-first, sk-second, sk-empty]
    primary: [claude-demo.json]
  - name: other
    api-keys: [sk-other]
    primary: [claude-demo.json]
  - name: expired
    api-keys: [sk-expired]
    primary: [claude-demo.json]
    expire-at: '2020-01-01T00:00:00Z'
`)
	return path, db
}

func monitorQuery(t *testing.T, key string, q url.Values) (MonitorResponse, pluginapi.ManagementResponse) {
	t.Helper()
	r := resource(t, base+RouteAPIMonitor, q, keyHeader(key))
	var d MonitorResponse
	if r.StatusCode == 200 {
		if err := json.Unmarshal(r.Body, &d); err != nil {
			t.Fatal(err)
		}
	}
	return d, r
}

func TestMonitorIsolatesEveryQueryAndWhitelistsOutput(t *testing.T) {
	path, _ := monitorFixture(t)
	o, _ := parseMonitorOptions(nil)
	if _, err := readMonitor(path, usageKeyHash("sk-first"), o, time.Now()); err != nil {
		t.Fatal(err)
	}
	d, r := monitorQuery(t, "sk-first", nil)
	if r.StatusCode != 200 {
		t.Fatalf("%d: %s", r.StatusCode, r.Body)
	}
	if d.Summary.Calls != 2 || d.Summary.Successes != 1 || d.Summary.Failures != 1 || d.Summary.InputTokens != 100 || d.Summary.CachedTokens != 80 || d.Summary.TotalTokens != 132 {
		t.Fatalf("unexpected summary: %+v", d.Summary)
	}
	if len(d.Models) != 1 || d.Models[0].Calls != 2 || len(d.Events) != 2 || d.Events[0].Model != "gpt-demo" {
		t.Fatalf("wrong scope: %+v", d)
	}
	if d.Summary.AvgLatencyMS == nil || *d.Summary.AvgLatencyMS != 1000 || d.Events[1].LatencyMS != nil || d.Events[1].TTFTMS != nil || d.Events[1].Reasoning != "" || d.Events[1].ServiceTier != "" || d.Events[1].StatusCode != nil {
		t.Fatalf("unsafe optional fields: %+v", d)
	}
	body := string(r.Body)
	for _, secret := range []string{"PRIVATE-", "OTHER-", "sk-first", "sk-second", usageKeyHash("sk-first"), "api_key_hash", "auth_index", "request_id", "raw_json", "fail_summary", "client_ip"} {
		if strings.Contains(body, secret) {
			t.Fatalf("leaked %q: %s", secret, body)
		}
	}
	if r.Headers.Get("Cache-Control") != "no-store" {
		t.Fatal("buyer data must not be cached")
	}
	for _, tc := range []struct {
		key   string
		calls int64
		model string
	}{{"sk-second", 1, "OTHER-KEY-MODEL"}, {"sk-other", 1, "OTHER-SEAT-MODEL"}, {"sk-empty", 0, ""}} {
		d, r = monitorQuery(t, tc.key, nil)
		if r.StatusCode != 200 || d.Summary.Calls != tc.calls || len(d.Events) != int(tc.calls) {
			t.Fatalf("scope %s: %+v %s", tc.key, d, r.Body)
		}
		if tc.calls > 0 && d.Events[0].Model != tc.model {
			t.Fatal("another key's model returned")
		}
	}
}

func TestMonitorFiltersApplyToSummaryModelsAndEvents(t *testing.T) {
	_, db := monitorFixture(t)
	_, err := db.Exec(`insert into usage_events (timestamp_ms,api_key_hash,requested_model,failed,total_tokens) values (?,?,'old',0,999),(?,?,'future',0,999)`, time.Now().Add(-48*time.Hour).UnixMilli(), usageKeyHash("sk-first"), time.Now().Add(24*time.Hour).UnixMilli(), usageKeyHash("sk-first"))
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"success", "failed"} {
		d, r := monitorQuery(t, "sk-first", url.Values{"range": {"1h"}, "status": {status}, "limit": {"1"}})
		if r.StatusCode != 200 || d.Summary.Calls != 1 || len(d.Models) != 1 || d.Models[0].Calls != 1 || len(d.Events) != 1 || d.Events[0].Failed != (status == "failed") || d.Truncated {
			t.Fatalf("filter %s: %+v %s", status, d, r.Body)
		}
	}
	d, r := monitorQuery(t, "sk-first", url.Values{"limit": {"1"}})
	if r.StatusCode != 200 || d.Summary.Calls != 2 || len(d.Events) != 1 || !d.Truncated {
		t.Fatal("bounded rows must not truncate totals")
	}
	d, _ = monitorQuery(t, "sk-first", url.Values{"range": {"7d"}})
	if d.Summary.Calls != 3 {
		t.Fatalf("seven day range: %+v", d)
	}
}

func TestMonitorRejectsIdentitySelectorsAndInvalidCredentials(t *testing.T) {
	monitorFixture(t)
	for _, tc := range []struct {
		key  string
		want int
	}{{"", 400}, {"sk-owner", 404}, {"sk-expired", 403}} {
		_, r := monitorQuery(t, tc.key, nil)
		if r.StatusCode != tc.want {
			t.Fatalf("%q: %d", tc.key, r.StatusCode)
		}
	}
	for _, q := range []url.Values{
		{"key": {"sk-other"}}, {"api_key_hash": {usageKeyHash("sk-other")}}, {"seat": {"other"}}, {"auth_id": {"claude-demo.json"}},
		{"range": {"all"}}, {"status": {"all OR 1=1"}}, {"limit": {"501"}}, {"limit": {"0"}}, {"status": {"all", "failed"}},
	} {
		_, r := monitorQuery(t, "sk-first", q)
		if r.StatusCode != 400 {
			t.Fatalf("accepted %v: %d", q, r.StatusCode)
		}
	}
	r := resource(t, base+RouteAPIMonitor, url.Values{"key": {"sk-first"}}, nil)
	if r.StatusCode != 400 {
		t.Fatal("query credentials must not authenticate")
	}
	mustConfigure(t, "enabled: true\nunknown: value")
	_, r = monitorQuery(t, "sk-first", nil)
	if r.StatusCode != 503 {
		t.Fatal("invalid config must fail closed")
	}
}

func TestMonitorUnavailableNeverLeaksErrorsOrPartialData(t *testing.T) {
	path, db := monitorFixture(t)
	if _, err := db.Exec(`update usage_events set output_tokens='PRIVATE-INVALID-TOKEN' where id=1`); err != nil {
		t.Fatal(err)
	}
	_, r := monitorQuery(t, "sk-first", nil)
	if r.StatusCode != 503 || strings.Contains(string(r.Body), "PRIVATE") || strings.Contains(string(r.Body), path) || strings.Contains(string(r.Body), "summary") {
		t.Fatalf("unsafe failure: %s", r.Body)
	}
	if _, err := db.Exec(`drop table usage_events`); err != nil {
		t.Fatal(err)
	}
	_, r = monitorQuery(t, "sk-first", nil)
	if r.StatusCode != 503 {
		t.Fatal("missing table reported as zero")
	}
}

func TestMonitorLegacySchemaDoesNotRevealResolvedModels(t *testing.T) {
	path, db := monitorFixture(t)
	_, err := db.Exec(`drop table usage_events;
		create table usage_events (id integer primary key,timestamp_ms integer,api_key_hash text,model text,failed integer,input_tokens integer,output_tokens integer,cached_tokens integer,total_tokens integer);
		insert into usage_events values (1,?,?,'PRIVATE-INTERNAL-ALIAS',0,3,2,1,5);`, time.Now().Add(-time.Second).UnixMilli(), usageKeyHash("sk-first"))
	if err != nil {
		t.Fatal(err)
	}
	d, r := monitorQuery(t, "sk-first", nil)
	if r.StatusCode != 200 || d.Summary.TotalTokens != 5 || d.Events[0].Model != "未记录模型" || d.Events[0].LatencyMS != nil {
		t.Fatalf("legacy schema: %+v %s", d, r.Body)
	}
	// Opening the database for monitoring must never make it writable.
	ro, err := openUsageDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err = ro.Exec(`delete from usage_events`); err == nil {
		t.Fatal("usage database was opened writable")
	}
}

func TestMonitorPublicPageAndSafeModelNames(t *testing.T) {
	r := resource(t, base+RouteMonitor, nil, nil)
	if r.StatusCode != 200 || r.Headers.Get("X-Frame-Options") != "DENY" || !strings.Contains(r.Headers.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatal("monitor resource or security headers missing")
	}
	for _, v := range []string{"demo@example.test", "sk-secret-key", "<img src=x>", strings.Repeat("x", 129), "../../auth.json", ""} {
		if safeRequestedModel(v) != "未记录模型" {
			t.Fatalf("unsafe model %q", v)
		}
	}
	if safeRequestedModel("vendor/gpt-demo:thinking") != "vendor/gpt-demo:thinking" {
		t.Fatal("valid model rejected")
	}
}
