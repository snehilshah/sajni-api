package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"sajni/internal/db"
)

const (
	mediaMetadataRefreshInterval = 7 * 24 * time.Hour
	mediaMetadataRefreshBatch    = 24
	mediaMetadataRefreshWorkers  = 4
)

type mediaRefreshCandidate struct {
	ID             int64
	Title          string
	ExternalID     string
	SeasonsTotal   int
	SeasonEpisodes []int
}

func (candidate mediaRefreshCandidate) seasonCount() int {
	if len(candidate.SeasonEpisodes) > 0 {
		return len(candidate.SeasonEpisodes)
	}
	return candidate.SeasonsTotal
}

func mediaDetailsSeasonCount(details MediaDetails) int {
	if len(details.SeasonEpisodes) > 0 {
		return len(details.SeasonEpisodes)
	}
	return details.SeasonsTotal
}

type mediaRefreshDecision int

const (
	mediaRefreshChecked mediaRefreshDecision = iota
	mediaRefreshBaseline
	mediaRefreshNewSeason
)

func decideMediaRefresh(candidate mediaRefreshCandidate, details MediaDetails) mediaRefreshDecision {
	oldSeasons := candidate.seasonCount()
	newSeasons := mediaDetailsSeasonCount(details)
	if oldSeasons <= 0 && newSeasons > 0 {
		return mediaRefreshBaseline
	}
	if newSeasons > oldSeasons {
		return mediaRefreshNewSeason
	}
	return mediaRefreshChecked
}

type mediaRefreshStore interface {
	staleCompletedShows(context.Context, string, time.Time, int) ([]mediaRefreshCandidate, error)
	applyMediaRefresh(context.Context, string, mediaRefreshCandidate, MediaDetails, time.Time) (bool, error)
}

type postgresMediaRefreshStore struct {
	db *db.DB
}

