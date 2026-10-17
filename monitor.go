package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Monitor responses deliberately contain no account identity, row/request IDs,
// key hashes, headers, request/response bodies or free-form failure messages.
type MonitorTotals struct {
	Calls        int64    `json:"calls"`
	Successes    int64    `json:"successes"`
	Failures     int64    `json:"failures"`
	InputTokens  int64    `json:"input_tokens"`
	OutputTokens int64    `json:"output_tokens"`
	CachedTokens int64    `json:"cached_tokens"`
	TotalTokens  int64    `json:"total_tokens"`
	AvgLatencyMS *float64 `json:"avg_latency_ms"`
}

type MonitorModel struct {
	Model string `json:"model"`
	MonitorTotals
}

type MonitorEvent struct {
	TimestampMS  int64    `json:"timestamp_ms"`
	Model        string   `json:"model"`
	Reasoning    string   `json:"reasoning"`
	ServiceTier  string   `json:"service_tier"`
	Failed       bool     `json:"failed"`
	StatusCode   *int64   `json:"status_code"`
	InputTokens  int64    `json:"input_tokens"`
	OutputTokens int64    `json:"output_tokens"`
	CachedTokens int64    `json:"cached_tokens"`
	TotalTokens  int64    `json:"total_tokens"`
	LatencyMS    *float64 `json:"latency_ms"`
	TTFTMS       *float64 `json:"ttft_ms"`
}

type MonitorResponse struct {
	SummaryAtMS   int64          `json:"summary_at_ms"`
	GeneratedAtMS int64          `json:"generated_at_ms"`
	Range         string         `json:"range"`
	Status        string         `json:"status"`
	Summary       MonitorTotals  `json:"summary"`
	Models        []MonitorModel `json:"models"`
	Events        []MonitorEvent `json:"events"`
	Truncated     bool           `json:"truncated"`
}

type monitorOptions struct {
	window            time.Duration
	rangeName, status string
	limit             int
}

func parseMonitorOptions(q url.Values) (monitorOptions, error) {
	o := monitorOptions{window: 24 * time.Hour, rangeName: "24h", status: "all", limit: 100}
	for k, values := range q {
		if len(values) != 1 {
			return o, errors.New("invalid monitor filters")
		}
		switch k {
		case "range":
			o.rangeName = values[0]
			switch values[0] {
			case "1h":
				o.window = time.Hour
			case "24h":
			case "7d":
				o.window = 7 * 24 * time.Hour
			default:
				return o, errors.New("invalid range")
			}
		case "status":
			o.status = values[0]
			if o.status != "all" && o.status != "success" && o.status != "failed" {
				return o, errors.New("invalid status")
			}
		case "limit":
			n, err := strconv.Atoi(values[0])
			if err != nil || n < 1 || n > 500 {
				return o, errors.New("limit must be between 1 and 500")
			}
			o.limit = n
		default:
			return o, errors.New("unsupported monitor filter")
		}
	}
	return o, nil
}

func handleAPIMonitor(req pluginapi.ManagementRequest, table *routingTable, cfg PluginConfig) ([]byte, error) {
	if req.Method != "" && req.Method != http.MethodGet {
		return jsonResponse(http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
	// No legacy query-string credentials or caller-selected identity on this API.
	key := strings.TrimSpace(req.Headers.Get("X-Carpool-Key"))
	if buyer, status, message := lookupSeat(table, key); buyer == nil {
		return jsonResponse(status, map[string]string{"error": message})
	}
	o, err := parseMonitorOptions(req.Query)
	if err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	result, err := table.monitor.get(table.configRevision, usageKeyHash(key), o)
	if err != nil {
		// Do not log SQLite errors: an invalid cell may put private values in errors.
		logf("warn", "request monitoring data is unavailable")
		return jsonResponse(http.StatusServiceUnavailable, map[string]string{"error": "请求记录暂不可用，请联系车主检查用量采集与数据库配置"})
	}
	// Authentication is never cached. Reject revoked or expired keys even if a
	// query or cache hit began before the routing configuration changed.
	current := activeTable.Load()
	if buyer, status, message := lookupSeat(current, key); buyer == nil {
		return jsonResponse(status, map[string]string{"error": message})
	}
	if current != table {
		return jsonResponse(http.StatusServiceUnavailable, map[string]string{"error": "配置已更新，请重试"})
	}
	return okEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, Body: result})
}

// Summary/model aggregation scans the key's time range once. Window totals
// cover all model groups, including those outside the displayed top 50.
type monitorAggregate struct {
	AtMS    int64          `json:"at_ms"`
	Summary MonitorTotals  `json:"summary"`
	Models  []MonitorModel `json:"models"`
}

func monitorSQL(columns map[string]bool, keyHash string, o monitorOptions, now time.Time) (model, input, cached, latency, ttft, where string, args []any) {
	input, cached = usageTokenExpressions(columns)
	optional := func(name string) string {
		if columns[name] {
			return name
		}
		return "NULL"
	}
	model = "coalesce(substr(" + optional("requested_model") + ",1,129),'')"
	latency = "NULL"
	if columns["latency_ms"] {
		latency = "case when latency_ms between 0 and 86400000 then latency_ms else null end"
	}
	ttft = "NULL"
	if columns["ttft_ms"] {
		ttft = "case when ttft_ms between 0 and 86400000 then ttft_ms else null end"
	}
	where = " from usage_events where api_key_hash = ? and timestamp_ms >= ? and timestamp_ms <= ?"
	args = []any{keyHash, now.Add(-o.window).UnixMilli(), now.UnixMilli()}
	if o.status == "success" {
		where += " and coalesce(failed,0)=0"
	} else if o.status == "failed" {
		where += " and coalesce(failed,0)!=0"
	}
	return
}

