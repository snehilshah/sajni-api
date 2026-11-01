package api

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"sajni/internal/db"

	"github.com/rs/zerolog/log"
)

// Request diagnostics (2026-11 "page stuck loading" investigation). Every
// request is logged at info with the client's request id (X-Sajni-Req, set by
// the web client's fetch wrapper), the Cloud Run trace, this instance's
// counters and the DB pool state, so a stuck request on the phone can be
// matched to what the server saw, or proven never to have arrived. The
// response carries Server-Timing so the client can split server time from
// network time.

var (
	instanceID    = newInstanceID()
	instanceStart = time.Now()
	instanceReqs  atomic.Int64
	inflight      atomic.Int64
)

func newInstanceID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// statusWriter records the status a handler wrote.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

// Flush delegates to the underlying ResponseWriter so SSE and chunked
// transfers work through the middleware.
func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (sw *statusWriter) Unwrap() http.ResponseWriter { return sw.ResponseWriter }

// obsWriter stamps Server-Timing when headers go out and counts bytes.
type obsWriter struct {
	http.ResponseWriter
	start       time.Time
	seq         int64
	status      int
	wroteHeader bool
	headerAt    time.Duration
	bytes       int64
}

func (ow *obsWriter) WriteHeader(code int) {
	if ow.wroteHeader {
		return
	}
	ow.wroteHeader = true
	ow.status = code
	ow.headerAt = time.Since(ow.start)
	ow.Header().Set("Server-Timing", fmt.Sprintf(
		`app;dur=%.1f, inst;desc="%s", seq;desc="%d", up;desc="%d"`,
		float64(ow.headerAt.Microseconds())/1000, instanceID, ow.seq,
		int64(time.Since(instanceStart).Seconds())))
	ow.ResponseWriter.WriteHeader(code)
}

func (ow *obsWriter) Write(b []byte) (int, error) {
	if !ow.wroteHeader {
		ow.WriteHeader(http.StatusOK)
	}
	n, err := ow.ResponseWriter.Write(b)
	ow.bytes += int64(n)
	return n, err
}

func (ow *obsWriter) Flush() {
	if !ow.wroteHeader {
		ow.WriteHeader(http.StatusOK)
	}
	if f, ok := ow.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (ow *obsWriter) Unwrap() http.ResponseWriter { return ow.ResponseWriter }

// withLogging logs every request (5xx at error, >2s at warn, the rest at
// info). Health probes and the telemetry sink itself are skipped.
func withLogging(h http.Handler, database *db.DB, project string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			h.ServeHTTP(w, r)
			return
		}
		seq := instanceReqs.Add(1)
		inf := inflight.Add(1)
		defer inflight.Add(-1)
		var before dbStats
		if database != nil {
			before = poolStats(database)
		}
		ow := &obsWriter{ResponseWriter: w, start: time.Now(), seq: seq, status: http.StatusOK}
		h.ServeHTTP(ow, r)
		dur := time.Since(ow.start)
		if r.URL.Path == "/api/telemetry" {
			return
		}

		evt := log.Info()
		switch {
		case ow.status >= 500:
			evt = log.Error()
		case dur > 2*time.Second:
			evt = log.Warn()
		}
		evt = evt.Str("method", r.Method).
			Str("path", r.URL.Path).
			Str("query", r.URL.RawQuery).
			Int("status", ow.status).
			Int64("ms", dur.Milliseconds()).
			Int64("header_ms", ow.headerAt.Milliseconds()).
			Int64("bytes", ow.bytes).
			Str("req", r.Header.Get("X-Sajni-Req")).
			Str("vercel_id", r.Header.Get("X-Vercel-Id")).
			Str("proto", r.Proto).
			Str("inst", instanceID).
			Int64("inst_seq", seq).
			Int64("inst_up_s", int64(time.Since(instanceStart).Seconds())).
			Int64("inflight", inf).
			Bool("ctx_done", r.Context().Err() != nil)
		if trace := r.Header.Get("X-Cloud-Trace-Context"); trace != "" && project != "" {
			if id, _, ok := strings.Cut(trace, "/"); ok {
				evt = evt.Str("logging.googleapis.com/trace", "projects/"+project+"/traces/"+id)
			}
		}
		if database != nil {
			after := poolStats(database)
			evt = evt.Int("db_open", after.open).
				Int("db_in_use", after.inUse).
				Int("db_idle", after.idle).
				Int64("db_waits", after.waitCount-before.waitCount).
				Int64("db_wait_ms", (after.waitDur-before.waitDur).Milliseconds()).
				Int64("db_closed_idle", after.closedIdle-before.closedIdle).
				Int64("db_closed_life", after.closedLife-before.closedLife)
		}
		evt.Msg("request")
	})
}

type dbStats struct {
	open, inUse, idle      int
	waitCount              int64
	waitDur                time.Duration
	closedIdle, closedLife int64
}

func poolStats(database *db.DB) dbStats {
	s := database.Stats()
	return dbStats{
		open: s.OpenConnections, inUse: s.InUse, idle: s.Idle,
		waitCount: s.WaitCount, waitDur: s.WaitDuration,
		closedIdle: s.MaxIdleClosed + s.MaxIdleTimeClosed, closedLife: s.MaxLifetimeClosed,
	}
}
