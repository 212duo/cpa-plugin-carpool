package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Seat limits are enforced in request.intercept_before rather than in the
// scheduler: it runs once per request, before any auth is picked, and its request
// ID comes back in request.complete when the request ends, which is what frees a
// concurrency slot.

const (
	rpmWindow = time.Minute
	// inflightTTL bounds how long a request holds a concurrency slot when CPA never
	// reports its completion, so a lost event cannot lock a seat out for good.
	inflightTTL = 30 * time.Minute
	// concurrencyRetryAfter is the Retry-After sent when a seat is at its
	// concurrency limit; there is no way to know when a slot frees up.
	concurrencyRetryAfter = 2 * time.Second
)

// legacyHost is set when the host does not send request.complete (schema version
// below 2). Concurrency is then not limited, because slots would never be freed.
var legacyHost atomic.Bool

type seatLimiter struct {
	mu       sync.Mutex
	seats    map[string]*seatCounters   // by seat name, which survives reconfiguration
	inflight map[string]inflightRequest // by request ID
}

type seatCounters struct {
	active int
	starts []time.Time // accepted requests within rpmWindow, oldest first
}

type inflightRequest struct {
	seat string
	at   time.Time
}

var limiter = newSeatLimiter()

func newSeatLimiter() *seatLimiter {
	return &seatLimiter{
		seats:    make(map[string]*seatCounters),
		inflight: make(map[string]inflightRequest),
	}
}

// limitDecision is the outcome of seatLimiter.acquire.
type limitDecision struct {
	allowed    bool
	code       string // seat_concurrency_limit or seat_rpm_limit when rejected
	limit      int
	retryAfter time.Duration
}

func (l *seatLimiter) counters(name string) *seatCounters {
	c := l.seats[name]
	if c == nil {
		c = &seatCounters{}
		l.seats[name] = c
	}
	return c
}

func (c *seatCounters) prune(now time.Time) {
	i := 0
	for i < len(c.starts) && now.Sub(c.starts[i]) >= rpmWindow {
		i++
	}
	if i > 0 {
		c.starts = append(c.starts[:0], c.starts[i:]...)
	}
}

// expireStale frees the slots of the seat's requests older than inflightTTL.
func (l *seatLimiter) expireStale(name string, c *seatCounters, now time.Time) {
	for id, r := range l.inflight {
		if r.seat == name && now.Sub(r.at) > inflightTTL {
			delete(l.inflight, id)
			if c.active > 0 {
				c.active--
			}
		}
	}
}

// acquire admits a request for the seat or explains why not. track is false when
// the request cannot be released later (no request ID, or no request.complete),
// in which case it counts against the RPM limit only.
func (l *seatLimiter) acquire(requestID string, s *seat, track bool, now time.Time) limitDecision {
	l.mu.Lock()
	defer l.mu.Unlock()

	if track {
		if _, seen := l.inflight[requestID]; seen {
			return limitDecision{allowed: true} // the same request intercepted again
		}
	}
	name := s.user.Name
	c := l.counters(name)
	c.prune(now)

	if track && s.maxConcurrency > 0 && c.active >= s.maxConcurrency {
		l.expireStale(name, c, now)
		if c.active >= s.maxConcurrency {
			return limitDecision{code: "seat_concurrency_limit", limit: s.maxConcurrency, retryAfter: concurrencyRetryAfter}
		}
	}
	if s.rpm > 0 && len(c.starts) >= s.rpm {
		// The request that has to leave the window before another one fits.
		oldest := c.starts[len(c.starts)-s.rpm]
		return limitDecision{code: "seat_rpm_limit", limit: s.rpm, retryAfter: oldest.Add(rpmWindow).Sub(now)}
	}

	c.starts = append(c.starts, now)
	if track {
		c.active++
		l.inflight[requestID] = inflightRequest{seat: name, at: now}
	}
	return limitDecision{allowed: true}
}

// release frees the concurrency slot held by a request, if any.
func (l *seatLimiter) release(requestID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.inflight[requestID]
	if !ok {
		return
	}
	delete(l.inflight, requestID)
	if c := l.seats[r.seat]; c != nil && c.active > 0 {
		c.active--
	}
}

