package db

import (
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// DB wraps the sql.DB connection to Postgres.
type DB struct {
	*sql.DB
}

// New opens a Postgres connection using the provided DSN
// (e.g. "postgres://user:pass@host:5432/dbname?sslmode=disable")
// and ensures the schema is up to date.
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
	if err := d.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return d, nil
}

func (d *DB) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS users (
		id            BIGSERIAL    PRIMARY KEY,
		email         TEXT         NOT NULL UNIQUE,
		password_hash TEXT         NOT NULL,
		created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
	);

	CREATE TABLE IF NOT EXISTS refresh_tokens (
		id          BIGSERIAL   PRIMARY KEY,
		user_id     BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		token_hash  TEXT        NOT NULL,
		expires_at  TIMESTAMPTZ NOT NULL,
		created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_refresh_tokens_user ON refresh_tokens(user_id);

	CREATE TABLE IF NOT EXISTS memos (
		id         BIGSERIAL   PRIMARY KEY,
		user_id    BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		content    TEXT        NOT NULL DEFAULT '',
		pinned     BOOLEAN     NOT NULL DEFAULT FALSE,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_memos_user ON memos(user_id);

	CREATE TABLE IF NOT EXISTS tasks (
		id          BIGSERIAL   PRIMARY KEY,
		user_id     BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		title       TEXT        NOT NULL DEFAULT '',
		description TEXT        NOT NULL DEFAULT '',
		status      TEXT        NOT NULL DEFAULT 'todo',
		priority    TEXT        NOT NULL DEFAULT 'medium',
		due_date    DATE,
		created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_tasks_user ON tasks(user_id);
	-- scheduled_at lets the agent answer "find a free 1-hour slot" by joining
	-- against actual time blocks. Nullable so existing tasks stay valid.
	ALTER TABLE tasks ADD COLUMN IF NOT EXISTS scheduled_at TIMESTAMPTZ;
	ALTER TABLE tasks ADD COLUMN IF NOT EXISTS duration_minutes INTEGER NOT NULL DEFAULT 30;
	-- Tasks v2 — list-first redesign (Microsoft Todo inspired).
	-- list_id groups tasks under a custom list (e.g. "Work", "Home").
	-- parent_task_id allows nested subtasks.
	-- steps is an inline checklist embedded in the task itself
	--   (shape: [{"id":string,"text":string,"done":bool}]).
	-- important is the starred flag for the smart "Important" list.
	-- sort_order lets users hand-order tasks within a list.
	CREATE TABLE IF NOT EXISTS task_lists (
		id         BIGSERIAL   PRIMARY KEY,
		user_id    BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		name       TEXT        NOT NULL DEFAULT '',
		color      TEXT        NOT NULL DEFAULT '#2D5A4F',
		icon       TEXT        NOT NULL DEFAULT 'list',
		sort_order INTEGER     NOT NULL DEFAULT 0,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_task_lists_user ON task_lists(user_id);
	ALTER TABLE tasks ADD COLUMN IF NOT EXISTS list_id BIGINT REFERENCES task_lists(id) ON DELETE SET NULL;
	ALTER TABLE tasks ADD COLUMN IF NOT EXISTS parent_task_id BIGINT REFERENCES tasks(id) ON DELETE CASCADE;
	ALTER TABLE tasks ADD COLUMN IF NOT EXISTS steps JSONB NOT NULL DEFAULT '[]'::jsonb;
	ALTER TABLE tasks ADD COLUMN IF NOT EXISTS important BOOLEAN NOT NULL DEFAULT FALSE;
	ALTER TABLE tasks ADD COLUMN IF NOT EXISTS sort_order INTEGER NOT NULL DEFAULT 0;
	CREATE INDEX IF NOT EXISTS idx_tasks_list ON tasks(list_id);
	CREATE INDEX IF NOT EXISTS idx_tasks_parent ON tasks(parent_task_id);

	CREATE TABLE IF NOT EXISTS habits (
		id         BIGSERIAL   PRIMARY KEY,
		user_id    BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		name       TEXT        NOT NULL DEFAULT '',
		frequency  TEXT        NOT NULL DEFAULT 'daily',
		color      TEXT        NOT NULL DEFAULT '#2D5A4F',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_habits_user ON habits(user_id);

	CREATE TABLE IF NOT EXISTS habit_logs (
		id          BIGSERIAL PRIMARY KEY,
		user_id     BIGINT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		habit_id    BIGINT    NOT NULL REFERENCES habits(id) ON DELETE CASCADE,
		logged_date DATE      NOT NULL,
		UNIQUE(user_id, habit_id, logged_date)
	);
	CREATE INDEX IF NOT EXISTS idx_habit_logs_user ON habit_logs(user_id);

	CREATE TABLE IF NOT EXISTS media (
		id               BIGSERIAL   PRIMARY KEY,
		user_id          BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
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
		created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_media_user ON media(user_id);
	-- Show progress: per-season episode counts (e.g. [10,12,8]) so the UI
	-- can ask for current_season + current_episode and derive the rest
	-- without making the user track totals manually. Filled in on add via
	-- TMDB; old rows stay valid (empty array means "no per-season data").
	ALTER TABLE media ADD COLUMN IF NOT EXISTS season_episodes JSONB NOT NULL DEFAULT '[]'::jsonb;
	-- Movie collection — TMDB's "belongs_to_collection" (Mission
	-- Impossible Collection, Marvel Phase X, etc.). Lets us group movies
	-- and show "X of N watched" per series.
	ALTER TABLE media ADD COLUMN IF NOT EXISTS collection_id   TEXT NOT NULL DEFAULT '';
	ALTER TABLE media ADD COLUMN IF NOT EXISTS collection_name TEXT NOT NULL DEFAULT '';
	CREATE INDEX IF NOT EXISTS idx_media_collection ON media(user_id, collection_id) WHERE collection_id <> '';
	-- Watch-history timeline. Auto-written on add/update so the user
	-- can see "added Apr 12 / finished S2 May 4 / completed Jun 2" for
	-- any title. kind is one of: added, started, progress, completed,
	-- rating, dropped, restarted. meta carries per-event detail
	-- (season/episode for shows, page for books).
	CREATE TABLE IF NOT EXISTS media_events (
		id         BIGSERIAL   PRIMARY KEY,
		user_id    BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		media_id   BIGINT      NOT NULL REFERENCES media(id) ON DELETE CASCADE,
		kind       TEXT        NOT NULL,
		meta       JSONB       NOT NULL DEFAULT '{}'::jsonb,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_media_events_media ON media_events(media_id, created_at DESC);
	CREATE INDEX IF NOT EXISTS idx_media_events_user_completed ON media_events(user_id, created_at DESC)
		WHERE kind = 'completed';

	CREATE TABLE IF NOT EXISTS journal_entries (
		id         BIGSERIAL   PRIMARY KEY,
		user_id    BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		date       DATE        NOT NULL,
		blob_key   TEXT        NOT NULL DEFAULT '',
		mood       TEXT,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		UNIQUE(user_id, date)
	);

	CREATE TABLE IF NOT EXISTS notes (
		id         BIGSERIAL   PRIMARY KEY,
		user_id    BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		title      TEXT        NOT NULL DEFAULT '',
		blob_key   TEXT        NOT NULL DEFAULT '',
		folder     TEXT        NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_notes_user ON notes(user_id);

	CREATE TABLE IF NOT EXISTS note_folders (
		user_id    BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		path       TEXT        NOT NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (user_id, path)
	);

	CREATE TABLE IF NOT EXISTS task_due_history (
		id          BIGSERIAL   PRIMARY KEY,
		user_id     BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
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
		user_id     BIGINT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		entity_type TEXT      NOT NULL,
		entity_id   BIGINT    NOT NULL,
		tag         TEXT      NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_tags_user ON tags(user_id);
	CREATE INDEX IF NOT EXISTS idx_tags_tag ON tags(user_id, tag);
	CREATE INDEX IF NOT EXISTS idx_tags_entity ON tags(user_id, entity_type, entity_id);

	CREATE TABLE IF NOT EXISTS backlinks (
		id          BIGSERIAL PRIMARY KEY,
		user_id     BIGINT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		source_type TEXT      NOT NULL,
		source_id   BIGINT    NOT NULL,
		target_ref  TEXT      NOT NULL DEFAULT ''
	);
	CREATE INDEX IF NOT EXISTS idx_backlinks_user ON backlinks(user_id);
	CREATE INDEX IF NOT EXISTS idx_backlinks_ref ON backlinks(user_id, target_ref);
	CREATE INDEX IF NOT EXISTS idx_backlinks_source ON backlinks(user_id, source_type, source_id);

	-- Finance ---------------------------------------------------------------
	CREATE TABLE IF NOT EXISTS fin_accounts (
		id              BIGSERIAL   PRIMARY KEY,
		user_id         BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
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
		id        BIGSERIAL PRIMARY KEY,
		user_id   BIGINT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		name      TEXT      NOT NULL DEFAULT '',
		kind      TEXT      NOT NULL DEFAULT 'expense',
		color     TEXT      NOT NULL DEFAULT '#6B7280',
		icon      TEXT      NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_fin_categories_user ON fin_categories(user_id);

	CREATE TABLE IF NOT EXISTS fin_transactions (
		id              BIGSERIAL   PRIMARY KEY,
		user_id         BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
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
		user_id     BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
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
		user_id         BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
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
		id           BIGSERIAL   PRIMARY KEY,
		user_id      BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		account_id   BIGINT      NOT NULL REFERENCES fin_accounts(id) ON DELETE CASCADE,
		name         TEXT        NOT NULL DEFAULT '',
		target_amount NUMERIC(14,2) NOT NULL DEFAULT 0,
		current_amount NUMERIC(14,2) NOT NULL DEFAULT 0,
		color        TEXT        NOT NULL DEFAULT '#2D5A4F',
		created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_fin_virtual_savings_user ON fin_virtual_savings(user_id);

	CREATE TABLE IF NOT EXISTS fin_cc_statements (
		id             BIGSERIAL   PRIMARY KEY,
		user_id        BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		account_id     BIGINT      NOT NULL REFERENCES fin_accounts(id) ON DELETE CASCADE,
		statement_date DATE        NOT NULL,
		due_date       DATE        NOT NULL,
		amount_due     NUMERIC(14,2) NOT NULL DEFAULT 0,
		cashback_earned NUMERIC(14,2) NOT NULL DEFAULT 0,
		paid           BOOLEAN     NOT NULL DEFAULT FALSE,
		paid_at        TIMESTAMPTZ,
		created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_fin_cc_statements_user ON fin_cc_statements(user_id);
	CREATE INDEX IF NOT EXISTS idx_fin_cc_statements_account ON fin_cc_statements(account_id);

	CREATE TABLE IF NOT EXISTS fin_networth_snapshots (
		id          BIGSERIAL PRIMARY KEY,
		user_id     BIGINT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		snapshot_date DATE    NOT NULL,
		assets      NUMERIC(14,2) NOT NULL DEFAULT 0,
		liabilities NUMERIC(14,2) NOT NULL DEFAULT 0,
		net_worth   NUMERIC(14,2) NOT NULL DEFAULT 0,
		UNIQUE(user_id, snapshot_date)
	);
	CREATE INDEX IF NOT EXISTS idx_fin_networth_user ON fin_networth_snapshots(user_id);

	-- AI ----------------------------------------------------------------
	-- One row per chat conversation. messages is the full history as a
	-- JSON array of {role, parts: [...]} entries (mirrors genai.Content).
	-- Trimmed to the last N entries before being sent to the model.
	CREATE TABLE IF NOT EXISTS ai_sessions (
		id         BIGSERIAL   PRIMARY KEY,
		user_id    BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		title      TEXT        NOT NULL DEFAULT '',
		messages   JSONB       NOT NULL DEFAULT '[]'::jsonb,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_ai_sessions_user ON ai_sessions(user_id, updated_at DESC);

	-- Soft-delete: users.deleted_at is set when a user requests account
	-- deletion. A background purge removes their data after 7 days so
	-- mistakes are recoverable via /auth/cancel-delete inside the window.
	ALTER TABLE users ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;
	CREATE INDEX IF NOT EXISTS idx_users_deleted_at ON users(deleted_at) WHERE deleted_at IS NOT NULL;
	`
	if _, err := d.Exec(schema); err != nil {
		return err
	}
	return nil
}
