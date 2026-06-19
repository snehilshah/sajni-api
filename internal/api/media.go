package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"sajni/internal/db"
)

// httpClient bounds every outbound TMDB / Open Library call. The stdlib
// default has NO timeout, so a slow upstream (or a Cloud Run cold start)
// left search requests hanging — that read as the whole Add dialog
// freezing until the response landed.
var httpClient = &http.Client{Timeout: 8 * time.Second}

// Tiny in-memory TTL cache for the public metadata calls (search /
// details / collection). Results are public + immutable enough that a
// short TTL turns repeat lookups instant without any store or migration.
type cacheEntry struct {
	val any
	exp time.Time
}

var (
	tmdbCache   = map[string]cacheEntry{}
	tmdbCacheMu sync.Mutex
)

func cacheGet(key string) (any, bool) {
	tmdbCacheMu.Lock()
	defer tmdbCacheMu.Unlock()
	e, ok := tmdbCache[key]
	if !ok || time.Now().After(e.exp) {
		if ok {
			delete(tmdbCache, key)
		}
		return nil, false
	}
	return e.val, true
}

func cacheSet(key string, val any, ttl time.Duration) {
	tmdbCacheMu.Lock()
	defer tmdbCacheMu.Unlock()
	tmdbCache[key] = cacheEntry{val: val, exp: time.Now().Add(ttl)}
}

func registerMediaRoutes(mux *http.ServeMux, deps Deps) {
	// More-specific routes register before /{id}.
	mux.HandleFunc("GET /api/media/search", searchMedia())
	mux.HandleFunc("GET /api/media/details", mediaDetails())
	mux.HandleFunc("GET /api/media/collection", collectionDetails())
	mux.HandleFunc("GET /api/media/{id}/events", listMediaEvents(deps))
	mux.HandleFunc("GET /api/media", listMedia(deps))
	mux.HandleFunc("POST /api/media", createMedia(deps))
	mux.HandleFunc("PUT /api/media/{id}", updateMedia(deps))
	mux.HandleFunc("DELETE /api/media/{id}", deleteMedia(deps))
}

// MediaEvent is one entry in the watch-history timeline. Auto-written
// from create/update; never accepted from the client directly so we
// always know the meta is canonical.
type MediaEvent struct {
	ID        int64           `json:"id"`
	MediaID   int64           `json:"media_id"`
	Kind      string          `json:"kind"`
	Meta      json.RawMessage `json:"meta"`
	CreatedAt string          `json:"created_at"`
}

// logMediaEvent writes one event row. Errors are best-effort — a
// failed audit insert shouldn't break the user's actual add/update
// action. Callers can pass meta=nil for events with no detail.
func logMediaEvent(d *db.DB, uid string, mediaID int64, kind string, meta map[string]any) {
	raw := []byte("{}")
	if meta != nil {
		if b, err := json.Marshal(meta); err == nil {
			raw = b
		}
	}
	_, _ = d.Exec(
		`INSERT INTO media_events (user_id, media_id, kind, meta) VALUES ($1, $2, $3, $4::jsonb)`,
		uid, mediaID, kind, string(raw),
	)
}

// progressMeta packs the snapshot we store on `progress`/`completed`
// events: which season+episode the user was on plus the cumulative
// counts. Used by the UI to render "S2E6 · 17/100" style timeline rows.
func progressMeta(seasonsWatched, episodesWatched, episodesTotal int, seasonEpisodes []int) map[string]any {
	meta := map[string]any{
		"episodes_watched": episodesWatched,
		"episodes_total":   episodesTotal,
		"seasons_watched":  seasonsWatched,
	}
	if len(seasonEpisodes) > 0 {
		// Derive S?E? from the cumulative count.
		acc := 0
		for i := 0; i < len(seasonEpisodes); i++ {
			if acc+seasonEpisodes[i] >= episodesWatched || i == len(seasonEpisodes)-1 {
				meta["season"] = i + 1
				meta["episode"] = episodesWatched - acc
				break
			}
			acc += seasonEpisodes[i]
		}
	} else if seasonsWatched > 0 {
		meta["season"] = seasonsWatched
		meta["episode"] = episodesWatched
	}
	return meta
}

