package main

// Uncached three-query reference from f279e06, used only by opt-in load tests.
import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"
	"time"
)

var monitorBaselineQueries atomic.Uint64

func readMonitorBaseline(dbPath, keyHash string, o monitorOptions, now time.Time) (MonitorResponse, error) {
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
	monitorBaselineQueries.Add(1)
	if err = tx.QueryRowContext(ctx, "select "+aggregate+where, args...).Scan(totalDest(&out.Summary)...); err != nil {
		return out, err
	}
	monitorBaselineQueries.Add(1)
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
	monitorBaselineQueries.Add(1)
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