// usage returns the seat's requests in flight and started in the last minute.
func (l *seatLimiter) usage(name string, now time.Time) (active, recent int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.seats[name]
	if c == nil {
		return 0, 0
	}
	c.prune(now)
	return c.active, len(c.starts)
}

func (l *seatLimiter) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seats = make(map[string]*seatCounters)
	l.inflight = make(map[string]inflightRequest)
}

// seatLimits reports the seat's limits together with their current use.
func seatLimits(s *seat, now time.Time) SeatLimits {
	active, recent := limiter.usage(s.user.Name, now)
	return SeatLimits{
		MaxConcurrency: s.maxConcurrency,
		RPM:            s.rpm,
		Active:         active,
		RecentRequests: recent,
	}
}

// interceptRequest is the subset of pluginapi.RequestInterceptRequest the limiter
// reads; the request body is left undecoded.
type interceptRequest struct {
	RequestID string
	Headers   map[string][]string
	Metadata  map[string]any
}

// interceptBeforeAuth applies the caller's seat limits. Requests from keys that
// carpool does not manage pass through untouched; so does everything while the
// configuration is invalid, since the scheduler rejects those requests anyway.
func interceptBeforeAuth(raw []byte, table *routingTable) ([]byte, error) {
	var req interceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if table == nil || table.configErr != nil {
		return passThrough()
	}
	s := table.userFor(pluginapi.SchedulerOptions{Headers: req.Headers, Metadata: req.Metadata})
	if s == nil {
		return passThrough()
	}
	if s.user.Paused {
		body, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": "permission_error", "code": "seat_paused", "message": "Seat paused, please contact the host"}})
		return okEnvelope(pluginapi.RequestInterceptResponse{Terminate: true, StatusCode: http.StatusForbidden, ResponseHeaders: http.Header{"Content-Type": {"application/json"}}, ResponseBody: body})
	}

	track := req.RequestID != "" && !legacyHost.Load()
	d := limiter.acquire(req.RequestID, s, track, time.Now())
	if d.allowed {
		return passThrough()
	}
	if table.debug {
		logf("debug", "seat %s: request rejected, %s (%d)", s.user.Name, d.code, d.limit)
	}
	return limitRejection(s, d)
}

// completeRequest handles request.complete, which CPA sends once for every request
// that reached request interception, however it ended.
func completeRequest(raw []byte) ([]byte, error) {
	var completion struct {
		RequestID string
	}
	if err := json.Unmarshal(raw, &completion); err != nil {
		return nil, err
	}
	if completion.RequestID != "" {
		limiter.release(completion.RequestID)
	}
	return okEnvelope(struct{}{})
}

func passThrough() ([]byte, error) {
	return okEnvelope(pluginapi.RequestInterceptResponse{})
}

// limitRejection answers the client with a 429 in a shape both OpenAI and
// Anthropic clients understand, so they back off and retry.
func limitRejection(s *seat, d limitDecision) ([]byte, error) {
	secs := int(math.Ceil(d.retryAfter.Seconds()))
	if secs < 1 {
		secs = 1
	}
	var msg string
	if d.code == "seat_concurrency_limit" {
		msg = fmt.Sprintf("carpool: seat %s allows at most %d concurrent requests, retry when one finishes", s.user.Name, d.limit)
	} else {
		msg = fmt.Sprintf("carpool: seat %s allows at most %d requests per minute, retry in %d s", s.user.Name, d.limit, secs)
	}
	body, err := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    "rate_limit_error",
			"code":    d.code,
			"message": msg,
		},
	})
	if err != nil {
		return nil, err
	}
	return okEnvelope(pluginapi.RequestInterceptResponse{
		Terminate:  true,
		StatusCode: http.StatusTooManyRequests,
		ResponseHeaders: http.Header{
			"Content-Type": []string{"application/json"},
			"Retry-After":  []string{strconv.Itoa(secs)},
		},
		ResponseBody: body,
	})
}