// listMediaEvents returns the watch history for one media row, scoped
// to the calling user. Newest events first.
func listMediaEvents(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		// Confirm ownership before exposing any rows.
		var ok int
		d.QueryRow("SELECT 1 FROM media WHERE id=$1 AND user_id=$2", id, uid).Scan(&ok)
		if ok != 1 {
			errJSON(w, 404, "not found")
			return
		}
		rows, err := d.Query(
			`SELECT id, media_id, kind, COALESCE(meta::text, '{}'), created_at
			   FROM media_events
			  WHERE user_id = $1 AND media_id = $2
			  ORDER BY created_at DESC, id DESC`,
			uid, id,
		)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()
		out := []MediaEvent{}
		for rows.Next() {
			var e MediaEvent
			var metaRaw string
			if err := rows.Scan(&e.ID, &e.MediaID, &e.Kind, &metaRaw, &e.CreatedAt); err != nil {
				continue
			}
			e.Meta = json.RawMessage(metaRaw)
			out = append(out, e)
		}
		writeJSON(w, 200, out)
	}
}

type Media struct {
	ID              int64  `json:"id"`
	Title           string `json:"title"`
	Type            string `json:"type"`
	Status          string `json:"status"`
	Rating          *int   `json:"rating"`
	Notes           string `json:"notes"`
	Platform        string `json:"platform"`
	PosterURL       string `json:"poster_url"`
	Year            *int   `json:"year"`
	Genre           string `json:"genre"`
	ExternalID      string `json:"external_id"`
	EpisodesWatched int    `json:"episodes_watched"`
	EpisodesTotal   int    `json:"episodes_total"`
	SeasonsWatched  int    `json:"seasons_watched"`
	SeasonsTotal    int    `json:"seasons_total"`
	SeasonEpisodes  []int  `json:"season_episodes"`
	CollectionID    string `json:"collection_id"`
	CollectionName  string `json:"collection_name"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
	// Last time we logged a 'completed' event for this row. Empty
	// string when the user hasn't finished it yet (or they marked it
	// complete before we shipped the event log).
	LastCompletedAt string `json:"last_completed_at"`
}

func listMedia(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())

		args := []any{uid}
		clauses := []string{"user_id = $1"}
		ph := 2

		if t := queryParam(r, "type"); t != "" {
			clauses = append(clauses, "type = $"+itoa(ph))
			args = append(args, t)
			ph++
		}
		if s := queryParam(r, "status"); s != "" {
			clauses = append(clauses, "status = $"+itoa(ph))
			args = append(args, s)
			ph++
		}

		// Optional collection filter so the frontend can show
		// "all my Mission Impossible movies" with one request.
		if c := queryParam(r, "collection_id"); c != "" {
			clauses = append(clauses, "collection_id = $"+itoa(ph))
			args = append(args, c)
			ph++
		}

		q := `SELECT m.id, m.title, m.type, m.status, m.rating, m.notes, m.platform, m.poster_url,
		       m.year, m.genre, m.external_id, m.episodes_watched, m.episodes_total,
		       m.seasons_watched, m.seasons_total,
		       COALESCE(m.season_episodes::text, '[]'),
		       m.collection_id, m.collection_name,
		       m.created_at, m.updated_at,
		       COALESCE(
		         (SELECT MAX(created_at)::text FROM media_events
		            WHERE media_id = m.id AND user_id = m.user_id AND kind = 'completed'),
		         ''
		       ) AS last_completed_at
		      FROM media m`
		// Rewrite user_id reference to the alias; clauses are built with
		// the bare column name so prefix them.
		for i, c := range clauses {
			clauses[i] = strings.ReplaceAll(c, "user_id", "m.user_id")
			clauses[i] = strings.ReplaceAll(clauses[i], "m.m.user_id", "m.user_id")
			_ = c
		}
		q += " WHERE " + strings.Join(clauses, " AND ")
		q += " ORDER BY m.updated_at DESC"

		rows, err := d.Query(q, args...)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()

		var items []Media
		for rows.Next() {
			var m Media
			var seRaw string
			if err := rows.Scan(&m.ID, &m.Title, &m.Type, &m.Status, &m.Rating, &m.Notes,
				&m.Platform, &m.PosterURL, &m.Year, &m.Genre, &m.ExternalID,
				&m.EpisodesWatched, &m.EpisodesTotal,
				&m.SeasonsWatched, &m.SeasonsTotal,
				&seRaw, &m.CollectionID, &m.CollectionName,
				&m.CreatedAt, &m.UpdatedAt, &m.LastCompletedAt); err != nil {
				errJSON(w, 500, err.Error())
				return
			}
			m.SeasonEpisodes = decodeIntArray(seRaw)
			items = append(items, m)
		}
		if items == nil {
			items = []Media{}
		}
		writeJSON(w, 200, items)
	}
}

func decodeIntArray(raw string) []int {
	if raw == "" || raw == "null" {
		return []int{}
	}
	var out []int
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return []int{}
	}
	return out
}

func createMedia(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var body struct {
			Title           string `json:"title"`
			Type            string `json:"type"`
			Status          string `json:"status"`
			Rating          *int   `json:"rating"`
			Notes           string `json:"notes"`
			Platform        string `json:"platform"`
			PosterURL       string `json:"poster_url"`
			Year            *int   `json:"year"`
			Genre           string `json:"genre"`
			ExternalID      string `json:"external_id"`
			EpisodesWatched int    `json:"episodes_watched"`
			EpisodesTotal   int    `json:"episodes_total"`
			SeasonsWatched  int    `json:"seasons_watched"`
			SeasonsTotal    int    `json:"seasons_total"`
			SeasonEpisodes  []int  `json:"season_episodes"`
			CollectionID    string `json:"collection_id"`
			CollectionName  string `json:"collection_name"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if body.Type == "" {
			body.Type = "movie"
		}
		if body.Status == "" {
			body.Status = "pending"
		}
		seJSON, _ := json.Marshal(body.SeasonEpisodes)
		if len(body.SeasonEpisodes) == 0 {
			seJSON = []byte("[]")
		}
		var id int64
		err := d.QueryRow(
			`INSERT INTO media (user_id, title, type, status, rating, notes, platform, poster_url,
			 year, genre, external_id, episodes_watched, episodes_total, seasons_watched, seasons_total,
			 season_episodes, collection_id, collection_name)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16::jsonb, $17, $18) RETURNING id`,
			uid, body.Title, body.Type, body.Status, body.Rating, body.Notes,
			body.Platform, body.PosterURL, body.Year, body.Genre, body.ExternalID,
			body.EpisodesWatched, body.EpisodesTotal, body.SeasonsWatched, body.SeasonsTotal,
			string(seJSON), body.CollectionID, body.CollectionName,
		).Scan(&id)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}

		// Watch-history: log the add and any "started" / "completed"
		// state implied by the initial status.
		logMediaEvent(d, uid, id, "added", map[string]any{
			"title":  body.Title,
			"type":   body.Type,
			"status": body.Status,
		})
		switch body.Status {
		case "in_progress":
			logMediaEvent(d, uid, id, "started",
				progressMeta(body.SeasonsWatched, body.EpisodesWatched, body.EpisodesTotal, body.SeasonEpisodes))
		case "complete":
			logMediaEvent(d, uid, id, "completed",
				progressMeta(body.SeasonsWatched, body.EpisodesWatched, body.EpisodesTotal, body.SeasonEpisodes))
		}

		writeJSON(w, 201, map[string]int64{"id": id})
	}
}

