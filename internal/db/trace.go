package db

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rs/zerolog/log"
)

// slowQuery is the threshold above which a query is logged. Diagnostics for
// the "page stuck loading" investigation (2026-11): every new connection,
// every slow query and every non-SQL (network/driver) failure is logged.
const slowQuery = 300 * time.Millisecond

type tracer struct{}

type traceKey struct{}

type traceStart struct {
	at  time.Time
	sql string
}

func (tracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, traceKey{}, traceStart{at: time.Now(), sql: d.SQL})
}

func (tracer) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, d pgx.TraceQueryEndData) {
	st, ok := ctx.Value(traceKey{}).(traceStart)
	if !ok {
		return
	}
	dur := time.Since(st.at)
	err := d.Err
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// Constraint violations and the like are the caller's business.
		err = nil
	}
	if err == nil && dur < slowQuery {
		return
	}
	evt := log.Warn()
	msg := "db slow query"
	switch {
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		evt, msg = log.Info(), "db query canceled"
	case err != nil:
		evt, msg = log.Error(), "db query failed"
	}
	var pid uint32
	if conn != nil && conn.PgConn() != nil {
		pid = conn.PgConn().PID()
	}
	evt.Err(err).
		Str("sql", squashSQL(st.sql, 160)).
		Int64("ms", dur.Milliseconds()).
		Uint32("pg_pid", pid).
		Msg(msg)
}

func (tracer) TraceConnectStart(ctx context.Context, _ pgx.TraceConnectStartData) context.Context {
	return context.WithValue(ctx, traceKey{}, traceStart{at: time.Now()})
}

func (tracer) TraceConnectEnd(ctx context.Context, d pgx.TraceConnectEndData) {
	st, ok := ctx.Value(traceKey{}).(traceStart)
	if !ok {
		return
	}
	dur := time.Since(st.at)
	if d.Err != nil {
		log.Error().Err(d.Err).Int64("ms", dur.Milliseconds()).Msg("db connect failed")
		return
	}
	var pid uint32
	if d.Conn != nil && d.Conn.PgConn() != nil {
		pid = d.Conn.PgConn().PID()
	}
	log.Info().Int64("ms", dur.Milliseconds()).Uint32("pg_pid", pid).Msg("db connect")
}

func squashSQL(sql string, max int) string {
	s := strings.Join(strings.Fields(sql), " ")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
