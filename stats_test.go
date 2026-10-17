package main

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsageDatabaseMustExistAndBeAFile(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.sqlite")
	for _, path := range []string{dir, missing} {
		if got := resolveUsageDB(path); got != "" {
			t.Fatalf("resolveUsageDB(%q) = %q", path, got)
		}
	}
	readTokenStats(missing, "hash")
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("reading a missing database created it: %v", err)
	}
}

func TestUsageReadDistinguishesZeroFromUnavailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.sqlite")
	if _, err := readTokenStats(path, "hash"); !errors.Is(err, errUsageDBUnavailable) {
		t.Fatalf("missing database: %v", err)
	}
	if err := os.WriteFile(path, []byte("invalid sqlite data"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readTokenStats(path, "hash"); err == nil {
		t.Fatal("an unreadable database was treated as zero usage")
	}
	dbPath := filepath.Join(t.TempDir(), "valid.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`create table usage_events (api_key_hash text, input_tokens integer, output_tokens integer, cached_tokens integer, total_tokens integer);`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	got, err := readTokenStats(dbPath, "hash")
	if err != nil || got != (TokenStats{}) {
		t.Fatalf("valid empty database: %+v, %v", got, err)
	}
}

func TestUsageReadRejectsUnsupportedSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`create table usage_events (api_key_hash text);`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readTokenStats(path, "hash"); err == nil || !strings.Contains(err.Error(), "missing required column") {
		t.Fatalf("unsupported schema was not reported: %v", err)
	}
}

func TestUsageReadUsesManagerPlusNormalizedAccounting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`create table usage_events (
		api_key_hash text, input_tokens integer, output_tokens integer,
		cached_tokens integer, total_tokens integer, cache_tokens integer,
		cache_read_tokens integer, cache_creation_tokens integer,
		normalized_total_input_tokens integer, normalized_cache_read_tokens integer);
		insert into usage_events values ('hash', 10, 2, 70, 82, 70, 50, 20, 80, 50);
		insert into usage_events values ('hash', 20, 0, 30, 50, 30, 0, 30, null, null);
		insert into usage_events values ('other', 999, 999, 999, 2997, 999, 999, 0, 999, 999);`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	got, err := readTokenStats(path, "hash")
	want := TokenStats{Calls: 2, InputTokens: 100, OutputTokens: 2, CachedTokens: 50, TotalTokens: 132}
	if err != nil || got != want {
		t.Fatalf("normalized usage %+v, want %+v: %v", got, want, err)
	}
}

func TestUsageReadIncludesUncheckpointedWALEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`pragma journal_mode=WAL; pragma wal_autocheckpoint=0;
		create table usage_events (api_key_hash text, input_tokens integer, output_tokens integer, cached_tokens integer, total_tokens integer);
		insert into usage_events values ('hash', 100, 27, 50, 127);`)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("fixture has no WAL data: %v", err)
	}
	got, err := readTokenStats(path, "hash")
	if err != nil || got.Calls != 1 || got.TotalTokens != 127 {
		t.Fatalf("uncheckpointed events are missing: %+v, %v", got, err)
	}
}

func TestUsageDatabasePathIsNotAURI(t *testing.T) {
	// # and spaces are valid file name characters, not URI syntax supplied by
	// an administrator. SQLite must open the exact file in read-only mode.
	dbPath := filepath.Join(t.TempDir(), "usage #1.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`create table usage_events (api_key_hash text, input_tokens integer, output_tokens integer, cached_tokens integer, total_tokens integer);
		insert into usage_events values ('test-hash', 10, 2, 3, 15);`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := readTokenStats(dbPath, "test-hash"); err != nil || got.Calls != 1 || got.TotalTokens != 15 {
		t.Fatalf("database path was not preserved: %+v, %v", got, err)
	}
}