func updateMedia(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}

		var body map[string]any
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}

		// Snapshot the row before update so we can detect status flips
		// and progress jumps for the event log.
		var (
			prevStatus     string
			prevEpsWatched int
			prevSeasWatch  int
			prevRating     sql.NullInt64
			prevType       string
		)
		var prevSEraw string
		var prevEpsTotal int
		_ = d.QueryRow(`SELECT status, COALESCE(episodes_watched,0), COALESCE(seasons_watched,0),
		                       rating, type, COALESCE(season_episodes::text,'[]'),
		                       COALESCE(episodes_total,0)
		                  FROM media WHERE id=$1 AND user_id=$2`, id, uid).
			Scan(&prevStatus, &prevEpsWatched, &prevSeasWatch, &prevRating, &prevType, &prevSEraw, &prevEpsTotal)
		prevSeasonEps := decodeIntArray(prevSEraw)

		allowed := map[string]bool{
			"title": true, "type": true, "status": true, "rating": true,
			"notes": true, "platform": true, "poster_url": true, "year": true,
			"genre": true, "external_id": true,
			"episodes_watched": true, "episodes_total": true,
			"seasons_watched": true, "seasons_total": true,
			"collection_id": true, "collection_name": true,
		}

		var sets []string
		var args []any
		ph := 1
		for k, v := range body {
			if allowed[k] {
				sets = append(sets, fmt.Sprintf("%s = $%d", k, ph))
				args = append(args, v)
				ph++
			}
		}
		// season_episodes goes through JSONB casting separately.
		if v, ok := body["season_episodes"]; ok {
			arr, _ := json.Marshal(v)
			if len(arr) == 0 {
				arr = []byte("[]")
			}
			sets = append(sets, fmt.Sprintf("season_episodes = $%d::jsonb", ph))
			args = append(args, string(arr))
			ph++
		}
		if len(sets) == 0 {
			writeJSON(w, 200, map[string]string{"status": "ok"})
			return
		}
		sets = append(sets, "updated_at = NOW()")
		idPh := ph
		uidPh := ph + 1
		args = append(args, id, uid)
		q := fmt.Sprintf("UPDATE media SET %s WHERE id = $%d AND user_id = $%d", strings.Join(sets, ", "), idPh, uidPh)
		if _, err := d.Exec(q, args...); err != nil {
			errJSON(w, 500, err.Error())
			return
		}

		// Watch-history: emit events for any user-visible state change.
		// Read the post-update row once and diff against the snapshot.
		var (
			newStatus     string
			newEpsWatched int
			newSeasWatch  int
			newRating     sql.NullInt64
			newSEraw      string
			newEpsTotal   int
		)
		_ = d.QueryRow(`SELECT status, COALESCE(episodes_watched,0), COALESCE(seasons_watched,0),
		                       rating, COALESCE(season_episodes::text,'[]'),
		                       COALESCE(episodes_total,0)
		                  FROM media WHERE id=$1 AND user_id=$2`, id, uid).
			Scan(&newStatus, &newEpsWatched, &newSeasWatch, &newRating, &newSEraw, &newEpsTotal)
		seasonEps := decodeIntArray(newSEraw)
		if len(seasonEps) == 0 {
			// Fall back to the prior snapshot so the meta still has
			// season counts when the update didn't touch them.
			seasonEps = prevSeasonEps
		}

		if newStatus != prevStatus {
			switch newStatus {
			case "in_progress":
				if prevStatus != "in_progress" {
					logMediaEvent(d, uid, id, "started",
						progressMeta(newSeasWatch, newEpsWatched, newEpsTotal, seasonEps))
				}
			case "complete":
				logMediaEvent(d, uid, id, "completed",
					progressMeta(newSeasWatch, newEpsWatched, newEpsTotal, seasonEps))
			case "dropped":
				logMediaEvent(d, uid, id, "dropped",
					progressMeta(newSeasWatch, newEpsWatched, newEpsTotal, seasonEps))
			}
		}

		// Episode/season advance — only log forward jumps (not edits
		// that walk back). Skip when the only change was status, to
		// avoid logging "started" + "progress" for the same row.
		if newStatus == prevStatus && (newEpsWatched > prevEpsWatched || newSeasWatch > prevSeasWatch) {
			logMediaEvent(d, uid, id, "progress",
				progressMeta(newSeasWatch, newEpsWatched, newEpsTotal, seasonEps))
		}

		// Rating set / changed.
		if newRating.Valid && (!prevRating.Valid || newRating.Int64 != prevRating.Int64) {
			logMediaEvent(d, uid, id, "rating", map[string]any{"rating": newRating.Int64})
		}

		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func deleteMedia(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		d.Exec("DELETE FROM media WHERE id = $1 AND user_id = $2", id, uid)
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

type SearchResult struct {
	ExternalID string `json:"external_id"`
	Title      string `json:"title"`
	Year       string `json:"year"`
	PosterURL  string `json:"poster_url"`
	Overview   string `json:"overview"`
	Genre      string `json:"genre"`
}

// searchMedia proxies searches to TMDB or Open Library.
// Doesn't need user scoping since results are public metadata.
func searchMedia() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := queryParam(r, "q")
		mediaType := queryParam(r, "type")
		if query == "" {
			writeJSON(w, 200, []SearchResult{})
			return
		}

		switch mediaType {
		case "book":
			results := searchOpenLibrary(query)
			writeJSON(w, 200, results)
		default:
			results := searchTMDB(query, mediaType)
			writeJSON(w, 200, results)
		}
	}
}

