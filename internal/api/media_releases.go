package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"

	"sajni/internal/db"
	"sajni/internal/push"
)

const mediaReleaseClaimLease = 5 * time.Minute

type releaseUser struct {
	id, email, name, timezone string
	// channel is users.notify_channel (email | push | both); it decides
	// whether the email copy goes out once push has landed.
	channel string
}

type claimedRelease struct {
	id          int64
	title       string
	releaseDate string
}

// ProcessMediaReleaseCron nudges at 10:00 in each owner's timezone on the day
// before release, then promotes released movies into the pending queue. Both
// actions are scoped by media.user_id: owning a saved row is the only way to
// become a recipient. Delivery is push + email like every other nudge, so a
// push-only user still hears about a film landing tomorrow.
func ProcessMediaReleaseCron(ctx context.Context, deps Deps) (reminded, graduated int, err error) {
	return processMediaReleaseCronAt(ctx, deps, time.Now())
}

func processMediaReleaseCronAt(ctx context.Context, deps Deps, now time.Time) (reminded, graduated int, err error) {
	users, err := listReleaseUsers(ctx, deps.DB)
	if err != nil {
		return 0, 0, err
	}

	var jobErrors []error
	for _, user := range users {
		localNow := now.In(timezoneLocation(user.timezone))
		moved, moveErr := graduateReleasedMedia(ctx, deps.DB, user.id, localNow)
		graduated += moved
		if moveErr != nil {
			jobErrors = append(jobErrors, fmt.Errorf("graduate media for user %s: %w", user.id, moveErr))
		}

		// Push alone is a complete channel now, so the sweep runs whenever
		// either sender exists — gating on Auth would mute push-only users.
		if (deps.Auth == nil && deps.Push == nil) || !scheduledNotificationWindow(localNow) {
			continue
		}
		sent, sendErr := sendDueMediaReleaseReminders(ctx, deps, user, localNow)
		reminded += sent
		if sendErr != nil {
			jobErrors = append(jobErrors, fmt.Errorf("release reminders for user %s: %w", user.id, sendErr))
		}
	}
	return reminded, graduated, errors.Join(jobErrors...)
}

