package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type monitorLoadResult struct {
	Name                                                                                         string `json:"name"`
	Rows, Keys, Clients, Requests, Errors, IsolationFailures                                     int
	DataQueries                                                                                  uint64 `json:"data_queries"`
	Seconds, RequestsPerSecond, P50MS, P95MS, P99MS, CPUSeconds, CPUPercent, PeakRSSMiB, HeapMiB float64
	latencies                                                                                    []float64
}

// An explicit path may be reused between runs; only our synthetic fixture with
// matching metadata can be modified. Never use a real Manager database here.
func monitorStressFixture(t *testing.T, rows, keys int) (string, *sql.DB) {
	t.Helper()
	path := os.Getenv("CARPOOL_MONITOR_STRESS_DB")
	if path == "" {
		path = filepath.Join(t.TempDir(), "monitor-load.sqlite")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var tables int
	if err = db.QueryRow("select count(*) from sqlite_master where type='table'").Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables > 0 {
		var gotRows, gotKeys int
		if err = db.QueryRow("select rows,keys from carpool_synthetic_load_fixture").Scan(&gotRows, &gotKeys); err != nil || gotRows != rows || gotKeys != keys {
			t.Fatal("refusing to modify a database without matching synthetic-load metadata")
		}
		return path, db
	}
	_, err = db.Exec(`pragma journal_mode=WAL; pragma synchronous=NORMAL;
		create table stress_keys(id integer primary key,hash text);
		create table usage_events(id integer primary key,timestamp_ms integer,api_key_hash text,
		requested_model text,model text,failed integer,fail_status_code integer,
		input_tokens integer,output_tokens integer,cached_tokens integer,total_tokens integer,
		normalized_total_input_tokens integer,normalized_cache_read_tokens integer,
		latency_ms integer,ttft_ms integer,reasoning_effort text,service_tier text,
		account_snapshot text,auth_index text,raw_json text,fail_summary text);
		create table carpool_synthetic_load_fixture(rows integer,keys integer);`)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < keys; i++ {
		if _, err = tx.Exec("insert into stress_keys values(?,?)", i, usageKeyHash(fmt.Sprintf("sk-demo-load-%03d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`with recursive sequence(n) as (select 0 union all select n+1 from sequence where n+1 < ?)
		insert into usage_events
		select n+1,?-60000-(n*?/?),k.hash,
		printf('tenant-%03d-model-%d',k.id,(n/? )%8),'PRIVATE-RESOLVED',case when (n/?)%10=0 then 1 else 0 end,
		case when (n/?)%10=0 then 429 else null end,
		10,2,70,82,80,50,1000,100,'high','priority','PRIVATE-ACCOUNT','PRIVATE-AUTH',
		'PRIVATE-RAW-'||replace(hex(zeroblob(128)),'0','x'),'PRIVATE-ERROR'
		from sequence join stress_keys k on k.id=sequence.n%?;
		create index idx_usage_events_timestamp on usage_events(timestamp_ms);
		create index idx_usage_events_model on usage_events(model);`, rows, time.Now().UnixMilli(), int64(6*24*time.Hour/time.Millisecond), rows, keys, keys, keys, keys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("insert into carpool_synthetic_load_fixture values(?,?)", rows, keys); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("pragma wal_checkpoint(truncate)"); err != nil {
		t.Fatal(err)
	}
	return path, db
}

func monitorLoadConfig(t *testing.T, path string, keys int) *monitorService {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "enabled: true\ndb-path: %q\nusers:\n", path)
	for i := 0; i < keys; i++ {
		fmt.Fprintf(&b, " - name: load-%03d\n   api-keys: [sk-demo-load-%03d]\n   primary: [claude-demo.json]\n", i, i)
	}
	mustConfigure(t, b.String())
	s := activeTable.Load().monitor
	t.Cleanup(s.close)
	return s
}

func runMonitorLoad(t *testing.T, name, path string, rows, keys, clients, requests, activeKeys int, baseline bool) monitorLoadResult {
	t.Helper()
	s := activeTable.Load().monitor
	before := s.queries.Load()
	baselineBefore := monitorBaselineQueries.Load()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		table := activeTable.Load()
		req := pluginapi.ManagementRequest{Method: http.MethodGet, Path: RouteAPIMonitor, Headers: r.Header, Query: r.URL.Query()}
		var payload []byte
		var err error
		if baseline {
			if buyer, _, _ := lookupSeat(table, r.Header.Get("X-Carpool-Key")); buyer == nil {
				http.Error(w, "unauthorized", 403)
				return
			}
			o, e := parseMonitorOptions(req.Query)
			if e != nil {
				http.Error(w, "bad filters", 400)
				return
			}
			var d MonitorResponse
			d, err = readMonitorBaseline(path, usageKeyHash(r.Header.Get("X-Carpool-Key")), o, time.Now())
			if err == nil {
				payload, err = json.Marshal(d)
			}
		} else {
			var raw []byte
			raw, err = handleAPIMonitor(req, table, table.config)
			if err == nil {
				var env Envelope
				err = json.Unmarshal(raw, &env)
				if err == nil {
					var result pluginapi.ManagementResponse
					err = json.Unmarshal(env.Result, &result)
					if err == nil {
						w.WriteHeader(result.StatusCode)
						payload = result.Body
					}
				}
			}
		}
		if err != nil {
			http.Error(w, "unavailable", 503)
			return
		}
		w.Write(payload)
	}))
	defer server.Close()
	transport := &http.Transport{MaxIdleConns: clients, MaxIdleConnsPerHost: clients}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 8 * time.Second}
	runtime.GC()
	cpuBefore, _ := monitorStressUsage()
	start := time.Now()
	var next atomic.Int64
	var failures, isolation atomic.Int64
	var mu sync.Mutex
	latencies := make([]float64, 0, requests)
	var wg sync.WaitGroup
	for c := 0; c < clients; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= requests {
					return
				}
				keyID := i % activeKeys
				began := time.Now()
				req, _ := http.NewRequest(http.MethodGet, server.URL+"?range=7d&limit=100", nil)
				req.Header.Set("X-Carpool-Key", fmt.Sprintf("sk-demo-load-%03d", keyID))
				r, err := client.Do(req)
				if err != nil {
					failures.Add(1)
				} else {
					body, e := io.ReadAll(r.Body)
					r.Body.Close()
					if e != nil || r.StatusCode != 200 {
						failures.Add(1)
					} else {
						var d MonitorResponse
						if json.Unmarshal(body, &d) != nil || d.Summary.Calls != int64(rows/keys) || d.Summary.InputTokens != int64(rows/keys*80) || d.Summary.CachedTokens != int64(rows/keys*50) || len(d.Events) != 100 {
							isolation.Add(1)
						} else {
							prefix := fmt.Sprintf("tenant-%03d-model-", keyID)
							for _, m := range d.Models {
								if !strings.HasPrefix(m.Model, prefix) {
									isolation.Add(1)
								}
							}
							for _, e := range d.Events {
								if !strings.HasPrefix(e.Model, prefix) {
									isolation.Add(1)
								}
							}
							if strings.Contains(string(body), "PRIVATE-") || strings.Contains(string(body), "sk-demo-load-") {
								isolation.Add(1)
							}
						}
					}
				}
				ms := float64(time.Since(began)) / float64(time.Millisecond)
				mu.Lock()
				latencies = append(latencies, ms)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()
	cpuAfter, rss := monitorStressUsage()
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	sort.Float64s(latencies)
	percentile := func(p float64) float64 { index := int(float64(len(latencies)-1) * p); return latencies[index] }
	queries := s.queries.Load() - before
	if baseline {
		queries = monitorBaselineQueries.Load() - baselineBefore
	}
	result := monitorLoadResult{Name: name, Rows: rows, Keys: keys, Clients: clients, Requests: requests, Errors: int(failures.Load()), IsolationFailures: int(isolation.Load()), DataQueries: queries, Seconds: elapsed, RequestsPerSecond: float64(requests) / elapsed, P50MS: percentile(.5), P95MS: percentile(.95), P99MS: percentile(.99), CPUSeconds: cpuAfter - cpuBefore, CPUPercent: 100 * (cpuAfter - cpuBefore) / elapsed, PeakRSSMiB: float64(rss) / (1 << 20), HeapMiB: float64(memory.HeapAlloc) / (1 << 20)}
	result.latencies = latencies
	data, _ := json.Marshal(result)
	t.Log("MONITOR_LOAD_RESULT " + string(data))
	return result
}

func TestMonitorLargeDataLoad(t *testing.T) {
	if os.Getenv("CARPOOL_MONITOR_STRESS") != "1" {
		t.Skip("set CARPOOL_MONITOR_STRESS=1 to run the million-row HTTP load test")
	}
	rows, keys := 1000000, 100
	if value := os.Getenv("CARPOOL_MONITOR_STRESS_ROWS"); value != "" {
		var err error
		rows, err = strconv.Atoi(value)
		if err != nil || rows < 10000 || rows%keys != 0 {
			t.Fatal("rows must be >=10000 and divisible by 100")
		}
	}
	path, db := monitorStressFixture(t, rows, keys)
	if info, err := os.Stat(path); err == nil {
		t.Logf("Fixture: %d rows, %d keys, %.1f MiB; CPU=%s/%s, logical CPUs=%d", rows, keys, float64(info.Size())/(1<<20), runtime.GOOS, runtime.GOARCH, runtime.NumCPU())
	}
	index, err := os.ReadFile("scripts/usage-monitor-index.sql")
	if err != nil {
		t.Fatal(err)
	}
	var results []monitorLoadResult
	for _, indexed := range []bool{false, true} {
		label := "without-key-index"
		if indexed {
			label = "with-key-index"
			_, err = db.Exec(string(index))
		} else {
			_, err = db.Exec("drop index if exists idx_carpool_usage_key_time")
		}
		if err != nil {
			t.Fatal(err)
		}
		monitorLoadConfig(t, path, keys)
		results = append(results, runMonitorLoad(t, label+"/baseline", path, rows, keys, 8, 24, 8, true))
		s := monitorLoadConfig(t, path, keys)
		clients := 1
		if indexed {
			clients = 50
		}
		cold := runMonitorLoad(t, label+"/cold", path, rows, keys, clients, clients, clients, false)
		results = append(results, cold)
		if indexed && cold.Errors != 0 {
			t.Errorf("indexed cold requests failed: %+v", cold)
		}
		// Warm twenty independent keys, so the hot test cannot pass by sharing one key.
		for i := 0; i < 20; i++ {
			_, err = s.get(activeTable.Load().configRevision, usageKeyHash(fmt.Sprintf("sk-demo-load-%03d", i)), monitorOptions{window: 7 * 24 * time.Hour, rangeName: "7d", status: "all", limit: 100})
			if err != nil {
				t.Fatalf("warmup key %d (%s): %v", i, label, err)
			}
		}
		hot := runMonitorLoad(t, label+"/hot", path, rows, keys, 50, 2000, 20, false)
		results = append(results, hot)
		// Without the index, sequential warmup can take longer than the 10s
		// record TTL. Up to one legitimate record refresh per warmed key is OK.
		if hot.Errors != 0 || hot.IsolationFailures != 0 || hot.DataQueries > 20 || (indexed && hot.DataQueries != 0) {
			t.Errorf("hot-cache correctness/query reduction failed: %+v", hot)
		}
		// Expire only the record cache; the next burst must keep the summaries.
		later := time.Now().Add(11 * time.Second)
		s.responses.mu.Lock()
		s.responses.now = func() time.Time { return later }
		s.responses.mu.Unlock()
		refresh := runMonitorLoad(t, label+"/record-refresh", path, rows, keys, 20, 200, 20, false)
		results = append(results, refresh)
		if refresh.Errors != 0 || refresh.IsolationFailures != 0 || refresh.DataQueries != 20 {
			t.Errorf("record refresh rescanned summaries or leaked data: %+v", refresh)
		}
		if indexed && os.Getenv("CARPOOL_MONITOR_STRESS_SUSTAINED") == "1" {
			results = append(results, runMonitorSustained(t, path, db, rows, keys))
		}
	}
	for _, result := range results {
		if result.IsolationFailures != 0 {
			t.Errorf("privacy/correctness failure: %+v", result)
		}
	}
	if output := os.Getenv("CARPOOL_MONITOR_STRESS_REPORT"); output != "" {
		data, _ := json.MarshalIndent(results, "", "  ")
		if err = os.WriteFile(output, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func runMonitorSustained(t *testing.T, path string, writer *sql.DB, rows, keys int) monitorLoadResult {
	t.Helper()
	s := monitorLoadConfig(t, path, keys)
	for i := 0; i < 20; i++ {
		if _, err := s.get(activeTable.Load().configRevision, usageKeyHash(fmt.Sprintf("sk-demo-load-%03d", i)), monitorOptions{window: 7 * 24 * time.Hour, rangeName: "7d", status: "all", limit: 100}); err != nil {
			t.Fatal(err)
		}
	}
	before := s.queries.Load()
	cpuBefore, _ := monitorStressUsage()
	start := time.Now()
	stop, done := make(chan struct{}), make(chan struct{})
	var writeErrors, writes atomic.Int64
	go func() {
		defer close(done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				_, err := writer.Exec(`insert into usage_events(timestamp_ms,api_key_hash,requested_model,failed,input_tokens,output_tokens,cached_tokens,total_tokens,normalized_total_input_tokens,normalized_cache_read_tokens) values(?,?,'PRIVATE-WRITER-MODEL',0,10,2,0,12,10,0)`, time.Now().UnixMilli(), usageKeyHash("sk-demo-writer-not-a-buyer"))
				if err != nil {
					writeErrors.Add(1)
				} else {
					writes.Add(1)
				}
			}
		}
	}()
	defer func() { close(stop); <-done }()
	result := monitorLoadResult{Name: "with-key-index/35s-polling-with-writer", Rows: rows, Keys: keys, Clients: 50}
	for round := 0; round < 4; round++ {
		if delay := time.Until(start.Add(time.Duration(round) * 10 * time.Second)); delay > 0 {
			time.Sleep(delay)
		}
		batch := runMonitorLoad(t, fmt.Sprintf("poll-round-%d", round), path, rows, keys, 50, 50, 20, false)
		result.Requests += batch.Requests
		result.Errors += batch.Errors
		result.IsolationFailures += batch.IsolationFailures
		result.latencies = append(result.latencies, batch.latencies...)
	}
	if delay := time.Until(start.Add(35 * time.Second)); delay > 0 {
		time.Sleep(delay)
	}
	result.Seconds = time.Since(start).Seconds()
	result.RequestsPerSecond = float64(result.Requests) / result.Seconds
	result.DataQueries = s.queries.Load() - before
	cpuAfter, rss := monitorStressUsage()
	result.CPUSeconds = cpuAfter - cpuBefore
	result.CPUPercent = 100 * result.CPUSeconds / result.Seconds
	result.PeakRSSMiB = float64(rss) / (1 << 20)
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	result.HeapMiB = float64(memory.HeapAlloc) / (1 << 20)
	sort.Float64s(result.latencies)
	result.P50MS = result.latencies[int(float64(len(result.latencies)-1)*.5)]
	result.P95MS = result.latencies[int(float64(len(result.latencies)-1)*.95)]
	result.P99MS = result.latencies[int(float64(len(result.latencies)-1)*.99)]
	data, _ := json.Marshal(result)
	t.Logf("MONITOR_LOAD_RESULT %s; concurrent writes=%d, write errors=%d", data, writes.Load(), writeErrors.Load())
	if result.Errors != 0 || result.IsolationFailures != 0 || writeErrors.Load() != 0 || writes.Load() < 300 || result.DataQueries > 100 {
		t.Errorf("sustained polling failed: %+v, writes=%d, write errors=%d", result, writes.Load(), writeErrors.Load())
	}
	return result
}