// searchTMDB returns combined movie + show results for the movie/show
// tabs via TMDB /search/multi, so a title of either kind surfaces from
// either tab. The kind is carried in each external_id (tmdb:movie:… /
// tmdb:tv:…) — the frontend derives its Movie/Show badge + form type
// from that, so no response field changes. People results are dropped.
func searchTMDB(query, mediaType string) []SearchResult {
	apiKey := os.Getenv("TMDB_API_KEY")
	if apiKey == "" {
		return []SearchResult{}
	}
	cacheKey := "search:" + mediaType + ":" + strings.ToLower(query)
	if v, ok := cacheGet(cacheKey); ok {
		return v.([]SearchResult)
	}

	// movie / show (and the empty default) share one combined search;
	// any other explicit type still hits its single endpoint.
	combined := mediaType == "movie" || mediaType == "show" || mediaType == ""
	var u string
	if combined {
		u = fmt.Sprintf("https://api.themoviedb.org/3/search/multi?api_key=%s&query=%s&page=1",
			apiKey, url.QueryEscape(query))
	} else {
		endpoint := "movie"
		if mediaType == "show" {
			endpoint = "tv"
		}
		u = fmt.Sprintf("https://api.themoviedb.org/3/search/%s?api_key=%s&query=%s&page=1",
			endpoint, apiKey, url.QueryEscape(query))
	}
	resp, err := httpClient.Get(u)
	if err != nil {
		return []SearchResult{}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var raw struct {
		Results []json.RawMessage `json:"results"`
	}
	json.Unmarshal(body, &raw)

	results := []SearchResult{}
	for _, rr := range raw.Results {
		var item map[string]any
		json.Unmarshal(rr, &item)

		// /search/multi tags each row with media_type; a single-endpoint
		// search doesn't, so infer the kind from the requested type.
		kind, _ := item["media_type"].(string)
		if kind == "" {
			if mediaType == "show" {
				kind = "tv"
			} else {
				kind = "movie"
			}
		}
		if kind != "movie" && kind != "tv" {
			continue // drop people / anything else multi returns
		}

		sr := SearchResult{}
		sr.ExternalID = fmt.Sprintf("tmdb:%s:%.0f", kind, item["id"])
		if kind == "movie" {
			if v, ok := item["title"].(string); ok {
				sr.Title = v
			}
			if v, ok := item["release_date"].(string); ok && len(v) >= 4 {
				sr.Year = v[:4]
			}
		} else {
			if v, ok := item["name"].(string); ok {
				sr.Title = v
			}
			if v, ok := item["first_air_date"].(string); ok && len(v) >= 4 {
				sr.Year = v[:4]
			}
		}
		if v, ok := item["poster_path"].(string); ok && v != "" {
			sr.PosterURL = "https://image.tmdb.org/t/p/w300" + v
		}
		if v, ok := item["overview"].(string); ok {
			sr.Overview = v
		}
		if sr.Title == "" {
			continue
		}
		results = append(results, sr)
	}
	if len(results) > 8 {
		results = results[:8]
	}
	cacheSet(cacheKey, results, 10*time.Minute)
	return results
}

func searchOpenLibrary(query string) []SearchResult {
	cacheKey := "ol:" + strings.ToLower(query)
	if v, ok := cacheGet(cacheKey); ok {
		return v.([]SearchResult)
	}
	u := fmt.Sprintf("https://openlibrary.org/search.json?q=%s&limit=10", url.QueryEscape(query))
	resp, err := httpClient.Get(u)
	if err != nil {
		return []SearchResult{}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var raw struct {
		Docs []map[string]any `json:"docs"`
	}
	json.Unmarshal(body, &raw)

	results := []SearchResult{}
	for _, item := range raw.Docs {
		sr := SearchResult{}
		if v, ok := item["title"].(string); ok {
			sr.Title = v
		}
		if v, ok := item["first_publish_year"].(float64); ok {
			sr.Year = fmt.Sprintf("%.0f", v)
		}
		if v, ok := item["cover_i"].(float64); ok && v > 0 {
			sr.PosterURL = fmt.Sprintf("https://covers.openlibrary.org/b/id/%.0f-M.jpg", v)
		}
		if v, ok := item["key"].(string); ok {
			sr.ExternalID = "openlibrary:" + v
		}
		if authors, ok := item["author_name"].([]any); ok && len(authors) > 0 {
			if a, ok := authors[0].(string); ok {
				sr.Overview = "by " + a
			}
		}
		if subjects, ok := item["subject"].([]any); ok && len(subjects) > 0 {
			if s, ok := subjects[0].(string); ok {
				sr.Genre = s
			}
		}
		if sr.Title != "" {
			results = append(results, sr)
		}
	}
	if len(results) > 8 {
		results = results[:8]
	}
	cacheSet(cacheKey, results, 10*time.Minute)
	return results
}

// parseTMDBExternalID splits "tmdb:{kind}:{id}" — the format we mint in
// searchTMDB — back into (kind, id). Returns ok=false for any other shape.
func parseTMDBExternalID(s string) (kind, id string, ok bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 || parts[0] != "tmdb" {
		return "", "", false
	}
	if parts[1] != "movie" && parts[1] != "tv" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// MediaDetails is the response shape of GET /api/media/details. The
// frontend uses it to populate the show progress UI (per-season episode
// counts) and the movie-collection badge. Fields not relevant to a
// given kind are zero-valued.
type MediaDetails struct {
	ExternalID     string             `json:"external_id"`
	Type           string             `json:"type"` // "show" | "movie"
	Title          string             `json:"title"`
	Year           string             `json:"year"`
	PosterURL      string             `json:"poster_url"`
	Genre          string             `json:"genre"`
	Overview       string             `json:"overview"`
	SeasonsTotal   int                `json:"seasons_total"`
	EpisodesTotal  int                `json:"episodes_total"`
	SeasonEpisodes []int              `json:"season_episodes"`
	CollectionID   string             `json:"collection_id"`
	CollectionName string             `json:"collection_name"`
	Collection     *CollectionPayload `json:"collection,omitempty"`
}

type CollectionPayload struct {
	ID    string           `json:"id"`
	Name  string           `json:"name"`
	Parts []CollectionPart `json:"parts"`
}

type CollectionPart struct {
	ExternalID string `json:"external_id"`
	Title      string `json:"title"`
	Year       string `json:"year"`
	PosterURL  string `json:"poster_url"`
	Overview   string `json:"overview"`
}

// mediaDetails fetches a single TMDB title's full record so we can fill
// per-season episode counts (for shows) or collection info (for movies)
// without making the user enter them by hand.
func mediaDetails() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ext := queryParam(r, "external_id")
		if ext == "" {
			errJSON(w, 400, "missing external_id")
			return
		}
		kind, id, ok := parseTMDBExternalID(ext)
		if !ok {
			errJSON(w, 400, "external_id must be tmdb:{movie|tv}:{id}")
			return
		}
		apiKey := os.Getenv("TMDB_API_KEY")
		if apiKey == "" {
			errJSON(w, 503, "tmdb not configured")
			return
		}
		cacheKey := "details:" + ext
		if v, ok := cacheGet(cacheKey); ok {
			writeJSON(w, 200, v.(MediaDetails))
			return
		}
		out := MediaDetails{ExternalID: ext}
		if kind == "tv" {
			out.Type = "show"
			if err := fillShowDetails(&out, id, apiKey); err != nil {
				errJSON(w, 502, err.Error())
				return
			}
		} else {
			out.Type = "movie"
			if err := fillMovieDetails(&out, id, apiKey); err != nil {
				errJSON(w, 502, err.Error())
				return
			}
		}
		cacheSet(cacheKey, out, 30*time.Minute)
		writeJSON(w, 200, out)
	}
}

func fillShowDetails(out *MediaDetails, id, apiKey string) error {
	u := fmt.Sprintf("https://api.themoviedb.org/3/tv/%s?api_key=%s", id, apiKey)
	resp, err := httpClient.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var d struct {
		Name             string `json:"name"`
		FirstAirDate     string `json:"first_air_date"`
		Overview         string `json:"overview"`
		PosterPath       string `json:"poster_path"`
		NumberOfSeasons  int    `json:"number_of_seasons"`
		NumberOfEpisodes int    `json:"number_of_episodes"`
		Genres           []struct {
			Name string `json:"name"`
		} `json:"genres"`
		Seasons []struct {
			SeasonNumber int    `json:"season_number"`
			EpisodeCount int    `json:"episode_count"`
			Name         string `json:"name"`
			AirDate      string `json:"air_date"`
		} `json:"seasons"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return err
	}
	out.Title = d.Name
	if len(d.FirstAirDate) >= 4 {
		out.Year = d.FirstAirDate[:4]
	}
	if d.PosterPath != "" {
		out.PosterURL = "https://image.tmdb.org/t/p/w500" + d.PosterPath
	}
	out.Overview = d.Overview
	out.SeasonsTotal = d.NumberOfSeasons
	out.EpisodesTotal = d.NumberOfEpisodes
	if len(d.Genres) > 0 {
		names := make([]string, 0, len(d.Genres))
		for _, g := range d.Genres {
			names = append(names, g.Name)
		}
		out.Genre = strings.Join(names, ", ")
	}
	// Build season_episodes ordered by season_number, skipping season 0
	// (TMDB reserves it for "specials" — most shows have one and it
	// throws off the count if we include it).
	maxSeason := 0
	for _, s := range d.Seasons {
		if s.SeasonNumber > maxSeason {
			maxSeason = s.SeasonNumber
		}
	}
	if maxSeason > 0 {
		out.SeasonEpisodes = make([]int, maxSeason)
		for _, s := range d.Seasons {
			if s.SeasonNumber >= 1 && s.SeasonNumber <= maxSeason {
				out.SeasonEpisodes[s.SeasonNumber-1] = s.EpisodeCount
			}
		}
		// Recompute totals from per-season counts so they always sum up.
		sum := 0
		for _, n := range out.SeasonEpisodes {
			sum += n
		}
		out.EpisodesTotal = sum
		if out.SeasonsTotal == 0 {
			out.SeasonsTotal = maxSeason
		}
	}
	return nil
}

func fillMovieDetails(out *MediaDetails, id, apiKey string) error {
	u := fmt.Sprintf("https://api.themoviedb.org/3/movie/%s?api_key=%s", id, apiKey)
	resp, err := httpClient.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var d struct {
		Title       string `json:"title"`
		ReleaseDate string `json:"release_date"`
		Overview    string `json:"overview"`
		PosterPath  string `json:"poster_path"`
		Genres      []struct {
			Name string `json:"name"`
		} `json:"genres"`
		BelongsToCollection *struct {
			ID         int    `json:"id"`
			Name       string `json:"name"`
			PosterPath string `json:"poster_path"`
		} `json:"belongs_to_collection"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return err
	}
	out.Title = d.Title
	if len(d.ReleaseDate) >= 4 {
		out.Year = d.ReleaseDate[:4]
	}
	if d.PosterPath != "" {
		out.PosterURL = "https://image.tmdb.org/t/p/w500" + d.PosterPath
	}
	out.Overview = d.Overview
	if len(d.Genres) > 0 {
		names := make([]string, 0, len(d.Genres))
		for _, g := range d.Genres {
			names = append(names, g.Name)
		}
		out.Genre = strings.Join(names, ", ")
	}
	if d.BelongsToCollection != nil && d.BelongsToCollection.ID > 0 {
		out.CollectionID = fmt.Sprintf("tmdb:collection:%d", d.BelongsToCollection.ID)
		out.CollectionName = d.BelongsToCollection.Name
	}
	return nil
}

// collectionDetails proxies TMDB /collection/{id} so the frontend can
// list every part of a movie series the user is in. Only returns the
// list — matching against the user's library happens client-side.
func collectionDetails() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cid := queryParam(r, "id")
		if cid == "" {
			errJSON(w, 400, "missing id")
			return
		}
		// Accept either "tmdb:collection:1234" or bare "1234".
		raw := strings.TrimPrefix(cid, "tmdb:collection:")
		apiKey := os.Getenv("TMDB_API_KEY")
		if apiKey == "" {
			errJSON(w, 503, "tmdb not configured")
			return
		}
		cacheKey := "collection:" + raw
		if v, ok := cacheGet(cacheKey); ok {
			writeJSON(w, 200, v.(CollectionPayload))
			return
		}
		u := fmt.Sprintf("https://api.themoviedb.org/3/collection/%s?api_key=%s", raw, apiKey)
		resp, err := httpClient.Get(u)
		if err != nil {
			errJSON(w, 502, err.Error())
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var d struct {
			ID    int    `json:"id"`
			Name  string `json:"name"`
			Parts []struct {
				ID          int    `json:"id"`
				Title       string `json:"title"`
				ReleaseDate string `json:"release_date"`
				PosterPath  string `json:"poster_path"`
				Overview    string `json:"overview"`
			} `json:"parts"`
		}
		if err := json.Unmarshal(body, &d); err != nil {
			errJSON(w, 502, "bad tmdb response")
			return
		}
		out := CollectionPayload{
			ID:   fmt.Sprintf("tmdb:collection:%d", d.ID),
			Name: d.Name,
		}
		for _, p := range d.Parts {
			cp := CollectionPart{
				ExternalID: fmt.Sprintf("tmdb:movie:%d", p.ID),
				Title:      p.Title,
				Overview:   p.Overview,
			}
			if len(p.ReleaseDate) >= 4 {
				cp.Year = p.ReleaseDate[:4]
			}
			if p.PosterPath != "" {
				cp.PosterURL = "https://image.tmdb.org/t/p/w300" + p.PosterPath
			}
			out.Parts = append(out.Parts, cp)
		}
		// Sort parts chronologically — release order is more useful than
		// TMDB's id order.
		sortCollectionParts(out.Parts)
		cacheSet(cacheKey, out, 30*time.Minute)
		writeJSON(w, 200, out)
	}
}

func sortCollectionParts(parts []CollectionPart) {
	for i := 1; i < len(parts); i++ {
		for j := i; j > 0 && partLess(parts[j], parts[j-1]); j-- {
			parts[j], parts[j-1] = parts[j-1], parts[j]
		}
	}
}

func partLess(a, b CollectionPart) bool {
	if a.Year != b.Year {
		if a.Year == "" {
			return false
		}
		if b.Year == "" {
			return true
		}
		return a.Year < b.Year
	}
	return a.Title < b.Title
}
