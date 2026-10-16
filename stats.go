package main

import (
	"database/sql"
	"os"

	_ "modernc.org/sqlite"
)

// readTokenStats queries the local CPA manager SQLite database (usage.sqlite)
// to fetch exact calls, input_tokens, output_tokens, cached_tokens, and total_tokens.
func readTokenStats(dbPath, keyHash string) TokenStats {
	var stats TokenStats
	if dbPath == "" {
		// Default search paths
		candidates := []string{
			"/opt/cpa/manager-data/usage.sqlite",
			"manager-data/usage.sqlite",
			"../manager-data/usage.sqlite",
		}
		for _, p := range candidates {
			if _, err := os.Stat(p); err == nil {
				dbPath = p
				break
			}
		}
	}

	if dbPath == "" {
		return stats
	}

	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return stats
	}
	defer db.Close()

	query := `
		SELECT 
			count(*),
			coalesce(sum(input_tokens), 0),
			coalesce(sum(output_tokens), 0),
			coalesce(sum(cached_tokens), 0),
			coalesce(sum(total_tokens), 0)
		FROM usage_events 
		WHERE api_key_hash = ?
	`
	row := db.QueryRow(query, keyHash)
	_ = row.Scan(&stats.Calls, &stats.InputTokens, &stats.OutputTokens, &stats.CachedTokens, &stats.TotalTokens)

	return stats
}
