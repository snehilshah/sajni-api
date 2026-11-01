package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// POST /api/telemetry takes batches of web-client diagnostics (fetch timings,
// stuck requests and queries, page lifecycle, main-thread stalls) and writes
// each event to the log as `client <kind>`, for the 2026-11 "page stuck
// loading" investigation. It is open so a client whose session broke can
// still report; signed-out batches are capped tightly.
const (
	telemetryMaxBody   = 512 << 10
	telemetryMaxEvents = 500
	anonBatchesPerMin  = 30
)

type telemetryBatch struct {
	Session json.RawMessage   `json:"session"`
	SID     string            `json:"sid"`
	Build   string            `json:"build"`
	Events  []json.RawMessage `json:"events"`
}

type telemetryHead struct {
	Kind string  `json:"k"`
	Seq  int64   `json:"q"`
	At   float64 `json:"at"`
}

var anonTelemetry = struct {
	sync.Mutex
	window time.Time
	count  int
}{}

func allowAnonTelemetry() bool {
	anonTelemetry.Lock()
	defer anonTelemetry.Unlock()
	now := time.Now()
	if now.Sub(anonTelemetry.window) > time.Minute {
		anonTelemetry.window, anonTelemetry.count = now, 0
	}
	anonTelemetry.count++
	return anonTelemetry.count <= anonBatchesPerMin
}

func handleTelemetry(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := deps.Auth.BearerUserID(r)
		if uid == "" && !allowAnonTelemetry() {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, telemetryMaxBody)
		var b telemetryBatch
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			errJSON(w, http.StatusBadRequest, "bad batch")
			return
		}
		if len(b.SID) > 64 || len(b.Build) > 64 {
			errJSON(w, http.StatusBadRequest, "bad batch")
			return
		}
		if len(b.Events) > telemetryMaxEvents {
			b.Events = b.Events[:telemetryMaxEvents]
		}
		log.Info().
			Str("src", "client").
			Str("sid", b.SID).
			Str("build", b.Build).
			Bool("authed", uid != "").
			Int("events", len(b.Events)).
			RawJSON("session", rawOrNull(b.Session)).
			Msg("client batch")
		for _, raw := range b.Events {
			var h telemetryHead
			if json.Unmarshal(raw, &h) != nil || h.Kind == "" || len(h.Kind) > 40 {
				continue
			}
			evt := log.Info()
			if strings.HasSuffix(h.Kind, "_stuck") || h.Kind == "user_report" ||
				h.Kind == "stall" || h.Kind == "error" || h.Kind == "chunk_slow" {
				evt = log.Warn()
			}
			evt.Str("src", "client").
				Str("sid", b.SID).
				Str("build", b.Build).
				Str("kind", h.Kind).
				Int64("seq", h.Seq).
				Str("client_at", time.UnixMilli(int64(h.At)).UTC().Format(time.RFC3339Nano)).
				RawJSON("ev", raw).
				Msg("client " + h.Kind)
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func rawOrNull(raw json.RawMessage) []byte {
	if len(raw) == 0 || !json.Valid(raw) {
		return []byte("null")
	}
	return raw
}
