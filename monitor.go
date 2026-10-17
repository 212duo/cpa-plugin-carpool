package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	result, err := readMonitor(cfg.DBPath, usageKeyHash(key), o, time.Now())
	if err != nil {
		// Do not log SQLite errors: an invalid cell may put private values in errors.
		logf("warn", "request monitoring data is unavailable")
		return jsonResponse(http.StatusServiceUnavailable, map[string]string{"error": "请求记录暂不可用，请联系车主检查用量采集与数据库配置"})
	}
	return jsonResponse(http.StatusOK, result)
}

func readMonitor(dbPath, keyHash string, o monitorOptions, now time.Time) (MonitorResponse, error) {
	out := MonitorResponse{GeneratedAtMS: now.UnixMilli(), Range: o.rangeName, Status: o.status, Models: []MonitorModel{}, Events: []MonitorEvent{}}
	db, err := openUsageDB(dbPath)
	if err != nil {
		return out, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	columns, err := usageColumns(ctx, db)
	if err != nil {
		return out, err
	}
	for _, name := range []string{"id", "timestamp_ms", "api_key_hash", "failed", "input_tokens", "output_tokens", "cached_tokens", "total_tokens"} {
		if !columns[name] {
			return out, fmt.Errorf("unsupported usage schema")
		}
	}
	input, cached := usageTokenExpressions(columns)
	optional := func(name, fallback string) string {
		if columns[name] {
			return name
		}
		return fallback
	}
	// Only the client-requested model may be displayed. Resolved models and
	// provider/auth labels can contain internal routing details.
	model := "coalesce(substr(" + optional("requested_model", "NULL") + ", 1, 129), '')"
	latency := "case when " + optional("latency_ms", "NULL") + " between 0 and 86400000 then latency_ms else null end"
	if !columns["latency_ms"] {
		latency = "NULL"
	}
	ttft := "case when " + optional("ttft_ms", "NULL") + " between 0 and 86400000 then ttft_ms else null end"
	if !columns["ttft_ms"] {
		ttft = "NULL"
	}
	where := " from usage_events where api_key_hash = ? and timestamp_ms >= ? and timestamp_ms <= ?"
	args := []any{keyHash, now.Add(-o.window).UnixMilli(), now.UnixMilli()}
	if o.status == "success" {
		where += " and coalesce(failed,0) = 0"
	} else if o.status == "failed" {
		where += " and coalesce(failed,0) != 0"
	}
	aggregate := "count(*), coalesce(sum(case when coalesce(failed,0)=0 then 1 else 0 end),0), coalesce(sum(case when coalesce(failed,0)!=0 then 1 else 0 end),0), coalesce(sum(" + input + "),0), coalesce(sum(output_tokens),0), coalesce(sum(" + cached + "),0), coalesce(sum(total_tokens),0), avg(" + latency + ")"
	// One SQLite snapshot keeps summaries and rows consistent while the collector writes.
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	totalDest := func(t *MonitorTotals) []any {
		return []any{&t.Calls, &t.Successes, &t.Failures, &t.InputTokens, &t.OutputTokens, &t.CachedTokens, &t.TotalTokens, &t.AvgLatencyMS}
	}
	if err = tx.QueryRowContext(ctx, "select "+aggregate+where, args...).Scan(totalDest(&out.Summary)...); err != nil {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, "select "+model+", "+aggregate+where+" group by "+model+" order by count(*) desc, 1 limit 50", args...)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var m MonitorModel
		if err = rows.Scan(append([]any{&m.Model}, totalDest(&m.MonitorTotals)...)...); err != nil {
			rows.Close()
			return out, err
		}
		m.Model = safeRequestedModel(m.Model)
		out.Models = append(out.Models, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	query := "select timestamp_ms, " + model + ", coalesce(substr(" + optional("reasoning_effort", "NULL") + ",1,32),''), coalesce(substr(" + optional("service_tier", "NULL") + ",1,32),''), coalesce(failed,0)!=0, " + optional("fail_status_code", "NULL") + ", " + input + ", coalesce(output_tokens,0), " + cached + ", coalesce(total_tokens,0), " + latency + ", " + ttft + where + " order by timestamp_ms desc, id desc limit ?"
	rows, err = tx.QueryContext(ctx, query, append(args, o.limit+1)...)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var e MonitorEvent
		if err = rows.Scan(&e.TimestampMS, &e.Model, &e.Reasoning, &e.ServiceTier, &e.Failed, &e.StatusCode, &e.InputTokens, &e.OutputTokens, &e.CachedTokens, &e.TotalTokens, &e.LatencyMS, &e.TTFTMS); err != nil {
			rows.Close()
			return out, err
		}
		e.Model = safeRequestedModel(e.Model)
		e.Reasoning = monitorEnum(e.Reasoning, "none", "minimal", "low", "medium", "high", "xhigh", "max", "auto")
		e.ServiceTier = monitorEnum(e.ServiceTier, "auto", "default", "flex", "priority", "scale", "standard")
		if e.StatusCode != nil && (*e.StatusCode < 100 || *e.StatusCode > 599) {
			e.StatusCode = nil
		}
		out.Events = append(out.Events, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if len(out.Events) > o.limit {
		out.Events = out.Events[:o.limit]
		out.Truncated = true
	}
	return out, tx.Commit()
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