func listReleaseUsers(ctx context.Context, d *db.DB) ([]releaseUser, error) {
	rows, err := d.QueryContext(ctx, `
		SELECT id, email, COALESCE(name,''), COALESCE(timezone,''), COALESCE(notify_channel,'both')
		  FROM users
		 WHERE deleted_at IS NULL
		 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("query release users: %w", err)
	}
	defer rows.Close()

	var users []releaseUser
	for rows.Next() {
		var user releaseUser
		if err := rows.Scan(&user.id, &user.email, &user.name, &user.timezone, &user.channel); err != nil {
			return nil, fmt.Errorf("scan release user: %w", err)
		}
		users = append(users, user)
	}
	return users, rows.Err()
}

// graduateReleasedMedia is also called by the library read path, so a movie
// cannot remain stale merely because a scheduled sweep was delayed. The
// update is atomic: only the worker that actually moves a row records the
// release event.
func graduateReleasedMedia(ctx context.Context, d *db.DB, uid string, localNow time.Time) (int, error) {
	today := localNow.Format("2006-01-02")
	var moved int
	err := d.QueryRowContext(ctx, `
		WITH released AS (
			UPDATE media
			   SET status = 'pending',
			       updated_at = NOW(),
			       release_reminder_claimed_until = NULL
			 WHERE user_id = $1
			   AND type = 'movie'
			   AND status = 'upcoming'
			   AND release_date IS NOT NULL
			   AND release_date <= $2::date
			RETURNING id, release_date
		), logged AS (
			INSERT INTO media_events (user_id, media_id, kind, meta)
			SELECT $1, id, 'released',
			       jsonb_build_object('release_date', release_date::text)
			  FROM released
			RETURNING 1
		)
		SELECT COUNT(*) FROM logged`,
		uid, today).Scan(&moved)
	if err != nil {
		return 0, err
	}
	return moved, nil
}

func sendDueMediaReleaseReminders(ctx context.Context, deps Deps, user releaseUser, localNow time.Time) (int, error) {
	tomorrow := mediaReleaseReminderDate(localNow)
	rows, err := deps.DB.QueryContext(ctx, `
		WITH due AS (
			SELECT id
			  FROM media
			 WHERE user_id = $1
			   AND type = 'movie'
			   AND status = 'upcoming'
			   AND release_date = $2::date
			   AND release_reminded_for IS DISTINCT FROM release_date
			   AND (release_reminder_claimed_until IS NULL OR release_reminder_claimed_until < NOW())
			 FOR UPDATE SKIP LOCKED
		)
		UPDATE media m
		   SET release_reminder_claimed_until = NOW() + make_interval(secs => $3)
		  FROM due
		 WHERE m.id = due.id
		RETURNING m.id, m.title, m.release_date::text`,
		user.id, tomorrow, int(mediaReleaseClaimLease.Seconds()))
	if err != nil {
		return 0, fmt.Errorf("claim release reminders: %w", err)
	}

	var claimed []claimedRelease
	for rows.Next() {
		var movie claimedRelease
		if err := rows.Scan(&movie.id, &movie.title, &movie.releaseDate); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan claimed release: %w", err)
		}
		claimed = append(claimed, movie)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	name := user.name
	if name == "" {
		name = user.email
	}
	sent := 0
	var jobErrors []error
	for _, movie := range claimed {
		releaseLabel := movie.releaseDate
		if releaseDate, err := time.Parse("2006-01-02", movie.releaseDate); err == nil {
			releaseLabel = releaseDate.Format("Monday, January 2, 2006")
		}
		pushed := notifyPush(ctx, deps, user.id, push.Notification{
			Type:  push.TypeMediaRelease,
			Title: "Out tomorrow",
			Body:  movie.title + " releases " + releaseLabel,
			Route: "/media?tab=movies",
		})

		// Email is skipped only for a push-only user whose push landed —
		// same rule as every other nudge (see channelWantsEmail).
		emailed := false
		var emailErr error
		if deps.Auth != nil && channelWantsEmail(user.channel, pushed) {
			emailErr = deps.Auth.SendMediaReleaseReminder(
				ctx,
				user.email,
				name,
				movie.title,
				releaseLabel,
				"/media?tab=movies",
			)
			emailed = emailErr == nil
			if emailErr != nil {
				log.Warn().Err(emailErr).Int64("media", movie.id).Str("user", user.id).Msg("movie release email failed")
			}
		}

		// Nothing landed on any channel — release the claim so the next tick
		// retries rather than stamping a reminder that never arrived.
		if !pushed && !emailed {
			_, _ = deps.DB.ExecContext(ctx,
				`UPDATE media SET release_reminder_claimed_until = NULL WHERE id = $1 AND user_id = $2`,
				movie.id, user.id)
			if emailErr != nil {
				jobErrors = append(jobErrors, fmt.Errorf("send media %d: %w", movie.id, emailErr))
			}
			continue
		}

		result, err := deps.DB.ExecContext(ctx, `
			UPDATE media
			   SET release_reminded_for = release_date,
			       release_reminder_claimed_until = NULL
			 WHERE id = $1
			   AND user_id = $2
			   AND status = 'upcoming'
			   AND release_date = $3::date
			   AND release_reminded_for IS DISTINCT FROM release_date`,
			movie.id, user.id, movie.releaseDate)
		if err != nil {
			jobErrors = append(jobErrors, fmt.Errorf("stamp media %d reminder: %w", movie.id, err))
			continue
		}
		n, _ := result.RowsAffected()
		if n == 1 {
			sent++
		}
	}
	return sent, errors.Join(jobErrors...)
}

func mediaReleaseReminderDate(localNow time.Time) string {
	return localNow.AddDate(0, 0, 1).Format("2006-01-02")
}
