package api

import (
	"database/sql"
	"encoding/json"
	"time"

	"sajni/internal/db"
)

// progressMergeWindow: a progress change this soon after the show's last
// one folds into that entry, so tapping +1 four times (or jumping a backlog
// in steps) is one "S1E3 → S1E7" entry, not four.
const progressMergeWindow = 30 * time.Minute

// progressRun is the show's latest activity entry when it's a progress entry
// still inside the merge window: where that run of progress started.
type progressRun struct {
	id       int64
	fromEps  int
	fromSeas int
}

type progressAction int

const (
	progressNone progressAction = iota
	progressInsert
	progressUpdate
	progressDelete
)

// planProgress decides what a move from prev to now does to the activity:
// extend the open run (or drop it once stepped back to where it started),
// else start one on a forward move. Backward moves outside a run log nothing.
func planProgress(run *progressRun, prevEps, prevSeas, eps, seas int) (action progressAction, fromEps, fromSeas int) {
	if run != nil {
		if eps <= run.fromEps && seas <= run.fromSeas {
			return progressDelete, 0, 0
		}
		return progressUpdate, run.fromEps, run.fromSeas
	}
	if eps > prevEps || seas > prevSeas {
		return progressInsert, prevEps, prevSeas
	}
	return progressNone, 0, 0
}

// logMediaProgress records a progress change, merging into the open run.
// Best-effort, like logMediaEvent.
func logMediaProgress(d *db.DB, uid string, mediaID int64, prevEps, prevSeas, eps, seas, total int, seasonEps []int) {
	var (
		run      *progressRun
		id       int64
		kind     string
		fromEps  sql.NullInt64
		fromSeas sql.NullInt64
		recent   bool
	)
	err := d.QueryRow(`
		SELECT id, kind, (meta->>'from_episodes_watched')::int, (meta->>'from_seasons_watched')::int,
		       created_at > NOW() - $3 * INTERVAL '1 second'
		  FROM media_events
		 WHERE user_id = $1 AND media_id = $2
		 ORDER BY created_at DESC, id DESC
		 LIMIT 1`, uid, mediaID, int(progressMergeWindow.Seconds())).
		Scan(&id, &kind, &fromEps, &fromSeas, &recent)
	// Entries from before runs were recorded carry no start; they stay as they are.
	if err == nil && kind == "progress" && recent && fromEps.Valid && fromSeas.Valid {
		run = &progressRun{id: id, fromEps: int(fromEps.Int64), fromSeas: int(fromSeas.Int64)}
	}

	action, startEps, startSeas := planProgress(run, prevEps, prevSeas, eps, seas)
	if action == progressNone {
		return
	}
	if action == progressDelete {
		_, _ = d.Exec(`DELETE FROM media_events WHERE id = $1 AND user_id = $2`, run.id, uid)
		return
	}

	meta := progressMeta(seas, eps, total, seasonEps)
	meta["from_episodes_watched"] = startEps
	meta["from_seasons_watched"] = startSeas
	start := progressMeta(startSeas, startEps, total, seasonEps)
	if s, ok := start["season"]; ok {
		meta["from_season"] = s
		meta["from_episode"] = start["episode"]
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return
	}
	if action == progressUpdate {
		_, _ = d.Exec(`UPDATE media_events SET meta = $1::jsonb, created_at = NOW() WHERE id = $2 AND user_id = $3`,
			string(raw), run.id, uid)
		return
	}
	_, _ = d.Exec(`INSERT INTO media_events (user_id, media_id, kind, meta) VALUES ($1, $2, 'progress', $3::jsonb)`,
		uid, mediaID, string(raw))
}
