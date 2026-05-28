package db

import (
	"database/sql"
	"fmt"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/rs/zerolog/log"
)

// DB wraps the sql.DB connection to Postgres.
type DB struct {
	*sql.DB
}

// New opens a Postgres connection using the provided DSN
// (e.g. "postgres://user:pass@host:5432/dbname?sslmode=disable")
// and ensures the schema is up to date.
//
// Setting DROP_AND_RESEED=1 wipes the public schema before re-running
// migrations. Used once when switching the schema (e.g. the auth
// rework that flipped users.id to UUID). Flip the flag back off after
// the next successful boot.
func New(dsn string) (*DB, error) {
	if dsn == "" {
		return nil, fmt.Errorf("empty DATABASE_URL")
	}

	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := conn.Ping(); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}

	d := &DB{DB: conn}

	if os.Getenv("DROP_AND_RESEED") == "1" {
		log.Warn().Msg("DROP_AND_RESEED=1 — wiping public schema before migrate")
		if _, err := d.Exec(`DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
			return nil, fmt.Errorf("drop schema: %w", err)
		}
	}

	if err := d.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return d, nil
}

func (d *DB) migrate() error {
	schema := `
	CREATE EXTENSION IF NOT EXISTS citext;

	-- ─── Auth ─────────────────────────────────────────────────────────
	-- users.id is a UUIDv7 (time-sortable) minted in Go, never DEFAULT.
	-- email is CITEXT so case differences (Alice@x vs alice@x) collide.
	-- name + email are mandatory; password_hash is gone (auth is now
	-- OAuth + email-TOTP).
	CREATE TABLE IF NOT EXISTS users (
		id            UUID         PRIMARY KEY,
		email         CITEXT       NOT NULL UNIQUE,
		name          TEXT         NOT NULL DEFAULT '',
		onboarded_at  TIMESTAMPTZ,
		created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
		deleted_at    TIMESTAMPTZ
	);
	CREATE INDEX IF NOT EXISTS idx_users_deleted_at ON users(deleted_at) WHERE deleted_at IS NOT NULL;

	-- One row per (provider, provider_subject) — Google sub, GitHub user
	-- id, or the email itself for the 'email' provider. Multiple
	-- identities can point at the same user — that's the account-linking
	-- story: log in via any of the three with the same verified email
	-- and you land on the same row.
	CREATE TABLE IF NOT EXISTS auth_identities (
		id               UUID         PRIMARY KEY,
		user_id          UUID         NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		provider         TEXT         NOT NULL,
		provider_subject TEXT         NOT NULL,
		email            CITEXT       NOT NULL,
		created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
		last_used_at     TIMESTAMPTZ,
		UNIQUE(provider, provider_subject)
	);
	CREATE INDEX IF NOT EXISTS idx_auth_identities_user ON auth_identities(user_id);

	-- Single-use codes for email-TOTP login AND for the "verify before
	-- linking" challenge when an OAuth provider asserts an unverified
	-- email collision. code_hash is SHA-256 of the 6-digit code.
	CREATE TABLE IF NOT EXISTS email_codes (
		id            UUID         PRIMARY KEY,
		email         CITEXT       NOT NULL,
		code_hash     BYTEA        NOT NULL,
		purpose       TEXT         NOT NULL DEFAULT 'login',
		link_user_id  UUID         REFERENCES users(id) ON DELETE CASCADE,
		link_provider TEXT,
		link_subject  TEXT,
		link_name     TEXT,
		attempts      INTEGER      NOT NULL DEFAULT 0,
		expires_at    TIMESTAMPTZ  NOT NULL,
		consumed_at   TIMESTAMPTZ,
		created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_email_codes_email_active ON email_codes(email, expires_at) WHERE consumed_at IS NULL;

	-- Refresh tokens are now looked up by SHA-256 hash (BYTEA, UNIQUE)
	-- — one indexed lookup per refresh, no bcrypt sweep. Hot path went
	-- from O(N) bcrypt to O(1) index seek.
	CREATE TABLE IF NOT EXISTS refresh_tokens (
		id          UUID         PRIMARY KEY,
		user_id     UUID         NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		token_hash  BYTEA        NOT NULL UNIQUE,
		expires_at  TIMESTAMPTZ  NOT NULL,
		created_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_refresh_tokens_user ON refresh_tokens(user_id);

	-- ─── Content (BIGSERIAL row PK; user_id is UUID) ──────────────────
	CREATE TABLE IF NOT EXISTS memos (
		id         BIGSERIAL   PRIMARY KEY,
		user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		content    TEXT        NOT NULL DEFAULT '',
		pinned     BOOLEAN     NOT NULL DEFAULT FALSE,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_memos_user ON memos(user_id);

	CREATE TABLE IF NOT EXISTS task_lists (
		id         BIGSERIAL   PRIMARY KEY,
		user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		name       TEXT        NOT NULL DEFAULT '',
		color      TEXT        NOT NULL DEFAULT '#2D5A4F',
		icon       TEXT        NOT NULL DEFAULT 'list',
		sort_order INTEGER     NOT NULL DEFAULT 0,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_task_lists_user ON task_lists(user_id);

	CREATE TABLE IF NOT EXISTS tasks (
		id               BIGSERIAL   PRIMARY KEY,
		user_id          UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		title            TEXT        NOT NULL DEFAULT '',
		description      TEXT        NOT NULL DEFAULT '',
		status           TEXT        NOT NULL DEFAULT 'todo',
		priority         TEXT        NOT NULL DEFAULT 'medium',
		due_date         DATE,
		scheduled_at     TIMESTAMPTZ,
		duration_minutes INTEGER     NOT NULL DEFAULT 30,
		list_id          BIGINT      REFERENCES task_lists(id) ON DELETE SET NULL,
		parent_task_id   BIGINT      REFERENCES tasks(id) ON DELETE CASCADE,
		steps            JSONB       NOT NULL DEFAULT '[]'::jsonb,
		important        BOOLEAN     NOT NULL DEFAULT FALSE,
		sort_order       INTEGER     NOT NULL DEFAULT 0,
		created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_tasks_user ON tasks(user_id);
	CREATE INDEX IF NOT EXISTS idx_tasks_list ON tasks(list_id);
	CREATE INDEX IF NOT EXISTS idx_tasks_parent ON tasks(parent_task_id);

	CREATE TABLE IF NOT EXISTS habits (
		id         BIGSERIAL   PRIMARY KEY,
		user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		name       TEXT        NOT NULL DEFAULT '',
		frequency  TEXT        NOT NULL DEFAULT 'daily',
		color      TEXT        NOT NULL DEFAULT '#2D5A4F',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_habits_user ON habits(user_id);

	CREATE TABLE IF NOT EXISTS habit_logs (
		id          BIGSERIAL PRIMARY KEY,
		user_id     UUID      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		habit_id    BIGINT    NOT NULL REFERENCES habits(id) ON DELETE CASCADE,
		logged_date DATE      NOT NULL,
		UNIQUE(user_id, habit_id, logged_date)
	);
	CREATE INDEX IF NOT EXISTS idx_habit_logs_user ON habit_logs(user_id);

	CREATE TABLE IF NOT EXISTS media (
		id               BIGSERIAL   PRIMARY KEY,
		user_id          UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		title            TEXT        NOT NULL DEFAULT '',
		type             TEXT        NOT NULL DEFAULT 'movie',
		status           TEXT        NOT NULL DEFAULT 'pending',
		rating           INTEGER,
		notes            TEXT        NOT NULL DEFAULT '',
		platform         TEXT        NOT NULL DEFAULT '',
		poster_url       TEXT        NOT NULL DEFAULT '',
		year             INTEGER,
		genre            TEXT        NOT NULL DEFAULT '',
		external_id      TEXT        NOT NULL DEFAULT '',
		episodes_watched INTEGER     NOT NULL DEFAULT 0,
		episodes_total   INTEGER     NOT NULL DEFAULT 0,
		seasons_watched  INTEGER     NOT NULL DEFAULT 0,
		seasons_total    INTEGER     NOT NULL DEFAULT 0,
		season_episodes  JSONB       NOT NULL DEFAULT '[]'::jsonb,
		collection_id    TEXT        NOT NULL DEFAULT '',
		collection_name  TEXT        NOT NULL DEFAULT '',
		created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_media_user ON media(user_id);
	CREATE INDEX IF NOT EXISTS idx_media_collection ON media(user_id, collection_id) WHERE collection_id <> '';

	CREATE TABLE IF NOT EXISTS media_events (
		id         BIGSERIAL   PRIMARY KEY,
		user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		media_id   BIGINT      NOT NULL REFERENCES media(id) ON DELETE CASCADE,
		kind       TEXT        NOT NULL,
		meta       JSONB       NOT NULL DEFAULT '{}'::jsonb,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_media_events_media ON media_events(media_id, created_at DESC);
	CREATE INDEX IF NOT EXISTS idx_media_events_user_completed ON media_events(user_id, created_at DESC)
		WHERE kind = 'completed';

	CREATE TABLE IF NOT EXISTS journal_entries (
		id             BIGSERIAL   PRIMARY KEY,
		user_id        UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		date           DATE        NOT NULL,
		blob_key       TEXT        NOT NULL DEFAULT '',
		mood           TEXT,
		location_label TEXT        NOT NULL DEFAULT '',
		location_lat   NUMERIC(9,6),
		location_lon   NUMERIC(9,6),
		attachments    JSONB       NOT NULL DEFAULT '[]'::jsonb,
		created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		UNIQUE(user_id, date)
	);

	-- Weekly journal entry. Same pattern as journal_entries but keyed by
	-- ISO week-year + week number (so 2024-W52 / 2025-W01 stay distinct).
	-- Content lives in object storage under journal/weekly/<key>.md.
	CREATE TABLE IF NOT EXISTS journal_weekly (
		id         BIGSERIAL   PRIMARY KEY,
		user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		iso_year   INTEGER     NOT NULL,
		iso_week   INTEGER     NOT NULL,
		blob_key   TEXT        NOT NULL DEFAULT '',
		mood       TEXT,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		UNIQUE(user_id, iso_year, iso_week)
	);
	CREATE INDEX IF NOT EXISTS idx_journal_weekly_user ON journal_weekly(user_id, iso_year DESC, iso_week DESC);

	CREATE TABLE IF NOT EXISTS notes (
		id         BIGSERIAL   PRIMARY KEY,
		user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		title      TEXT        NOT NULL DEFAULT '',
		blob_key   TEXT        NOT NULL DEFAULT '',
		folder     TEXT        NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_notes_user ON notes(user_id);

	CREATE TABLE IF NOT EXISTS note_folders (
		user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		path       TEXT        NOT NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (user_id, path)
	);

	CREATE TABLE IF NOT EXISTS task_due_history (
		id          BIGSERIAL   PRIMARY KEY,
		user_id     UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		task_id     BIGINT      NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
		due_date    DATE        NOT NULL,
		outcome     TEXT        NOT NULL DEFAULT 'missed',
		recorded_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_task_due_history_user ON task_due_history(user_id);
	CREATE INDEX IF NOT EXISTS idx_task_due_history_date ON task_due_history(due_date);
	CREATE INDEX IF NOT EXISTS idx_task_due_history_task ON task_due_history(task_id);

	CREATE TABLE IF NOT EXISTS tags (
		id          BIGSERIAL PRIMARY KEY,
		user_id     UUID      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		entity_type TEXT      NOT NULL,
		entity_id   BIGINT    NOT NULL,
		tag         TEXT      NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_tags_user ON tags(user_id);
	CREATE INDEX IF NOT EXISTS idx_tags_tag ON tags(user_id, tag);
	CREATE INDEX IF NOT EXISTS idx_tags_entity ON tags(user_id, entity_type, entity_id);

	CREATE TABLE IF NOT EXISTS backlinks (
		id          BIGSERIAL PRIMARY KEY,
		user_id     UUID      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		source_type TEXT      NOT NULL,
		source_id   BIGINT    NOT NULL,
		target_ref  TEXT      NOT NULL DEFAULT ''
	);
	CREATE INDEX IF NOT EXISTS idx_backlinks_user ON backlinks(user_id);
	CREATE INDEX IF NOT EXISTS idx_backlinks_ref ON backlinks(user_id, target_ref);
	CREATE INDEX IF NOT EXISTS idx_backlinks_source ON backlinks(user_id, source_type, source_id);

	-- ─── Finance ─────────────────────────────────────────────────────
	CREATE TABLE IF NOT EXISTS fin_accounts (
		id              BIGSERIAL   PRIMARY KEY,
		user_id         UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		name            TEXT        NOT NULL DEFAULT '',
		type            TEXT        NOT NULL DEFAULT 'savings',
		institution     TEXT        NOT NULL DEFAULT '',
		currency        TEXT        NOT NULL DEFAULT 'INR',
		opening_balance NUMERIC(14,2) NOT NULL DEFAULT 0,
		credit_limit    NUMERIC(14,2),
		statement_day   INTEGER,
		due_day         INTEGER,
		cashback_type   TEXT        NOT NULL DEFAULT 'none',
		cashback_value  NUMERIC(8,2) NOT NULL DEFAULT 0,
		color           TEXT        NOT NULL DEFAULT '#2D5A4F',
		archived        BOOLEAN     NOT NULL DEFAULT FALSE,
		created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_fin_accounts_user ON fin_accounts(user_id);

	CREATE TABLE IF NOT EXISTS fin_categories (
		id         BIGSERIAL PRIMARY KEY,
		user_id    UUID      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		name       TEXT      NOT NULL DEFAULT '',
		kind       TEXT      NOT NULL DEFAULT 'expense',
		color      TEXT      NOT NULL DEFAULT '#6B7280',
		icon       TEXT      NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_fin_categories_user ON fin_categories(user_id);

	CREATE TABLE IF NOT EXISTS fin_transactions (
		id              BIGSERIAL   PRIMARY KEY,
		user_id         UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		account_id      BIGINT      NOT NULL REFERENCES fin_accounts(id) ON DELETE CASCADE,
		category_id     BIGINT      REFERENCES fin_categories(id) ON DELETE SET NULL,
		type            TEXT        NOT NULL DEFAULT 'expense',
		amount          NUMERIC(14,2) NOT NULL DEFAULT 0,
		description     TEXT        NOT NULL DEFAULT '',
		txn_date        DATE        NOT NULL,
		transfer_pair   BIGINT,
		linked_account  BIGINT      REFERENCES fin_accounts(id) ON DELETE SET NULL,
		created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_fin_transactions_user ON fin_transactions(user_id);
	CREATE INDEX IF NOT EXISTS idx_fin_transactions_account ON fin_transactions(account_id);
	CREATE INDEX IF NOT EXISTS idx_fin_transactions_date ON fin_transactions(user_id, txn_date);

	CREATE TABLE IF NOT EXISTS fin_budgets (
		id          BIGSERIAL   PRIMARY KEY,
		user_id     UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		name        TEXT        NOT NULL DEFAULT '',
		period      TEXT        NOT NULL DEFAULT 'monthly',
		start_date  DATE        NOT NULL,
		end_date    DATE        NOT NULL,
		total_amount NUMERIC(14,2) NOT NULL DEFAULT 0,
		created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_fin_budgets_user ON fin_budgets(user_id);

	CREATE TABLE IF NOT EXISTS fin_budget_items (
		id          BIGSERIAL PRIMARY KEY,
		budget_id   BIGINT    NOT NULL REFERENCES fin_budgets(id) ON DELETE CASCADE,
		category_id BIGINT    REFERENCES fin_categories(id) ON DELETE SET NULL,
		amount      NUMERIC(14,2) NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_fin_budget_items_budget ON fin_budget_items(budget_id);

	CREATE TABLE IF NOT EXISTS fin_investments (
		id              BIGSERIAL   PRIMARY KEY,
		user_id         UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		name            TEXT        NOT NULL DEFAULT '',
		type            TEXT        NOT NULL DEFAULT 'sip',
		account_id      BIGINT      REFERENCES fin_accounts(id) ON DELETE SET NULL,
		invested_amount NUMERIC(14,2) NOT NULL DEFAULT 0,
		current_value   NUMERIC(14,2) NOT NULL DEFAULT 0,
		monthly_amount  NUMERIC(14,2) NOT NULL DEFAULT 0,
		frequency       TEXT        NOT NULL DEFAULT 'monthly',
		start_date      DATE,
		maturity_date   DATE,
		expected_return NUMERIC(8,4) NOT NULL DEFAULT 0,
		notes           TEXT        NOT NULL DEFAULT '',
		last_updated    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_fin_investments_user ON fin_investments(user_id);

	CREATE TABLE IF NOT EXISTS fin_virtual_savings (
		id             BIGSERIAL   PRIMARY KEY,
		user_id        UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		account_id     BIGINT      NOT NULL REFERENCES fin_accounts(id) ON DELETE CASCADE,
		name           TEXT        NOT NULL DEFAULT '',
		target_amount  NUMERIC(14,2) NOT NULL DEFAULT 0,
		current_amount NUMERIC(14,2) NOT NULL DEFAULT 0,
		color          TEXT        NOT NULL DEFAULT '#2D5A4F',
		created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_fin_virtual_savings_user ON fin_virtual_savings(user_id);

	CREATE TABLE IF NOT EXISTS fin_cc_statements (
		id              BIGSERIAL   PRIMARY KEY,
		user_id         UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		account_id      BIGINT      NOT NULL REFERENCES fin_accounts(id) ON DELETE CASCADE,
		statement_date  DATE        NOT NULL,
		due_date        DATE        NOT NULL,
		amount_due      NUMERIC(14,2) NOT NULL DEFAULT 0,
		cashback_earned NUMERIC(14,2) NOT NULL DEFAULT 0,
		paid            BOOLEAN     NOT NULL DEFAULT FALSE,
		paid_at         TIMESTAMPTZ,
		created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_fin_cc_statements_user ON fin_cc_statements(user_id);
	CREATE INDEX IF NOT EXISTS idx_fin_cc_statements_account ON fin_cc_statements(account_id);

	CREATE TABLE IF NOT EXISTS fin_networth_snapshots (
		id            BIGSERIAL PRIMARY KEY,
		user_id       UUID      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		snapshot_date DATE      NOT NULL,
		assets        NUMERIC(14,2) NOT NULL DEFAULT 0,
		liabilities   NUMERIC(14,2) NOT NULL DEFAULT 0,
		net_worth     NUMERIC(14,2) NOT NULL DEFAULT 0,
		UNIQUE(user_id, snapshot_date)
	);
	CREATE INDEX IF NOT EXISTS idx_fin_networth_user ON fin_networth_snapshots(user_id);

	CREATE TABLE IF NOT EXISTS fin_billers (
		id              BIGSERIAL   PRIMARY KEY,
		user_id         UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		name            TEXT        NOT NULL DEFAULT '',
		amount          NUMERIC(14,2) NOT NULL DEFAULT 0,
		frequency       TEXT        NOT NULL DEFAULT 'monthly',
		next_due_date   DATE        NOT NULL,
		account_id      BIGINT      REFERENCES fin_accounts(id) ON DELETE SET NULL,
		category_id     BIGINT      REFERENCES fin_categories(id) ON DELETE SET NULL,
		is_subscription BOOLEAN     NOT NULL DEFAULT FALSE,
		auto_renew      BOOLEAN     NOT NULL DEFAULT FALSE,
		alert_days      INTEGER     NOT NULL DEFAULT 3,
		color           TEXT        NOT NULL DEFAULT '#2D5A4F',
		notes           TEXT        NOT NULL DEFAULT '',
		archived        BOOLEAN     NOT NULL DEFAULT FALSE,
		last_run_at     TIMESTAMPTZ,
		created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_fin_billers_user ON fin_billers(user_id);
	CREATE INDEX IF NOT EXISTS idx_fin_billers_due ON fin_billers(user_id, next_due_date) WHERE archived = FALSE;

	CREATE TABLE IF NOT EXISTS fin_biller_payments (
		id          BIGSERIAL   PRIMARY KEY,
		user_id     UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		biller_id   BIGINT      NOT NULL REFERENCES fin_billers(id) ON DELETE CASCADE,
		txn_id      BIGINT      REFERENCES fin_transactions(id) ON DELETE SET NULL,
		due_date    DATE        NOT NULL,
		paid_date   DATE        NOT NULL,
		amount      NUMERIC(14,2) NOT NULL DEFAULT 0,
		auto        BOOLEAN     NOT NULL DEFAULT FALSE,
		created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		UNIQUE(biller_id, due_date)
	);
	CREATE INDEX IF NOT EXISTS idx_fin_biller_payments_user ON fin_biller_payments(user_id);

	CREATE TABLE IF NOT EXISTS fin_biller_alerts (
		id          BIGSERIAL   PRIMARY KEY,
		user_id     UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		biller_id   BIGINT      NOT NULL REFERENCES fin_billers(id) ON DELETE CASCADE,
		kind        TEXT        NOT NULL,
		due_date    DATE        NOT NULL,
		seen        BOOLEAN     NOT NULL DEFAULT FALSE,
		created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		UNIQUE(biller_id, kind, due_date)
	);
	CREATE INDEX IF NOT EXISTS idx_fin_biller_alerts_user ON fin_biller_alerts(user_id, seen);

	-- ─── Insights / Themes / AI ───────────────────────────────────────
	CREATE TABLE IF NOT EXISTS insights (
		id           BIGSERIAL   PRIMARY KEY,
		user_id      UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		window_key   TEXT        NOT NULL,
		kind         TEXT        NOT NULL,
		title        TEXT        NOT NULL DEFAULT '',
		body         TEXT        NOT NULL DEFAULT '',
		score        NUMERIC(8,4) NOT NULL DEFAULT 0,
		evidence     JSONB       NOT NULL DEFAULT '{}'::jsonb,
		pinned       BOOLEAN     NOT NULL DEFAULT FALSE,
		dismissed_at TIMESTAMPTZ,
		generated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_insights_user ON insights(user_id, generated_at DESC);

	CREATE TABLE IF NOT EXISTS insight_runs (
		id        BIGSERIAL   PRIMARY KEY,
		user_id   UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		window_key TEXT       NOT NULL,
		status    TEXT        NOT NULL DEFAULT 'ok',
		notes     TEXT        NOT NULL DEFAULT '',
		run_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_insight_runs_user ON insight_runs(user_id, window_key, run_at DESC);

	CREATE TABLE IF NOT EXISTS user_themes (
		id         BIGSERIAL   PRIMARY KEY,
		user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		name       TEXT        NOT NULL DEFAULT '',
		source     TEXT        NOT NULL DEFAULT 'ai',
		seeds      JSONB       NOT NULL DEFAULT '{}'::jsonb,
		prompt     TEXT        NOT NULL DEFAULT '',
		mode_pref  TEXT        NOT NULL DEFAULT 'auto',
		is_active  BOOLEAN     NOT NULL DEFAULT FALSE,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_user_themes_user ON user_themes(user_id);
	CREATE UNIQUE INDEX IF NOT EXISTS uniq_user_active_theme
		ON user_themes(user_id) WHERE is_active = TRUE;

	CREATE TABLE IF NOT EXISTS ai_sessions (
		id         BIGSERIAL   PRIMARY KEY,
		user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		title      TEXT        NOT NULL DEFAULT '',
		messages   JSONB       NOT NULL DEFAULT '[]'::jsonb,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_ai_sessions_user ON ai_sessions(user_id, updated_at DESC);

	CREATE TABLE IF NOT EXISTS thinking_projects (
		id             BIGSERIAL   PRIMARY KEY,
		user_id        UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		title          TEXT        NOT NULL DEFAULT '',
		description    TEXT        NOT NULL DEFAULT '',
		thesis         TEXT        NOT NULL DEFAULT '',
		gap_questions  JSONB       NOT NULL DEFAULT '[]'::jsonb,
		synthesized_at TIMESTAMPTZ,
		created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_thinking_projects_user ON thinking_projects(user_id, updated_at DESC);

	CREATE TABLE IF NOT EXISTS thinking_cards (
		id            BIGSERIAL   PRIMARY KEY,
		project_id    BIGINT      NOT NULL REFERENCES thinking_projects(id) ON DELETE CASCADE,
		user_id       UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		kind          TEXT        NOT NULL DEFAULT 'note',
		content       TEXT        NOT NULL DEFAULT '',
		ai_enrichment JSONB       NOT NULL DEFAULT '{}'::jsonb,
		enriched_at   TIMESTAMPTZ,
		created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_thinking_cards_project ON thinking_cards(project_id, created_at DESC);
	CREATE INDEX IF NOT EXISTS idx_thinking_cards_user ON thinking_cards(user_id);

	-- ─── Reminders (extends tasks) ────────────────────────────────────
	-- Reminders ride on tasks rather than a separate model: scheduled_at
	-- (already present) is the event instant, remind gates the email, and
	-- reminded_at is the sent-sentinel (NULL = unsent). Clearing it on edit
	-- re-arms the reminder; the */5 cron uses it for idempotency.
	ALTER TABLE tasks       ADD COLUMN IF NOT EXISTS remind      BOOLEAN     NOT NULL DEFAULT FALSE;
	ALTER TABLE tasks       ADD COLUMN IF NOT EXISTS reminded_at TIMESTAMPTZ;
	-- IANA timezone captured from the browser once, used to render the
	-- reminder email in the user's local clock time. NULL until captured.
	ALTER TABLE users       ADD COLUMN IF NOT EXISTS timezone    TEXT;
	-- Opt-in: when on (and the biller is not auto_renew) the biller cron
	-- spawns one bill-pay reminder task per due cycle.
	ALTER TABLE fin_billers ADD COLUMN IF NOT EXISTS remind_task BOOLEAN     NOT NULL DEFAULT FALSE;
	-- Hot path for the reminder cron: only the un-sent, remind-on rows.
	CREATE INDEX IF NOT EXISTS idx_tasks_remind ON tasks(scheduled_at)
		WHERE remind = TRUE AND reminded_at IS NULL;

	-- ─── Task audit trail ─────────────────────────────────────────────
	-- One row per tracked mutation, surfaced as a GitHub-style timeline in
	-- the task detail drawer. kind ∈ created|status|title|list. Note/body
	-- edits are deliberately NOT tracked. Distinct from task_due_history,
	-- which records due-date misses with different semantics.
	CREATE TABLE IF NOT EXISTS task_events (
		id         BIGSERIAL   PRIMARY KEY,
		user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		task_id    BIGINT      NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
		kind       TEXT        NOT NULL,
		from_val   TEXT        NOT NULL DEFAULT '',
		to_val     TEXT        NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_task_events_task ON task_events(task_id, created_at);
	`
	if _, err := d.Exec(schema); err != nil {
		return err
	}
	return nil
}
