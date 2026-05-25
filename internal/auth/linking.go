package auth

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// resolveOrLinkIdentity is the load-bearing piece of multi-provider
// sign-in. Given a provider-asserted identity, it returns the user_id
// the caller should mint a session for.
//
// Three outcomes:
//
//  1. The (provider, subject) pair already exists → return its user_id.
//
//  2. The email is new → create a fresh user + identity.
//
//  3. The email matches an existing user:
//     a. provider verified the email → link the identity automatically.
//     b. provider did NOT verify the email → do NOT link silently
//     (would let an attacker claim someone else's account via a
//     spoofed-but-unverified provider). Caller is expected to send a
//     TOTP challenge to the email and finalize on POST /email/verify.
//
// Returns (userID, needsLink, linkedNew).
//
//	needsLink=true   — caller should send a TOTP link challenge (3b).
//	linkedNew=true   — a new auth_identity row was just attached to an
//	                   existing user (3a). Caller surfaces this to the UI
//	                   so the user sees "linked X to your account".
func (s *Service) resolveOrLinkIdentity(ctx context.Context, provider, subject, email string, emailVerified bool, name string) (string, bool, bool, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if provider == "" || subject == "" {
		return "", false, false, errors.New("provider and subject required")
	}

	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return "", false, false, err
	}
	defer tx.Rollback()

	// 1. Fast path: identity already known.
	var existing string
	err = tx.QueryRowContext(ctx,
		`SELECT user_id FROM auth_identities WHERE provider=$1 AND provider_subject=$2`,
		provider, subject,
	).Scan(&existing)
	if err == nil {
		// NB: the previous version passed `existing` as $1 but never
		// referenced it in the SQL — pgx then reported
		// `could not determine data type of parameter $1 (SQLSTATE 42P18)`
		// because Postgres has no clue what type an unused parameter is.
		if _, err := tx.ExecContext(ctx,
			`UPDATE auth_identities SET last_used_at=NOW(), email=$1 WHERE provider=$2 AND provider_subject=$3`,
			email, provider, subject,
		); err != nil {
			return "", false, false, err
		}
		s.maybeClearSoftDelete(ctx, tx, existing)
		if err := tx.Commit(); err != nil {
			return "", false, false, err
		}
		return existing, false, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, false, err
	}

	// 2 + 3. Look for a user with this email.
	var userID string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM users WHERE email=$1`, email,
	).Scan(&userID)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		newID := NewID()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO users (id, email, name) VALUES ($1, $2, $3)`,
			newID, email, name,
		); err != nil {
			return "", false, false, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO auth_identities (id, user_id, provider, provider_subject, email, last_used_at)
			 VALUES ($1, $2, $3, $4, $5, NOW())`,
			NewID(), newID, provider, subject, email,
		); err != nil {
			return "", false, false, err
		}
		if err := tx.Commit(); err != nil {
			return "", false, false, err
		}
		return newID, false, false, nil

	case err != nil:
		return "", false, false, err

	default:
		if !emailVerified {
			if err := tx.Commit(); err != nil {
				return "", false, false, err
			}
			return userID, true, false, nil
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO auth_identities (id, user_id, provider, provider_subject, email, last_used_at)
			 VALUES ($1, $2, $3, $4, $5, NOW())`,
			NewID(), userID, provider, subject, email,
		); err != nil {
			return "", false, false, err
		}
		if name != "" {
			if _, err := tx.ExecContext(ctx,
				`UPDATE users SET name=$2 WHERE id=$1 AND COALESCE(name,'')=''`,
				userID, name,
			); err != nil {
				return "", false, false, err
			}
		}
		s.maybeClearSoftDelete(ctx, tx, userID)
		if err := tx.Commit(); err != nil {
			return "", false, false, err
		}
		return userID, false, true, nil
	}
}

// findOrCreateByEmail underpins email-TOTP sign-in. Creates a user row
// if missing; returns (userID, created, name).
func (s *Service) findOrCreateByEmail(ctx context.Context, email, name string) (string, bool, string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	var id, gotName string
	err := s.DB.QueryRowContext(ctx,
		`SELECT id, name FROM users WHERE email=$1`, email,
	).Scan(&id, &gotName)
	if err == nil {
		// Existing user — fill blank name if caller supplied one.
		if name != "" && gotName == "" {
			s.DB.ExecContext(ctx, `UPDATE users SET name=$2 WHERE id=$1`, id, name)
			gotName = name
		}
		s.DB.ExecContext(ctx, `UPDATE users SET deleted_at=NULL WHERE id=$1 AND deleted_at IS NOT NULL`, id)
		return id, false, gotName, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, "", err
	}
	newID := NewID()
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO users (id, email, name) VALUES ($1, $2, $3)`,
		newID, email, name,
	); err != nil {
		return "", false, "", err
	}
	return newID, true, name, nil
}

// maybeClearSoftDelete pulls the user out of the 7-day grace period if
// they sign in again before it elapses.
func (s *Service) maybeClearSoftDelete(ctx context.Context, tx *sql.Tx, userID string) {
	tx.ExecContext(ctx, `UPDATE users SET deleted_at=NULL WHERE id=$1 AND deleted_at IS NOT NULL`, userID)
}
