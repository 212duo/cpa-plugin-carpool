package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const monitorRecordsTTL = 10 * time.Second
const monitorSummaryTTL = 30 * time.Second

// Across configurations, at most two monitoring queries execute simultaneously.
// Cache hits do not take a slot; waiting and SQL share a three-second deadline.
var monitorQuerySlots = make(chan struct{}, 2)

type monitorDatabase struct {
	db      *sql.DB
	info    os.FileInfo
	path    string
	columns map[string]bool
	refs    int // guarded by service.mu
	retired bool
}

// Each routing snapshot owns its service. Reconfiguration retires the entire
// service, cancels in-flight queries and drops both caches, including on disable.
type monitorService struct {
	mu                   sync.Mutex
	path                 string
	database             *monitorDatabase
	ctx                  context.Context
	cancel               context.CancelFunc
	closed               bool
	responses, summaries *monitorByteCache
	queries              atomic.Uint64
}

func newMonitorService(path string) *monitorService {
	ctx, cancel := context.WithCancel(context.Background())
	s := &monitorService{path: path, ctx: ctx, cancel: cancel, responses: newMonitorByteCache(monitorRecordsTTL, 256, 16<<20), summaries: newMonitorByteCache(monitorSummaryTTL, 512, 4<<20)}
	// Composed caches must not extend the lifetime of an older summary.
	s.responses.capExpiry = func(data []byte) time.Time {
		var stamp struct {
			At int64 `json:"summary_at_ms"`
		}
		if json.Unmarshal(data, &stamp) != nil {
			return time.Time{}
		}
		return time.UnixMilli(stamp.At).Add(monitorSummaryTTL)
	}
	s.summaries.capExpiry = func(data []byte) time.Time {
		var stamp struct {
			At int64 `json:"at_ms"`
		}
		if json.Unmarshal(data, &stamp) != nil {
			return time.Time{}
		}
		return time.UnixMilli(stamp.At).Add(monitorSummaryTTL)
	}
	return s
}

func (s *monitorService) close() {
	s.cancel()
	s.responses.close()
	s.summaries.close()
	s.mu.Lock()
	s.closed = true
	if d := s.database; d != nil {
		d.retired = true
		if d.refs == 0 {
			d.db.Close()
		}
	}
	s.mu.Unlock()
}

func (s *monitorService) acquire(ctx context.Context) (*monitorDatabase, error) {
	path := resolveUsageDB(s.path)
	if path == "" {
		return nil, errUsageDBUnavailable
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || ctx.Err() != nil {
		return nil, errMonitorBusy
	}
	if d := s.database; d != nil && d.path == path && os.SameFile(info, d.info) {
		d.refs++
		return d, nil
	}
	db, err := openUsageDB(path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	columns, err := usageColumns(ctx, db)
	if err != nil {
		db.Close()
		return nil, err
	}
	for _, name := range []string{"id", "timestamp_ms", "api_key_hash", "failed", "input_tokens", "output_tokens", "cached_tokens", "total_tokens"} {
		if !columns[name] {
			db.Close()
			return nil, errUsageDBUnavailable
		}
	}
	if old := s.database; old != nil {
		old.retired = true
		if old.refs == 0 {
			old.db.Close()
		}
	}
	d := &monitorDatabase{db: db, info: info, path: path, columns: columns, refs: 1}
	s.database = d
	return d, nil
}

func (s *monitorService) release(d *monitorDatabase) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d.refs--
	if d.retired && d.refs == 0 {
		d.db.Close()
	}
}

func (s *monitorService) query(ctx context.Context, run func() error) error {
	select {
	case monitorQuerySlots <- struct{}{}:
		defer func() { <-monitorQuerySlots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.queries.Add(1)
	return run()
}

func (s *monitorService) get(revision, keyHash string, o monitorOptions) ([]byte, error) {
	ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
	defer cancel()
	d, err := s.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer s.release(d)
	k := monitorCacheKey{database: d, revision: revision, keyHash: keyHash, rangeName: o.rangeName, status: o.status, limit: o.limit}
	data, err := s.responses.get(ctx, k, func() ([]byte, error) {
		summaryKey := k
		summaryKey.limit = 0
		summaryData, err := s.summaries.get(ctx, summaryKey, func() ([]byte, error) {
			var a monitorAggregate
			err := s.query(ctx, func() error { var err error; a, err = readMonitorAggregate(ctx, d, keyHash, o, time.Now()); return err })
			if err != nil {
				return nil, err
			}
			return json.Marshal(a)
		})
		if err != nil {
			return nil, err
		}
		var a monitorAggregate
		if err = json.Unmarshal(summaryData, &a); err != nil {
			return nil, err
		}
		out := MonitorResponse{GeneratedAtMS: time.Now().UnixMilli(), SummaryAtMS: a.AtMS, Range: o.rangeName, Status: o.status, Summary: a.Summary, Models: a.Models, Events: []MonitorEvent{}}
		err = s.query(ctx, func() error { return readMonitorEvents(ctx, d, keyHash, o, time.UnixMilli(out.GeneratedAtMS), &out) })
		if err != nil {
			return nil, err
		}
		return json.Marshal(out)
	})
	if err != nil {
		return nil, err
	}
	// A database replaced under the same pathname must not reuse the old data.
	info, err := os.Stat(d.path)
	if err != nil || !os.SameFile(info, d.info) {
		return nil, errUsageDBUnavailable
	}
	return data, nil
}
