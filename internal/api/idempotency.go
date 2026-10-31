package api

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"

	"sajni/internal/db"

	"github.com/rs/zerolog/log"
)

// IdempotencyHeader marks a change the client may send more than once: the
// Android offline outbox replays queued changes, and a reply lost on a bad
// connection means the same change arrives again. The first copy runs; later
// copies get its stored reply instead of running twice (two expenses).
const IdempotencyHeader = "Idempotency-Key"

// withIdempotency runs a keyed change once. A key still running answers 409
// with X-Idempotency-Pending, so the client retries later. 5xx replies aren't
// kept: the change didn't happen, so a retry should run it.
func withIdempotency(d *db.DB, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get(IdempotencyHeader)
		if key == "" || r.Method == http.MethodGet || len(key) > 100 {
			next.ServeHTTP(w, r)
			return
		}
		uid := userID(r.Context())
		ctx := r.Context()

		// Claim the key. A claim left at status 0 for 2 minutes belongs to a
		// request that died mid-way; take it over.
		var claimed bool
		err := d.QueryRowContext(ctx, `
			INSERT INTO idempotency_keys (user_id, key) VALUES ($1, $2)
			ON CONFLICT (user_id, key) DO UPDATE SET created_at = NOW()
				WHERE idempotency_keys.status = 0 AND idempotency_keys.created_at < NOW() - INTERVAL '2 minutes'
			RETURNING TRUE`, uid, key).Scan(&claimed)
		if errors.Is(err, sql.ErrNoRows) {
			var status int
			var body []byte
			if err := d.QueryRowContext(ctx, `SELECT status, body FROM idempotency_keys WHERE user_id = $1 AND key = $2`, uid, key).Scan(&status, &body); err != nil {
				internalError(w, r, "read idempotency key", err)
				return
			}
			if status == 0 {
				w.Header().Set("X-Idempotency-Pending", "1")
				errJSON(w, http.StatusConflict, "This change is still being saved.")
				return
			}
			w.Header().Set("X-Idempotency-Replayed", "1")
			if len(body) > 0 {
				w.Header().Set("Content-Type", "application/json")
			}
			w.WriteHeader(status)
			w.Write(body)
			return
		}
		if err != nil {
			internalError(w, r, "claim idempotency key", err)
			return
		}

		rec := &recordingWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		// The reply is already sent; keeping it is best effort. Use a fresh
		// context, since the client may have gone.
		bg := context.WithoutCancel(ctx)
		if rec.status >= 500 {
			d.ExecContext(bg, `DELETE FROM idempotency_keys WHERE user_id = $1 AND key = $2`, uid, key)
			return
		}
		if _, err := d.ExecContext(bg, `UPDATE idempotency_keys SET status = $3, body = $4 WHERE user_id = $1 AND key = $2`,
			uid, key, rec.status, rec.body.Bytes()); err != nil {
			log.Error().Err(err).Msg("store idempotency reply")
		}
		d.ExecContext(bg, `DELETE FROM idempotency_keys WHERE user_id = $1 AND created_at < NOW() - INTERVAL '30 days'`, uid)
	})
}

// recordingWriter passes a reply through while keeping a copy of it.
type recordingWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	body        bytes.Buffer
}

func (rw *recordingWriter) WriteHeader(code int) {
	if !rw.wroteHeader {
		rw.status = code
		rw.wroteHeader = true
	}
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *recordingWriter) Write(b []byte) (int, error) {
	rw.wroteHeader = true
	rw.body.Write(b)
	return rw.ResponseWriter.Write(b)
}
