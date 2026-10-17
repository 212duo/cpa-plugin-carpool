package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var errUsageDBUnavailable = errors.New("usage database is not available")

// usageDBCandidates are tried in order when db-path is not configured.
var usageDBCandidates = []string{
	"/opt/cpa/manager-data/usage.sqlite",
	"manager-data/usage.sqlite",
	"../manager-data/usage.sqlite",
}

// resolveUsageDB returns the CPA Manager Plus database to read, or "" when there
// is none.
func resolveUsageDB(dbPath string) string {
	if dbPath != "" {
		if info, err := os.Stat(dbPath); err == nil && info.Mode().IsRegular() {
			return dbPath
		}
		return ""
	}
	for _, p := range usageDBCandidates {
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

// readTokenStats queries the local CPA manager SQLite database (usage.sqlite)
// to fetch exact calls, input_tokens, output_tokens, cached_tokens, and total_tokens.
func readTokenStats(dbPath, keyHash string) (TokenStats, error) {
	stats, err := readTokenStatsByHash(dbPath, []string{keyHash})
	return stats[keyHash], err
}

// readTokenStatsByHash sums the usage of several API key hashes in one query.
// No matching rows is a valid zero; missing or unreadable data is an error.
func readTokenStatsByHash(dbPath string, keyHashes []string) (map[string]TokenStats, error) {
	out := make(map[string]TokenStats, len(keyHashes))
	db, err := openUsageDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	columns, err := usageColumns(ctx, db)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"api_key_hash", "input_tokens", "output_tokens", "cached_tokens", "total_tokens"} {
		if !columns[name] {
			return nil, fmt.Errorf("usage_events is missing required column %s", name)
		}
	}
	input, cached := usageTokenExpressions(columns)
	if len(keyHashes) == 0 {
		return out, nil
	}

	args := make([]any, len(keyHashes))
	for i, h := range keyHashes {
		args[i] = h
	}
	query := `
		SELECT
			api_key_hash,
			count(*),
			coalesce(sum(` + input + `), 0),
			coalesce(sum(output_tokens), 0),
			coalesce(sum(` + cached + `), 0),
			coalesce(sum(total_tokens), 0)
		FROM usage_events
		WHERE api_key_hash IN (` + strings.TrimSuffix(strings.Repeat("?,", len(keyHashes)), ",") + `)
		GROUP BY api_key_hash
	`
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query usage database: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var hash string
		var s TokenStats
		if err := rows.Scan(&hash, &s.Calls, &s.InputTokens, &s.OutputTokens, &s.CachedTokens, &s.TotalTokens); err != nil {
			return nil, fmt.Errorf("decode usage row: %w", err)
		}
		out[hash] = s
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read usage rows: %w", err)
	}
	return out, nil
}

// openUsageDB never creates or writes a database, including paths with URI characters.
func openUsageDB(dbPath string) (*sql.DB, error) {
	dbPath = resolveUsageDB(dbPath)
	if dbPath == "" {
		return nil, errUsageDBUnavailable
	}

	absPath, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, fmt.Errorf("resolve usage database: %w", err)
	}
	uriPath := filepath.ToSlash(absPath)
	if filepath.VolumeName(absPath) != "" && !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	dsn := (&url.URL{Scheme: "file", Path: uriPath, RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open usage database: %w", err)
	}
	return db, nil
}

// usageTokenExpressions shares Manager Plus normalization between history and monitoring.
func usageTokenExpressions(columns map[string]bool) (input, cached string) {
	input = "coalesce(input_tokens, 0)"
	if columns["normalized_total_input_tokens"] {
		input = "coalesce(normalized_total_input_tokens, input_tokens, 0)"
	}
	// New Manager Plus versions distinguish cache reads and cache creation;
	// cached_tokens can contain both and must not be labelled a cache hit.
	cached = "coalesce(cached_tokens, 0)"
	if columns["cache_read_tokens"] {
		legacy := cached
		if columns["cache_tokens"] {
			legacy = "max(" + legacy + ", coalesce(cache_tokens, 0))"
		}
		creation := "0"
		if columns["cache_creation_tokens"] {
			creation = "coalesce(cache_creation_tokens, 0)"
		}
		cached = "case when coalesce(cache_read_tokens, 0) > 0 or " + creation + " > 0 then coalesce(cache_read_tokens, 0) else " + legacy + " end"
	}
	if columns["normalized_cache_read_tokens"] {
		cached = "coalesce(normalized_cache_read_tokens, " + cached + ")"
	}
	return input, cached
}

func usageColumns(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, "pragma table_info(usage_events)")
	if err != nil {
		return nil, fmt.Errorf("read usage schema: %w", err)
	}
	defer rows.Close()
	columns := make(map[string]bool)
	for rows.Next() {
		var id, notNull, primaryKey int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&id, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			return nil, fmt.Errorf("decode usage schema: %w", err)
		}
		columns[name] = true
	}
	return columns, rows.Err()
}

func (s TokenStats) add(o TokenStats) TokenStats {
	return TokenStats{
		Calls:        s.Calls + o.Calls,
		InputTokens:  s.InputTokens + o.InputTokens,
		OutputTokens: s.OutputTokens + o.OutputTokens,
		CachedTokens: s.CachedTokens + o.CachedTokens,
		TotalTokens:  s.TotalTokens + o.TotalTokens,
	}
}
