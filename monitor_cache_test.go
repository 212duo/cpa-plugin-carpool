package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMonitorCacheCoalescesSameKeyAndOwnsBytes(t *testing.T) {
	c := newMonitorByteCache(time.Minute, 4, 1024)
	var loads atomic.Int64
	key := monitorCacheKey{keyHash: usageKeyHash("sk-demo-one")}
	start, release := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			data, err := c.get(context.Background(), key, func() ([]byte, error) { loads.Add(1); <-release; return []byte("own-data"), nil })
			if err != nil || string(data) != "own-data" {
				t.Errorf("coalesced response: %q %v", data, err)
			} else {
				data[0] = 'X'
			}
		}()
	}
	close(start)
	close(release)
	wg.Wait()
	if loads.Load() != 1 {
		t.Fatalf("same-key stampede performed %d loads", loads.Load())
	}
	data, err := c.get(context.Background(), key, func() ([]byte, error) { t.Error("unexpected reload"); return nil, nil })
	if err != nil || string(data) != "own-data" {
		t.Fatal("response bytes shared mutable storage")
	}
}

func TestMonitorCacheBoundsExpiryErrorsAndCancellation(t *testing.T) {
	c := newMonitorByteCache(time.Second, 2, 12)
	now := time.Now()
	c.now = func() time.Time { return now }
	var loads int
	load := func() ([]byte, error) { loads++; return []byte("12345"), nil }
	for _, hash := range []string{"one", "two", "three"} {
		if _, err := c.get(context.Background(), monitorCacheKey{keyHash: hash}, load); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.entries) > 2 || c.bytes > 12 {
		t.Fatal("cache grew past its bounds")
	}
	now = now.Add(2 * time.Second)
	if _, err := c.get(context.Background(), monitorCacheKey{keyHash: "three"}, load); err != nil || loads != 4 {
		t.Fatal("expired data was reused")
	}
	key := monitorCacheKey{keyHash: "error"}
	sentinel := errors.New("unavailable")
	if _, err := c.get(context.Background(), key, func() ([]byte, error) { return nil, sentinel }); err != sentinel {
		t.Fatal(err)
	}
	if _, err := c.get(context.Background(), key, load); err != nil || loads != 5 {
		t.Fatal("errors were cached")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.get(ctx, key, load); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled caller received cached data")
	}
	c.close()
	if _, err := c.get(context.Background(), key, load); err == nil || len(c.entries) != 0 {
		t.Fatal("closed cache served data")
	}
}

func TestMonitorCacheCloseCannotBeRefilledByAnEarlierLoader(t *testing.T) {
	c := newMonitorByteCache(time.Minute, 4, 1024)
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		data, err := c.get(context.Background(), monitorCacheKey{keyHash: usageKeyHash("sk-demo")}, func() ([]byte, error) { close(started); <-release; return []byte("old-configuration-data"), nil })
		if len(data) != 0 {
			done <- errors.New("retired cache returned private data")
			return
		}
		done <- err
	}()
	<-started
	c.close()
	close(release)
	if err := <-done; err == nil || len(c.entries) != 0 {
		t.Fatal("earlier loader repopulated a retired cache")
	}
}

func TestMonitorRevocationCancelsAnInFlightCacheMiss(t *testing.T) {
	monitorFixture(t)
	old := activeTable.Load()
	// Keep the query waiting without relying on a slow database or timing races.
	monitorQuerySlots <- struct{}{}
	monitorQuerySlots <- struct{}{}
	defer func() { <-monitorQuerySlots; <-monitorQuerySlots }()
	done := make(chan int, 1)
	go func() { _, r := monitorQuery(t, "sk-first", nil); done <- r.StatusCode }()
	deadline := time.Now().Add(time.Second)
	for {
		old.monitor.responses.mu.Lock()
		loading := len(old.monitor.responses.flights) > 0
		old.monitor.responses.mu.Unlock()
		if loading {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("request did not enter the cache loader")
		}
		time.Sleep(time.Millisecond)
	}
	mustConfigure(t, "enabled: false")
	select {
	case status := <-done:
		if status == 200 {
			t.Fatal("revoked key received an in-flight response")
		}
	case <-time.After(time.Second):
		t.Fatal("reconfiguration did not cancel the waiting query")
	}
}