func (store postgresMediaRefreshStore) staleCompletedShows(
	ctx context.Context,
	uid string,
	cutoff time.Time,
	limit int,
) ([]mediaRefreshCandidate, error) {
	rows, err := store.db.QueryContext(ctx, `
		SELECT id, title, external_id, COALESCE(seasons_total, 0),
		       COALESCE(season_episodes::text, '[]')
		  FROM media
		 WHERE user_id = $1
		   AND type = 'show'
		   AND status = 'complete'
		   AND external_id LIKE 'tmdb:tv:%'
		   AND (metadata_checked_at IS NULL OR metadata_checked_at <= $2)
		 ORDER BY metadata_checked_at ASC NULLS FIRST, id ASC
		 LIMIT $3`, uid, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("query stale completed shows: %w", err)
	}
	defer rows.Close()

	candidates := make([]mediaRefreshCandidate, 0)
	for rows.Next() {
		var candidate mediaRefreshCandidate
		var seasonEpisodesRaw string
		if err := rows.Scan(
			&candidate.ID,
			&candidate.Title,
			&candidate.ExternalID,
			&candidate.SeasonsTotal,
			&seasonEpisodesRaw,
		); err != nil {
			return nil, fmt.Errorf("scan stale completed show: %w", err)
		}
		candidate.SeasonEpisodes = decodeIntArray(seasonEpisodesRaw)
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

func (store postgresMediaRefreshStore) applyMediaRefresh(
	ctx context.Context,
	uid string,
	candidate mediaRefreshCandidate,
	details MediaDetails,
	checkedAt time.Time,
) (bool, error) {
	decision := decideMediaRefresh(candidate, details)
	if decision == mediaRefreshChecked {
		_, err := store.db.ExecContext(ctx, `
			UPDATE media
			   SET metadata_checked_at = $1
			 WHERE id = $2 AND user_id = $3 AND status = 'complete'`,
			checkedAt, candidate.ID, uid)
		return false, err
	}

	seasonEpisodes, err := json.Marshal(details.SeasonEpisodes)
	if err != nil {
		return false, fmt.Errorf("encode season episodes: %w", err)
	}
	newSeasons := mediaDetailsSeasonCount(details)
	if decision == mediaRefreshBaseline {
		_, err := store.db.ExecContext(ctx, `
			UPDATE media
			   SET seasons_total = $1,
			       episodes_total = $2,
			       season_episodes = $3::jsonb,
			       metadata_checked_at = $4
			 WHERE id = $5 AND user_id = $6 AND status = 'complete'`,
			newSeasons, details.EpisodesTotal, string(seasonEpisodes), checkedAt, candidate.ID, uid)
		return false, err
	}

	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		UPDATE media
		   SET status = 'new_season',
		       seasons_total = $1,
		       episodes_total = $2,
		       season_episodes = $3::jsonb,
		       metadata_checked_at = $4,
		       updated_at = NOW()
		 WHERE id = $5 AND user_id = $6 AND status = 'complete'`,
		newSeasons, details.EpisodesTotal, string(seasonEpisodes), checkedAt, candidate.ID, uid)
	if err != nil {
		return false, err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if updated == 0 {
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return false, nil
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO media_events (user_id, media_id, kind, meta)
		VALUES ($1, $2, 'new_season',
		        jsonb_build_object('old_seasons', $3, 'new_seasons', $4))`,
		uid, candidate.ID, candidate.seasonCount(), newSeasons); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

type mediaRefreshResult struct {
	Checked int      `json:"checked"`
	Changed int      `json:"changed"`
	Titles  []string `json:"titles"`
}

type mediaDetailsFetcher func(context.Context, string, string) (MediaDetails, error)

func runMediaMetadataRefresh(
	ctx context.Context,
	store mediaRefreshStore,
	uid string,
	apiKey string,
	now time.Time,
	fetch mediaDetailsFetcher,
) (mediaRefreshResult, error) {
	candidates, err := store.staleCompletedShows(
		ctx,
		uid,
		now.Add(-mediaMetadataRefreshInterval),
		mediaMetadataRefreshBatch,
	)
	if err != nil {
		return mediaRefreshResult{}, err
	}
	if len(candidates) == 0 {
		return mediaRefreshResult{Titles: []string{}}, nil
	}

	type fetchedShow struct {
		candidate mediaRefreshCandidate
		details   MediaDetails
		err       error
	}
	jobs := make(chan mediaRefreshCandidate)
	fetched := make(chan fetchedShow, len(candidates))
	workerCount := min(mediaMetadataRefreshWorkers, len(candidates))
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for candidate := range jobs {
				details, err := fetch(ctx, candidate.ExternalID, apiKey)
				fetched <- fetchedShow{candidate: candidate, details: details, err: err}
			}
		}()
	}
	go func() {
		for _, candidate := range candidates {
			jobs <- candidate
		}
		close(jobs)
		workers.Wait()
		close(fetched)
	}()

	result := mediaRefreshResult{Titles: []string{}}
	for show := range fetched {
		if show.err != nil {
			log.Ctx(ctx).Warn().Err(show.err).
				Int64("media_id", show.candidate.ID).
				Msg("refresh TMDB show metadata")
			continue
		}
		changed, err := store.applyMediaRefresh(ctx, uid, show.candidate, show.details, now)
		if err != nil {
			log.Ctx(ctx).Warn().Err(err).
				Int64("media_id", show.candidate.ID).
				Msg("persist refreshed show metadata")
			continue
		}
		result.Checked++
		if changed {
			result.Changed++
			result.Titles = append(result.Titles, show.candidate.Title)
		}
	}
	sort.Strings(result.Titles)
	return result, nil
}

func refreshMediaMetadata(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Media.TMDBAPIKey == "" {
			errJSON(w, http.StatusServiceUnavailable, "tmdb not configured")
			return
		}
		result, err := runMediaMetadataRefresh(
			r.Context(),
			postgresMediaRefreshStore{db: deps.DB},
			userID(r.Context()),
			deps.Media.TMDBAPIKey,
			time.Now(),
			loadMediaDetails,
		)
		if err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}