func monitorTotalDest(t *MonitorTotals) []any {
	return []any{&t.Calls, &t.Successes, &t.Failures, &t.InputTokens, &t.OutputTokens, &t.CachedTokens, &t.TotalTokens, &t.AvgLatencyMS}
}

func readMonitorAggregate(ctx context.Context, d *monitorDatabase, keyHash string, o monitorOptions, now time.Time) (monitorAggregate, error) {
	a := monitorAggregate{AtMS: now.UnixMilli(), Models: []MonitorModel{}}
	model, input, cached, latency, _, where, args := monitorSQL(d.columns, keyHash, o, now)
	query := `with grouped as (
		select ` + model + ` as requested, count(*) as calls,
		coalesce(sum(case when coalesce(failed,0)=0 then 1 else 0 end),0) as successes,
		coalesce(sum(case when coalesce(failed,0)!=0 then 1 else 0 end),0) as failures,
		coalesce(sum(` + input + `),0) as inputs, coalesce(sum(output_tokens),0) as outputs,
		coalesce(sum(` + cached + `),0) as cached, coalesce(sum(total_tokens),0) as total,
		coalesce(sum(` + latency + `),0) as elapsed, count(` + latency + `) as timed
		` + where + ` group by ` + model + `
	) select requested,calls,successes,failures,inputs,outputs,cached,total,
		1.0*elapsed/nullif(timed,0),
		sum(calls) over(),sum(successes) over(),sum(failures) over(),sum(inputs) over(),
		sum(outputs) over(),sum(cached) over(),sum(total) over(),
		1.0*sum(elapsed) over()/nullif(sum(timed) over(),0)
	from grouped order by calls desc,requested limit 50`
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return a, err
	}
	defer rows.Close()
	for rows.Next() {
		var m MonitorModel
		dest := append([]any{&m.Model}, monitorTotalDest(&m.MonitorTotals)...)
		dest = append(dest, monitorTotalDest(&a.Summary)...)
		if err = rows.Scan(dest...); err != nil {
			return a, err
		}
		m.Model = safeRequestedModel(m.Model)
		a.Models = append(a.Models, m)
	}
	return a, rows.Err()
}

func readMonitorEvents(ctx context.Context, d *monitorDatabase, keyHash string, o monitorOptions, now time.Time, out *MonitorResponse) error {
	model, input, cached, latency, ttft, where, args := monitorSQL(d.columns, keyHash, o, now)
	optional := func(name string) string {
		if d.columns[name] {
			return name
		}
		return "NULL"
	}
	query := "select timestamp_ms, " + model + ", coalesce(substr(" + optional("reasoning_effort") + ",1,32),''), coalesce(substr(" + optional("service_tier") + ",1,32),''), coalesce(failed,0)!=0, " + optional("fail_status_code") + ", " + input + ", coalesce(output_tokens,0), " + cached + ", coalesce(total_tokens,0), " + latency + ", " + ttft + where + " order by timestamp_ms desc,id desc limit ?"
	rows, err := d.db.QueryContext(ctx, query, append(args, o.limit+1)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var e MonitorEvent
		if err = rows.Scan(&e.TimestampMS, &e.Model, &e.Reasoning, &e.ServiceTier, &e.Failed, &e.StatusCode, &e.InputTokens, &e.OutputTokens, &e.CachedTokens, &e.TotalTokens, &e.LatencyMS, &e.TTFTMS); err != nil {
			return err
		}
		e.Model = safeRequestedModel(e.Model)
		e.Reasoning = monitorEnum(e.Reasoning, "none", "minimal", "low", "medium", "high", "xhigh", "max", "auto")
		e.ServiceTier = monitorEnum(e.ServiceTier, "auto", "default", "flex", "priority", "scale", "standard")
		if e.StatusCode != nil && (*e.StatusCode < 100 || *e.StatusCode > 599) {
			e.StatusCode = nil
		}
		out.Events = append(out.Events, e)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if len(out.Events) > o.limit {
		out.Events = out.Events[:o.limit]
		out.Truncated = true
	}
	return nil
}

// Uncached entry point retained for schema/accounting tests and load comparisons.
func readMonitor(dbPath, keyHash string, o monitorOptions, now time.Time) (MonitorResponse, error) {
	s := newMonitorService(dbPath)
	defer s.close()
	ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
	defer cancel()
	d, err := s.acquire(ctx)
	if err != nil {
		return MonitorResponse{}, err
	}
	defer s.release(d)
	a, err := readMonitorAggregate(ctx, d, keyHash, o, now)
	if err != nil {
		return MonitorResponse{}, err
	}
	out := MonitorResponse{GeneratedAtMS: now.UnixMilli(), SummaryAtMS: now.UnixMilli(), Range: o.rangeName, Status: o.status, Summary: a.Summary, Models: a.Models, Events: []MonitorEvent{}}
	err = readMonitorEvents(ctx, d, keyHash, o, now, &out)
	return out, err
}

var publicModelPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]{0,127}$`)

func safeRequestedModel(value string) string {
	if !publicModelPattern.MatchString(value) || strings.Contains(strings.ToLower(value), "sk-") || strings.Contains(value, "..") {
		return "未记录模型"
	}
	return value
}

func monitorEnum(value string, allowed ...string) string {
	for _, a := range allowed {
		if value == a {
			return a
		}
	}
	return ""
}