func TestMonitorCachedAPISeparatesKeysAndDeduplicatesQueries(t *testing.T) {
	monitorFixture(t)
	service := activeTable.Load().monitor
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key, model := "sk-first", "gpt-demo"
			if i%2 == 1 {
				key = "sk-second"
				model = "OTHER-KEY-MODEL"
			}
			d, r := monitorQuery(t, key, nil)
			if r.StatusCode != 200 || len(d.Models) != 1 || d.Models[0].Model != model {
				t.Errorf("cross-key cache response: %s", r.Body)
			}
		}(i)
	}
	wg.Wait()
	if got := service.queries.Load(); got != 4 {
		t.Fatalf("two verified keys should cause four SQL queries, got %d", got)
	}
	_, r := monitorQuery(t, "sk-first", nil)
	if strings.Contains(string(r.Body), "OTHER-KEY") || strings.Contains(string(r.Body), "PRIVATE-") {
		t.Fatal("cached data leaked another key or raw fields")
	}
	_, r = monitorQuery(t, "sk-first", map[string][]string{"limit": {"1"}})
	if r.StatusCode != 200 || service.queries.Load() != 5 {
		t.Fatal("limits should share only the same key's aggregate")
	}
	_, r = monitorQuery(t, "sk-first", map[string][]string{"status": {"success"}})
	if r.StatusCode != 200 || service.queries.Load() != 7 {
		t.Fatal("status filters incorrectly shared cached data")
	}
	_, r = monitorQuery(t, "sk-first", map[string][]string{"range": {"1h"}})
	if r.StatusCode != 200 || service.queries.Load() != 9 {
		t.Fatal("time ranges incorrectly shared cached data")
	}
}

func TestMonitorSummaryRefreshesLessOftenWithoutHidingItsTimestamp(t *testing.T) {
	_, db := monitorFixture(t)
	service := activeTable.Load().monitor
	now := time.Now()
	service.responses.now = func() time.Time { return now }
	service.summaries.now = func() time.Time { return now }
	first, r := monitorQuery(t, "sk-first", nil)
	if r.StatusCode != 200 {
		t.Fatal(string(r.Body))
	}
	_, err := db.Exec(`insert into usage_events(timestamp_ms,api_key_hash,requested_model,failed,input_tokens,output_tokens,cached_tokens,total_tokens) values(?,?,'gpt-demo',0,3,2,1,5)`, time.Now().Add(-time.Second).UnixMilli(), usageKeyHash("sk-first"))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(11 * time.Second)
	d, r := monitorQuery(t, "sk-first", nil)
	if r.StatusCode != 200 || len(d.Events) != 3 || d.Summary.Calls != 2 || d.SummaryAtMS != first.SummaryAtMS || service.queries.Load() != 3 {
		t.Fatalf("record refresh unexpectedly rescanned summary: %+v", d)
	}
	now = now.Add(20 * time.Second)
	d, r = monitorQuery(t, "sk-first", nil)
	if r.StatusCode != 200 || d.Summary.Calls != 3 || service.queries.Load() != 5 {
		t.Fatalf("summary did not refresh: %+v", d)
	}
}

func TestMonitorResponseCacheCannotExtendSummaryLifetime(t *testing.T) {
	monitorFixture(t)
	s := activeTable.Load().monitor
	now := time.Now()
	s.responses.now = func() time.Time { return now }
	s.summaries.now = func() time.Time { return now }
	d, r := monitorQuery(t, "sk-first", nil)
	if r.StatusCode != 200 {
		t.Fatal(string(r.Body))
	}
	now = time.UnixMilli(d.SummaryAtMS).Add(29 * time.Second)
	monitorQuery(t, "sk-first", nil)
	limit := time.UnixMilli(d.SummaryAtMS).Add(monitorSummaryTTL)
	for _, entry := range s.responses.entries {
		if entry.expires.After(limit) {
			t.Fatal("response cache extended the summary's lifetime")
		}
	}
	now = limit.Add(time.Millisecond)
	monitorQuery(t, "sk-first", nil)
	if s.queries.Load() != 5 {
		t.Fatalf("expired summary should reload even if records were just cached: %d", s.queries.Load())
	}
}

func TestMonitorWarmCacheCannotBypassRevocationExpiryOrReconfigure(t *testing.T) {
	monitorFixture(t)
	old := activeTable.Load()
	monitorQuery(t, "sk-first", nil)
	for _, cfg := range []string{"enabled: false", "enabled: true\nunknown: true", "enabled: true\nusers:\n - name: expired\n   api-keys: [sk-first]\n   primary: [claude-demo.json]\n   expire-at: '2020-01-01T00:00:00Z'"} {
		mustConfigure(t, cfg)
		_, r := monitorQuery(t, "sk-first", nil)
		if r.StatusCode == 200 || strings.Contains(string(r.Body), "summary") {
			t.Fatal("warm cache bypassed current authorization")
		}
	}
	if _, err := old.monitor.get(old.configRevision, usageKeyHash("sk-first"), monitorOptions{window: time.Hour, rangeName: "1h", status: "all", limit: 100}); err == nil {
		t.Fatal("retired configuration returned data")
	}
	if len(old.monitor.responses.entries) != 0 || len(old.monitor.summaries.entries) != 0 {
		t.Fatal("old configuration retained cached results")
	}
}

func TestMonitorConfigurationAndDatabaseIdentityAreCacheBoundaries(t *testing.T) {
	path, db := monitorFixture(t)
	old := activeTable.Load()
	monitorQuery(t, "sk-first", nil)
	_, err := db.Exec(`delete from usage_events where api_key_hash=?`, usageKeyHash("sk-first"))
	if err != nil {
		t.Fatal(err)
	}
	// Even the same parsed configuration creates a new cache generation.
	mustConfigure(t, "enabled: true\ndb-path: "+fmt.Sprintf("%q", path)+"\nusers:\n - name: first\n   api-keys: [sk-first]\n   primary: [claude-demo.json]")
	current := activeTable.Load()
	t.Cleanup(current.monitor.close)
	d, r := monitorQuery(t, "sk-first", nil)
	if r.StatusCode != 200 || d.Summary.Calls != 0 || len(d.Events) != 0 {
		t.Fatalf("reconfiguration reused old results: %s", r.Body)
	}
	if old.monitor == current.monitor {
		t.Fatal("service reused across configuration snapshots")
	}
	// Database removal must be detected even while response cache is warm.
	current.monitor.mu.Lock()
	current.monitor.database.db.Close()
	current.monitor.mu.Unlock()
	db.Close()
	if err := os.Rename(path, filepath.Join(filepath.Dir(path), "retired.sqlite")); err != nil {
		t.Fatal(err)
	}
	_, r = monitorQuery(t, "sk-first", nil)
	if r.StatusCode != 503 {
		t.Fatal("cache served data after database removal")
	}
	fresh, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	_, err = fresh.Exec(`create table usage_events (id integer primary key,timestamp_ms integer,api_key_hash text,requested_model text,failed integer,input_tokens integer,output_tokens integer,cached_tokens integer,total_tokens integer); insert into usage_events values (1,?,?,'replacement-model',0,5,2,0,7)`, time.Now().Add(-time.Second).UnixMilli(), usageKeyHash("sk-first"))
	if err != nil {
		t.Fatal(err)
	}
	d, r = monitorQuery(t, "sk-first", nil)
	if r.StatusCode != 200 || d.Summary.TotalTokens != 7 || d.Events[0].Model != "replacement-model" {
		t.Fatalf("cache survived database replacement: %s", r.Body)
	}
}

func TestMonitorAggregateIncludesModelsOutsideTopFifty(t *testing.T) {
	_, db := monitorFixture(t)
	for i := 0; i < 60; i++ {
		_, err := db.Exec(`insert into usage_events(timestamp_ms,api_key_hash,requested_model,failed,input_tokens,output_tokens,cached_tokens,total_tokens,latency_ms) values(?,?,?,0,3,2,1,5,2000)`, time.Now().Add(-time.Second).UnixMilli(), usageKeyHash("sk-first"), fmt.Sprintf("model-%02d", i))
		if err != nil {
			t.Fatal(err)
		}
	}
	d, r := monitorQuery(t, "sk-first", nil)
	if r.StatusCode != 200 || len(d.Models) != 50 || d.Summary.Calls != 62 || d.Summary.TotalTokens != 432 || d.Summary.AvgLatencyMS == nil || *d.Summary.AvgLatencyMS != float64(121000)/61 {
		t.Fatalf("top-model limit lost totals or weighted latency: %s", r.Body)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(r.Body, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire["summary_at_ms"]) == 0 {
		t.Fatal("separately cached summary needs its own timestamp")
	}
}

func TestMonitorIndexScriptIsIdempotentAndUsed(t *testing.T) {
	_, db := monitorFixture(t)
	if _, err := db.Exec("drop index scope_time"); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile("scripts/usage-monitor-index.sql")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err = db.Exec(string(script)); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.Query(`explain query plan select id from usage_events where api_key_hash=? and timestamp_ms>=? and timestamp_ms<=? order by timestamp_ms desc,id desc limit 100`, usageKeyHash("sk-first"), 0, time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail
	}
	if !strings.Contains(plan, "idx_carpool_usage_key_time") || strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("inefficient query plan: %s", plan)
	}
}
