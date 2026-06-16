package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"sajni/internal/db"
	"sajni/internal/storage"
)

func registerJournalRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("GET /api/journal", listJournal(deps))
	// Weekly routes register BEFORE the {date} catch so that literals
	// like "weeks" and "week" win path-pattern selection unambiguously.
	mux.HandleFunc("GET /api/journal/weeks", listWeeklyEntries(deps))
	mux.HandleFunc("GET /api/journal/week/{year}/{week}", getWeeklyEntry(deps))
	mux.HandleFunc("PUT /api/journal/week/{year}/{week}", upsertWeeklyEntry(deps))
	mux.HandleFunc("DELETE /api/journal/week/{year}/{week}", deleteWeeklyEntry(deps))
	mux.HandleFunc("GET /api/journal/week/{year}/{week}/summary", weeklySummary(deps))

	// Monthly routes — same literal-before-{date} ordering rule.
	mux.HandleFunc("GET /api/journal/months", listMonthlyEntries(deps))
	mux.HandleFunc("GET /api/journal/month/{year}/{month}", getMonthlyEntry(deps))
	mux.HandleFunc("PUT /api/journal/month/{year}/{month}", upsertMonthlyEntry(deps))
	mux.HandleFunc("DELETE /api/journal/month/{year}/{month}", deleteMonthlyEntry(deps))
	mux.HandleFunc("GET /api/journal/month/{year}/{month}/summary", monthlySummary(deps))
	mux.HandleFunc("GET /api/journal/{date}", getJournalEntry(deps))
	mux.HandleFunc("PUT /api/journal/{date}", upsertJournalEntry(deps))
	mux.HandleFunc("DELETE /api/journal/{date}", deleteJournalEntry(deps))

	// Places lookup — proxies Google Places (New) so the API key never
	// reaches the browser. Both endpoints expect a `session` token so the
	// autocomplete + details pair is billed as a single session.
	mux.HandleFunc("GET /api/places/autocomplete", placesAutocomplete(deps))
	mux.HandleFunc("GET /api/places/details", placesDetails(deps))
}

func journalKey(uid string, date string) string {
	return storage.UserKey(uid, "journal", date+".md")
}

func listJournal(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		rows, err := d.Query("SELECT id, date::text, mood, created_at, updated_at FROM journal_entries WHERE user_id = $1 ORDER BY date DESC", uid)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()

		type Entry struct {
			ID        int64    `json:"id"`
			Date      string   `json:"date"`
			Mood      *string  `json:"mood"`
			Tags      []string `json:"tags"`
			CreatedAt string   `json:"created_at"`
			UpdatedAt string   `json:"updated_at"`
		}
		var entries []Entry
		for rows.Next() {
			var e Entry
			if err := rows.Scan(&e.ID, &e.Date, &e.Mood, &e.CreatedAt, &e.UpdatedAt); err != nil {
				errJSON(w, 500, err.Error())
				return
			}
			tagRows, err := d.Query("SELECT tag FROM tags WHERE user_id = $1 AND entity_type = 'journal' AND entity_id = $2", uid, e.ID)
			if err == nil {
				for tagRows.Next() {
					var t string
					tagRows.Scan(&t)
					e.Tags = append(e.Tags, t)
				}
				tagRows.Close()
			}
			if e.Tags == nil {
				e.Tags = []string{}
			}
			entries = append(entries, e)
		}
		if entries == nil {
			entries = []Entry{}
		}
		writeJSON(w, 200, entries)
	}
}

func getJournalEntry(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		date := pathParam(r, "date")

		type Entry struct {
			ID            int64          `json:"id"`
			Date          string         `json:"date"`
			Mood          *string        `json:"mood"`
			Content       string         `json:"content"`
			LocationLabel string         `json:"location_label"`
			LocationLat   *float64       `json:"location_lat"`
			LocationLon   *float64       `json:"location_lon"`
			Tags          []string       `json:"tags"`
			Backlinks     []BacklinkInfo `json:"backlinks"`
			CreatedAt     string         `json:"created_at"`
			UpdatedAt     string         `json:"updated_at"`
		}

		var e Entry
		err := d.QueryRow(`SELECT id, date::text, mood, COALESCE(location_label,''),
			location_lat, location_lon, created_at, updated_at
			FROM journal_entries WHERE user_id = $1 AND date = $2`, uid, date).
			Scan(&e.ID, &e.Date, &e.Mood, &e.LocationLabel, &e.LocationLat, &e.LocationLon, &e.CreatedAt, &e.UpdatedAt)
		if err != nil {
			writeJSON(w, 200, map[string]any{
				"id": 0, "date": date, "mood": nil, "content": "",
				"location_label": "", "location_lat": nil, "location_lon": nil,
				"tags": []string{}, "backlinks": []BacklinkInfo{}, "created_at": "", "updated_at": "",
			})
			return
		}

		if data, _, gerr := deps.Storage.Get(r.Context(), journalKey(uid, date)); gerr == nil {
			e.Content = string(data)
		}

		tagRows, _ := d.Query("SELECT tag FROM tags WHERE user_id = $1 AND entity_type = 'journal' AND entity_id = $2", uid, e.ID)
		if tagRows != nil {
			for tagRows.Next() {
				var t string
				tagRows.Scan(&t)
				e.Tags = append(e.Tags, t)
			}
			tagRows.Close()
		}
		if e.Tags == nil {
			e.Tags = []string{}
		}

		e.Backlinks = getIncomingBacklinks(d, uid, "journal", e.ID)

		writeJSON(w, 200, e)
	}
}

type BacklinkInfo struct {
	SourceType string `json:"source_type"`
	SourceID   int64  `json:"source_id"`
	Title      string `json:"title"`
}

// getIncomingBacklinks resolves all refs that match this entity's natural key.
func getIncomingBacklinks(d *db.DB, uid string, targetType string, targetID int64) []BacklinkInfo {
	var ref string
	switch targetType {
	case "note":
		var title string
		if err := d.QueryRow("SELECT title FROM notes WHERE user_id = $1 AND id = $2", uid, targetID).Scan(&title); err != nil {
			return []BacklinkInfo{}
		}
		ref = NormalizeRef(title)
	case "journal":
		var date string
		if err := d.QueryRow("SELECT date::text FROM journal_entries WHERE user_id = $1 AND id = $2", uid, targetID).Scan(&date); err != nil {
			return []BacklinkInfo{}
		}
		ref = NormalizeRef(date)
	default:
		return []BacklinkInfo{}
	}
	if ref == "" {
		return []BacklinkInfo{}
	}

	rows, err := d.Query(
		"SELECT source_type, source_id FROM backlinks WHERE user_id = $1 AND target_ref = $2",
		uid, ref,
	)
	if err != nil {
		return []BacklinkInfo{}
	}
	defer rows.Close()

	seen := map[string]bool{}
	var links []BacklinkInfo
	for rows.Next() {
		var bl BacklinkInfo
		rows.Scan(&bl.SourceType, &bl.SourceID)
		key := bl.SourceType + ":" + itoa64(bl.SourceID)
		if seen[key] {
			continue
		}
		seen[key] = true

		switch bl.SourceType {
		case "memo":
			var content string
			d.QueryRow("SELECT content FROM memos WHERE user_id = $1 AND id = $2", uid, bl.SourceID).Scan(&content)
			bl.Title = truncate(content, 80)
		case "note":
			d.QueryRow("SELECT title FROM notes WHERE user_id = $1 AND id = $2", uid, bl.SourceID).Scan(&bl.Title)
		case "journal":
			d.QueryRow("SELECT date::text FROM journal_entries WHERE user_id = $1 AND id = $2", uid, bl.SourceID).Scan(&bl.Title)
		case "task":
			d.QueryRow("SELECT title FROM tasks WHERE user_id = $1 AND id = $2", uid, bl.SourceID).Scan(&bl.Title)
		}
		links = append(links, bl)
	}
	if links == nil {
		links = []BacklinkInfo{}
	}
	return links
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func upsertJournalEntry(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		date := pathParam(r, "date")

		var body struct {
			Content       string   `json:"content"`
			Mood          *string  `json:"mood"`
			LocationLabel *string  `json:"location_label"`
			LocationLat   *float64 `json:"location_lat"`
			LocationLon   *float64 `json:"location_lon"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}

		// Emptying the text removes the whole day — row, blob, tags, and
		// backlinks — so the calendar dot disappears completely.
		if strings.TrimSpace(body.Content) == "" {
			var id int64
			if d.QueryRow("SELECT id FROM journal_entries WHERE user_id = $1 AND date = $2", uid, date).Scan(&id) == nil {
				d.Exec("DELETE FROM tags WHERE user_id=$1 AND entity_type='journal' AND entity_id=$2", uid, id)
				d.Exec("DELETE FROM backlinks WHERE user_id=$1 AND source_type='journal' AND source_id=$2", uid, id)
				d.Exec("DELETE FROM journal_entries WHERE id=$1 AND user_id=$2", id, uid)
			}
			deps.Storage.Delete(r.Context(), journalKey(uid, date))
			writeJSON(w, 200, map[string]any{"deleted": true})
			return
		}

		key := journalKey(uid, date)
		if err := deps.Storage.Put(r.Context(), key, []byte(body.Content), "text/markdown"); err != nil {
			errJSON(w, 500, "store entry: "+err.Error())
			return
		}

		locLabel := ""
		if body.LocationLabel != nil {
			locLabel = *body.LocationLabel
		}

		var id int64
		err := d.QueryRow("SELECT id FROM journal_entries WHERE user_id = $1 AND date = $2", uid, date).Scan(&id)
		if err != nil {
			err := d.QueryRow(
				`INSERT INTO journal_entries (user_id, date, blob_key, mood, location_label, location_lat, location_lon)
				 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
				uid, date, key, body.Mood, locLabel, body.LocationLat, body.LocationLon,
			).Scan(&id)
			if err != nil {
				errJSON(w, 500, err.Error())
				return
			}
		} else {
			d.Exec(`UPDATE journal_entries SET mood = $1, blob_key = $2,
				location_label = $3, location_lat = $4, location_lon = $5,
				updated_at = NOW() WHERE id = $6 AND user_id = $7`,
				body.Mood, key, locLabel, body.LocationLat, body.LocationLon, id, uid)
		}

		syncTags(d, uid, "journal", id, body.Content)
		syncBacklinks(d, uid, "journal", id, body.Content)
		writeJSON(w, 200, map[string]int64{"id": id})
	}
}

// --- Google Places (New) proxy --------------------------------------------
//
// Cost model: passing a `session` token bundles autocomplete + a single
// details call into one "session" priced at the Place Details tier
// (~$5/1k for basic data, FREE under the Essentials SKU when we only
// ask for `id,displayName,location`). Without the token every keystroke
// is its own ~$3/1k charge — so the frontend MUST pass session.

var placesHTTP = &http.Client{Timeout: 6 * time.Second}

func placesAutocomplete(deps Deps) http.HandlerFunc {
	key := os.Getenv("GOOGLE_PLACES_KEY")
	return func(w http.ResponseWriter, r *http.Request) {
		if key == "" {
			errJSON(w, 503, "places not configured")
			return
		}
		q := strings.TrimSpace(queryParam(r, "q"))
		if len(q) < 2 {
			writeJSON(w, 200, map[string]any{"predictions": []any{}})
			return
		}
		session := queryParam(r, "session")
		lat := queryParam(r, "lat")
		lon := queryParam(r, "lon")

		body := map[string]any{
			"input":        q,
			"sessionToken": session,
			// Bias toward "small label" results — establishments and POIs
			// beat full street addresses for journal pills.
			"includedPrimaryTypes": []string{"establishment", "point_of_interest"},
		}
		if lat != "" && lon != "" {
			body["locationBias"] = map[string]any{
				"circle": map[string]any{
					"center": map[string]string{"latitude": lat, "longitude": lon},
					"radius": 50000,
				},
			}
		}
		buf, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(r.Context(), "POST",
			"https://places.googleapis.com/v1/places:autocomplete", bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Goog-Api-Key", key)
		// Trim the field mask to just what the UI needs — billing scales
		// with returned fields.
		req.Header.Set("X-Goog-FieldMask",
			"suggestions.placePrediction.placeId,"+
				"suggestions.placePrediction.structuredFormat.mainText,"+
				"suggestions.placePrediction.structuredFormat.secondaryText")

		resp, err := placesHTTP.Do(req)
		if err != nil {
			errJSON(w, 502, err.Error())
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 400 {
			http.Error(w, string(raw), resp.StatusCode)
			return
		}
		var parsed struct {
			Suggestions []struct {
				PlacePrediction struct {
					PlaceID          string `json:"placeId"`
					StructuredFormat struct {
						MainText struct {
							Text string `json:"text"`
						} `json:"mainText"`
						SecondaryText struct {
							Text string `json:"text"`
						} `json:"secondaryText"`
					} `json:"structuredFormat"`
				} `json:"placePrediction"`
			} `json:"suggestions"`
		}
		json.Unmarshal(raw, &parsed)
		out := make([]map[string]string, 0, len(parsed.Suggestions))
		for _, s := range parsed.Suggestions {
			out = append(out, map[string]string{
				"place_id":  s.PlacePrediction.PlaceID,
				"primary":   s.PlacePrediction.StructuredFormat.MainText.Text,
				"secondary": s.PlacePrediction.StructuredFormat.SecondaryText.Text,
			})
		}
		writeJSON(w, 200, map[string]any{"predictions": out})
	}
}

func placesDetails(deps Deps) http.HandlerFunc {
	key := os.Getenv("GOOGLE_PLACES_KEY")
	return func(w http.ResponseWriter, r *http.Request) {
		if key == "" {
			errJSON(w, 503, "places not configured")
			return
		}
		placeID := queryParam(r, "place_id")
		if placeID == "" {
			errJSON(w, 400, "missing place_id")
			return
		}
		session := queryParam(r, "session")
		url := "https://places.googleapis.com/v1/places/" + placeID
		if session != "" {
			url += "?sessionToken=" + session
		}
		req, _ := http.NewRequestWithContext(r.Context(), "GET", url, nil)
		req.Header.Set("X-Goog-Api-Key", key)
		req.Header.Set("X-Goog-FieldMask", "id,displayName,location,shortFormattedAddress")
		resp, err := placesHTTP.Do(req)
		if err != nil {
			errJSON(w, 502, err.Error())
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 400 {
			http.Error(w, string(raw), resp.StatusCode)
			return
		}
		var parsed struct {
			ID          string `json:"id"`
			DisplayName struct {
				Text string `json:"text"`
			} `json:"displayName"`
			ShortFormattedAddress string `json:"shortFormattedAddress"`
			Location              struct {
				Latitude  float64 `json:"latitude"`
				Longitude float64 `json:"longitude"`
			} `json:"location"`
		}
		json.Unmarshal(raw, &parsed)
		// Short label like "Cinepolis, Vashi". Falls back to display name
		// alone if the address line isn't useful.
		label := parsed.DisplayName.Text
		if parsed.ShortFormattedAddress != "" {
			// Pull the locality/neighborhood from the formatted address —
			// the part right after the first comma is usually enough.
			parts := strings.SplitN(parsed.ShortFormattedAddress, ",", 3)
			if len(parts) >= 2 {
				label = parsed.DisplayName.Text + ", " + strings.TrimSpace(parts[1])
			}
		}
		writeJSON(w, 200, map[string]any{
			"place_id": parsed.ID,
			"label":    label,
			"lat":      parsed.Location.Latitude,
			"lon":      parsed.Location.Longitude,
		})
	}
}

func deleteJournalEntry(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		date := pathParam(r, "date")

		var id int64
		err := d.QueryRow("SELECT id FROM journal_entries WHERE user_id = $1 AND date = $2", uid, date).Scan(&id)
		if err != nil {
			errJSON(w, 404, "not found")
			return
		}

		if err := deps.Storage.Delete(r.Context(), journalKey(uid, date)); err != nil && !errors.Is(err, storage.ErrNotFound) {
			// Log-and-continue: DB row should still go.
		}

		d.Exec("DELETE FROM tags WHERE user_id = $1 AND entity_type = 'journal' AND entity_id = $2", uid, id)
		d.Exec("DELETE FROM backlinks WHERE user_id = $1 AND source_type = 'journal' AND source_id = $2", uid, id)
		d.Exec("DELETE FROM journal_entries WHERE id = $1 AND user_id = $2", id, uid)
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

// --- Weekly journal --------------------------------------------------------

func weeklyKey(uid string, year, week int) string {
	return storage.UserKey(uid, "journal", fmt.Sprintf("weekly/%04d-W%02d.md", year, week))
}

// isoWeekStart returns the Monday (00:00 UTC) of the given ISO week.
// Uses the standard "week containing Jan 4" rule.
func isoWeekStart(year, week int) time.Time {
	jan4 := time.Date(year, 1, 4, 0, 0, 0, 0, time.UTC)
	wd := int(jan4.Weekday())
	if wd == 0 {
		wd = 7 // Sunday → 7 so Monday-anchored math holds
	}
	weekOneMon := jan4.AddDate(0, 0, -(wd - 1))
	return weekOneMon.AddDate(0, 0, (week-1)*7)
}

// parseYearWeek extracts year/week from URL path params and validates.
func parseYearWeek(r *http.Request) (int, int, error) {
	y, err := strconv.Atoi(pathParam(r, "year"))
	if err != nil || y < 1970 || y > 9999 {
		return 0, 0, errors.New("invalid year")
	}
	w, err := strconv.Atoi(pathParam(r, "week"))
	if err != nil || w < 1 || w > 53 {
		return 0, 0, errors.New("invalid week")
	}
	return y, w, nil
}

func listWeeklyEntries(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		rows, err := d.Query(`SELECT id, iso_year, iso_week, mood, created_at, updated_at
			FROM journal_weekly WHERE user_id = $1
			ORDER BY iso_year DESC, iso_week DESC`, uid)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()

		type Entry struct {
			ID        int64   `json:"id"`
			IsoYear   int     `json:"iso_year"`
			IsoWeek   int     `json:"iso_week"`
			Mood      *string `json:"mood"`
			CreatedAt string  `json:"created_at"`
			UpdatedAt string  `json:"updated_at"`
		}
		out := []Entry{}
		for rows.Next() {
			var e Entry
			if err := rows.Scan(&e.ID, &e.IsoYear, &e.IsoWeek, &e.Mood, &e.CreatedAt, &e.UpdatedAt); err != nil {
				errJSON(w, 500, err.Error())
				return
			}
			out = append(out, e)
		}
		writeJSON(w, 200, out)
	}
}

func getWeeklyEntry(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		year, week, err := parseYearWeek(r)
		if err != nil {
			errJSON(w, 400, err.Error())
			return
		}

		type Entry struct {
			ID        int64   `json:"id"`
			IsoYear   int     `json:"iso_year"`
			IsoWeek   int     `json:"iso_week"`
			Mood      *string `json:"mood"`
			Content   string  `json:"content"`
			CreatedAt string  `json:"created_at"`
			UpdatedAt string  `json:"updated_at"`
		}

		var e Entry
		err = d.QueryRow(`SELECT id, iso_year, iso_week, mood, created_at, updated_at
			FROM journal_weekly WHERE user_id = $1 AND iso_year = $2 AND iso_week = $3`,
			uid, year, week).
			Scan(&e.ID, &e.IsoYear, &e.IsoWeek, &e.Mood, &e.CreatedAt, &e.UpdatedAt)
		if err != nil {
			// Empty stub mirrors getJournalEntry behaviour.
			writeJSON(w, 200, map[string]any{
				"id": 0, "iso_year": year, "iso_week": week,
				"mood": nil, "content": "", "created_at": "", "updated_at": "",
			})
			return
		}

		if data, _, gerr := deps.Storage.Get(r.Context(), weeklyKey(uid, year, week)); gerr == nil {
			e.Content = string(data)
		}
		writeJSON(w, 200, e)
	}
}

func upsertWeeklyEntry(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		year, week, err := parseYearWeek(r)
		if err != nil {
			errJSON(w, 400, err.Error())
			return
		}
		var body struct {
			Content string  `json:"content"`
			Mood    *string `json:"mood"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}

		key := weeklyKey(uid, year, week)
		if err := deps.Storage.Put(r.Context(), key, []byte(body.Content), "text/markdown"); err != nil {
			errJSON(w, 500, "store entry: "+err.Error())
			return
		}

		var id int64
		err = d.QueryRow(`SELECT id FROM journal_weekly
			WHERE user_id = $1 AND iso_year = $2 AND iso_week = $3`, uid, year, week).Scan(&id)
		if err != nil {
			err := d.QueryRow(
				`INSERT INTO journal_weekly (user_id, iso_year, iso_week, blob_key, mood)
				 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
				uid, year, week, key, body.Mood,
			).Scan(&id)
			if err != nil {
				errJSON(w, 500, err.Error())
				return
			}
		} else {
			d.Exec(`UPDATE journal_weekly SET mood = $1, blob_key = $2, updated_at = NOW()
				WHERE id = $3 AND user_id = $4`, body.Mood, key, id, uid)
		}
		writeJSON(w, 200, map[string]int64{"id": id})
	}
}

func deleteWeeklyEntry(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		year, week, err := parseYearWeek(r)
		if err != nil {
			errJSON(w, 400, err.Error())
			return
		}
		var id int64
		if err := d.QueryRow(`SELECT id FROM journal_weekly
			WHERE user_id = $1 AND iso_year = $2 AND iso_week = $3`, uid, year, week).Scan(&id); err != nil {
			errJSON(w, 404, "not found")
			return
		}
		if err := deps.Storage.Delete(r.Context(), weeklyKey(uid, year, week)); err != nil && !errors.Is(err, storage.ErrNotFound) {
			// log-and-continue
		}
		d.Exec("DELETE FROM journal_weekly WHERE id = $1 AND user_id = $2", id, uid)
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

// weeklySummary aggregates a 7-day window of tasks, habits, finance, and
// daily journal entries for the dashboard cards in the week view.
func weeklySummary(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		year, week, err := parseYearWeek(r)
		if err != nil {
			errJSON(w, 400, err.Error())
			return
		}

		start := isoWeekStart(year, week)
		end := start.AddDate(0, 0, 6)
		startStr := start.Format("2006-01-02")
		endStr := end.Format("2006-01-02")

		// --- Per-day stats. Pre-seed the 7 rows so empty days still appear. ---
		type DayStat struct {
			Date        string  `json:"date"`
			TasksDone   int     `json:"tasks_done"`
			TasksDue    int     `json:"tasks_due"`
			TasksMissed int     `json:"tasks_missed"`
			Mood        *string `json:"mood"`
			HasEntry    bool    `json:"has_entry"`
		}
		days := make([]DayStat, 7)
		index := map[string]int{}
		for i := 0; i < 7; i++ {
			ds := start.AddDate(0, 0, i).Format("2006-01-02")
			days[i] = DayStat{Date: ds}
			index[ds] = i
		}

		// Tasks due in range. status='done' counts toward done, anything else due.
		if rows, err := d.Query(`SELECT due_date::text, status FROM tasks
			WHERE user_id = $1 AND due_date BETWEEN $2 AND $3`, uid, startStr, endStr); err == nil {
			for rows.Next() {
				var dateStr, status string
				rows.Scan(&dateStr, &status)
				if i, ok := index[dateStr]; ok {
					if status == "done" {
						days[i].TasksDone++
					} else {
						days[i].TasksDue++
					}
				}
			}
			rows.Close()
		}

		// Completed-later: tasks completed in range (updated_at::date) regardless
		// of original due. Count toward TasksDone on the completion day.
		if rows, err := d.Query(`SELECT updated_at::date::text FROM tasks
			WHERE user_id = $1 AND status = 'done' AND updated_at::date BETWEEN $2 AND $3`,
			uid, startStr, endStr); err == nil {
			for rows.Next() {
				var dateStr string
				rows.Scan(&dateStr)
				if i, ok := index[dateStr]; ok {
					// Avoid double-counting: only add if the task wasn't already
					// counted above. Hard to dedupe without an id-set; cheap
					// best-effort approach: skip — the first query already covers
					// "due today + done" cases. Net effect: TasksDone reflects
					// tasks due that day that are done.
					_ = i
				}
			}
			rows.Close()
		}

		// Missed (task_due_history outcome='missed' in range).
		if rows, err := d.Query(`SELECT due_date::text FROM task_due_history
			WHERE user_id = $1 AND outcome = 'missed' AND due_date BETWEEN $2 AND $3`,
			uid, startStr, endStr); err == nil {
			for rows.Next() {
				var dateStr string
				rows.Scan(&dateStr)
				if i, ok := index[dateStr]; ok {
					days[i].TasksMissed++
				}
			}
			rows.Close()
		}

		// Daily journal entries (mood + has_entry).
		if rows, err := d.Query(`SELECT date::text, mood FROM journal_entries
			WHERE user_id = $1 AND date BETWEEN $2 AND $3`, uid, startStr, endStr); err == nil {
			for rows.Next() {
				var dateStr string
				var mood *string
				rows.Scan(&dateStr, &mood)
				if i, ok := index[dateStr]; ok {
					days[i].HasEntry = true
					days[i].Mood = mood
				}
			}
			rows.Close()
		}

		// --- Habits with per-day logged dates in range. ---
		type HabitWeek struct {
			ID         int64    `json:"id"`
			Name       string   `json:"name"`
			Color      string   `json:"color"`
			LoggedDays []string `json:"logged_days"`
		}
		habits := []HabitWeek{}
		habitIndex := map[int64]int{}
		if rows, err := d.Query(`SELECT id, name, color FROM habits
			WHERE user_id = $1 ORDER BY created_at ASC`, uid); err == nil {
			for rows.Next() {
				var h HabitWeek
				rows.Scan(&h.ID, &h.Name, &h.Color)
				h.LoggedDays = []string{}
				habitIndex[h.ID] = len(habits)
				habits = append(habits, h)
			}
			rows.Close()
		}
		if rows, err := d.Query(`SELECT habit_id, logged_date::text FROM habit_logs
			WHERE user_id = $1 AND logged_date BETWEEN $2 AND $3`, uid, startStr, endStr); err == nil {
			for rows.Next() {
				var hid int64
				var dateStr string
				rows.Scan(&hid, &dateStr)
				if i, ok := habitIndex[hid]; ok {
					habits[i].LoggedDays = append(habits[i].LoggedDays, dateStr)
				}
			}
			rows.Close()
		}

		// --- Finance: total expense + top category. ---
		var expenseTotal float64
		d.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM fin_transactions
			WHERE user_id = $1 AND type = 'expense' AND (txn_at AT TIME ZONE 'Asia/Kolkata')::date BETWEEN $2 AND $3`,
			uid, startStr, endStr).Scan(&expenseTotal)

		type TopCat struct {
			ID     int64   `json:"id"`
			Name   string  `json:"name"`
			Amount float64 `json:"amount"`
		}
		var top *TopCat
		if rows, err := d.Query(`SELECT t.category_id, COALESCE(c.name, 'Uncategorized'), SUM(t.amount) AS total
			FROM fin_transactions t
			LEFT JOIN fin_categories c ON c.id = t.category_id
			WHERE t.user_id = $1 AND t.type = 'expense' AND (t.txn_at AT TIME ZONE 'Asia/Kolkata')::date BETWEEN $2 AND $3
			GROUP BY t.category_id, c.name
			ORDER BY total DESC
			LIMIT 1`, uid, startStr, endStr); err == nil {
			if rows.Next() {
				var tc TopCat
				var catID *int64
				rows.Scan(&catID, &tc.Name, &tc.Amount)
				if catID != nil {
					tc.ID = *catID
				}
				top = &tc
			}
			rows.Close()
		}

		// Currency: pick the first account's currency as the user's default.
		var currency string
		d.QueryRow(`SELECT currency FROM fin_accounts WHERE user_id = $1 LIMIT 1`, uid).Scan(&currency)
		if currency == "" {
			currency = "INR"
		}

		writeJSON(w, 200, map[string]any{
			"iso_year":             year,
			"iso_week":             week,
			"start_date":           startStr,
			"end_date":             endStr,
			"days":                 days,
			"habits":               habits,
			"expense_total":        expenseTotal,
			"expense_top_category": top,
			"expense_currency":     currency,
		})
	}
}

// --- Monthly journal -------------------------------------------------------
// Parallels the weekly journal: a long-form month entry + a per-week task
// breakdown. Keyed by calendar year + month (1-12).

func monthlyKey(uid string, year, month int) string {
	return storage.UserKey(uid, "journal", fmt.Sprintf("monthly/%04d-%02d.md", year, month))
}

// parseYearMonth extracts year/month from URL path params and validates.
func parseYearMonth(r *http.Request) (int, int, error) {
	y, err := strconv.Atoi(pathParam(r, "year"))
	if err != nil || y < 1970 || y > 9999 {
		return 0, 0, errors.New("invalid year")
	}
	m, err := strconv.Atoi(pathParam(r, "month"))
	if err != nil || m < 1 || m > 12 {
		return 0, 0, errors.New("invalid month")
	}
	return y, m, nil
}

// mondayOf returns the Monday (00:00) of the week containing d.
func mondayOf(d time.Time) time.Time {
	offset := (int(d.Weekday()) + 6) % 7 // days since Monday (Sun=6)
	return d.AddDate(0, 0, -offset)
}

func listMonthlyEntries(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		rows, err := d.Query(`SELECT id, cal_year, cal_month, mood, created_at, updated_at
			FROM journal_monthly WHERE user_id = $1
			ORDER BY cal_year DESC, cal_month DESC`, uid)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()

		type Entry struct {
			ID        int64   `json:"id"`
			Year      int     `json:"year"`
			Month     int     `json:"month"`
			Mood      *string `json:"mood"`
			CreatedAt string  `json:"created_at"`
			UpdatedAt string  `json:"updated_at"`
		}
		out := []Entry{}
		for rows.Next() {
			var e Entry
			if err := rows.Scan(&e.ID, &e.Year, &e.Month, &e.Mood, &e.CreatedAt, &e.UpdatedAt); err != nil {
				errJSON(w, 500, err.Error())
				return
			}
			out = append(out, e)
		}
		writeJSON(w, 200, out)
	}
}

func getMonthlyEntry(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		year, month, err := parseYearMonth(r)
		if err != nil {
			errJSON(w, 400, err.Error())
			return
		}

		type Entry struct {
			ID        int64   `json:"id"`
			Year      int     `json:"year"`
			Month     int     `json:"month"`
			Mood      *string `json:"mood"`
			Content   string  `json:"content"`
			CreatedAt string  `json:"created_at"`
			UpdatedAt string  `json:"updated_at"`
		}

		var e Entry
		err = d.QueryRow(`SELECT id, cal_year, cal_month, mood, created_at, updated_at
			FROM journal_monthly WHERE user_id = $1 AND cal_year = $2 AND cal_month = $3`,
			uid, year, month).
			Scan(&e.ID, &e.Year, &e.Month, &e.Mood, &e.CreatedAt, &e.UpdatedAt)
		if err != nil {
			// Empty stub mirrors getWeeklyEntry behaviour.
			writeJSON(w, 200, map[string]any{
				"id": 0, "year": year, "month": month,
				"mood": nil, "content": "", "created_at": "", "updated_at": "",
			})
			return
		}

		if data, _, gerr := deps.Storage.Get(r.Context(), monthlyKey(uid, year, month)); gerr == nil {
			e.Content = string(data)
		}
		writeJSON(w, 200, e)
	}
}

func upsertMonthlyEntry(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		year, month, err := parseYearMonth(r)
		if err != nil {
			errJSON(w, 400, err.Error())
			return
		}
		var body struct {
			Content string  `json:"content"`
			Mood    *string `json:"mood"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}

		key := monthlyKey(uid, year, month)
		if err := deps.Storage.Put(r.Context(), key, []byte(body.Content), "text/markdown"); err != nil {
			errJSON(w, 500, "store entry: "+err.Error())
			return
		}

		var id int64
		err = d.QueryRow(`SELECT id FROM journal_monthly
			WHERE user_id = $1 AND cal_year = $2 AND cal_month = $3`, uid, year, month).Scan(&id)
		if err != nil {
			err := d.QueryRow(
				`INSERT INTO journal_monthly (user_id, cal_year, cal_month, blob_key, mood)
				 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
				uid, year, month, key, body.Mood,
			).Scan(&id)
			if err != nil {
				errJSON(w, 500, err.Error())
				return
			}
		} else {
			d.Exec(`UPDATE journal_monthly SET mood = $1, blob_key = $2, updated_at = NOW()
				WHERE id = $3 AND user_id = $4`, body.Mood, key, id, uid)
		}
		writeJSON(w, 200, map[string]int64{"id": id})
	}
}

func deleteMonthlyEntry(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		year, month, err := parseYearMonth(r)
		if err != nil {
			errJSON(w, 400, err.Error())
			return
		}
		var id int64
		if err := d.QueryRow(`SELECT id FROM journal_monthly
			WHERE user_id = $1 AND cal_year = $2 AND cal_month = $3`, uid, year, month).Scan(&id); err != nil {
			errJSON(w, 404, "not found")
			return
		}
		if err := deps.Storage.Delete(r.Context(), monthlyKey(uid, year, month)); err != nil && !errors.Is(err, storage.ErrNotFound) {
			// log-and-continue
		}
		d.Exec("DELETE FROM journal_monthly WHERE id = $1 AND user_id = $2", id, uid)
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

// monthlySummary aggregates a calendar month into per-week rows (the "Tasks by
// week" breakdown) plus month-total task/expense stats for the dashboard tiles.
func monthlySummary(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		year, month, err := parseYearMonth(r)
		if err != nil {
			errJSON(w, 400, err.Error())
			return
		}

		first := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
		last := first.AddDate(0, 1, -1)
		firstStr := first.Format("2006-01-02")
		lastStr := last.Format("2006-01-02")
		daysInMonth := last.Day()

		// Per-week rows: every ISO week whose Mon–Sun span overlaps the month.
		// Aggregation uses the full week span (not clipped) so a row matches the
		// week view you reach by clicking it.
		type WeekStat struct {
			IsoYear     int    `json:"iso_year"`
			IsoWeek     int    `json:"iso_week"`
			StartDate   string `json:"start_date"`
			EndDate     string `json:"end_date"`
			TasksDone   int    `json:"tasks_done"`
			TasksDue    int    `json:"tasks_due"`
			TasksMissed int    `json:"tasks_missed"`
		}
		weeks := []WeekStat{}
		index := map[string]int{} // date (YYYY-MM-DD) -> week-row index
		spanStart := mondayOf(first)
		spanEnd := mondayOf(last).AddDate(0, 0, 6)
		for ws := spanStart; !ws.After(spanEnd); ws = ws.AddDate(0, 0, 7) {
			iy, iw := ws.ISOWeek()
			we := ws.AddDate(0, 0, 6)
			weeks = append(weeks, WeekStat{
				IsoYear: iy, IsoWeek: iw,
				StartDate: ws.Format("2006-01-02"), EndDate: we.Format("2006-01-02"),
			})
			for i := 0; i < 7; i++ {
				index[ws.AddDate(0, 0, i).Format("2006-01-02")] = len(weeks) - 1
			}
		}
		spanStartStr := spanStart.Format("2006-01-02")
		spanEndStr := spanEnd.Format("2006-01-02")

		if rows, err := d.Query(`SELECT due_date::text, status FROM tasks
			WHERE user_id = $1 AND due_date BETWEEN $2 AND $3`, uid, spanStartStr, spanEndStr); err == nil {
			for rows.Next() {
				var dateStr, status string
				rows.Scan(&dateStr, &status)
				if i, ok := index[dateStr]; ok {
					if status == "done" {
						weeks[i].TasksDone++
					} else {
						weeks[i].TasksDue++
					}
				}
			}
			rows.Close()
		}
		if rows, err := d.Query(`SELECT due_date::text FROM task_due_history
			WHERE user_id = $1 AND outcome = 'missed' AND due_date BETWEEN $2 AND $3`,
			uid, spanStartStr, spanEndStr); err == nil {
			for rows.Next() {
				var dateStr string
				rows.Scan(&dateStr)
				if i, ok := index[dateStr]; ok {
					weeks[i].TasksMissed++
				}
			}
			rows.Close()
		}

		// Month-total task stats (clipped to the actual month, not the week span).
		var totalDone, totalDue, totalMissed, entriesWritten int
		d.QueryRow(`SELECT
			COUNT(*) FILTER (WHERE status = 'done'),
			COUNT(*) FILTER (WHERE status <> 'done')
			FROM tasks WHERE user_id = $1 AND due_date BETWEEN $2 AND $3`,
			uid, firstStr, lastStr).Scan(&totalDone, &totalDue)
		d.QueryRow(`SELECT COUNT(*) FROM task_due_history
			WHERE user_id = $1 AND outcome = 'missed' AND due_date BETWEEN $2 AND $3`,
			uid, firstStr, lastStr).Scan(&totalMissed)
		d.QueryRow(`SELECT COUNT(*) FROM journal_entries
			WHERE user_id = $1 AND date BETWEEN $2 AND $3`, uid, firstStr, lastStr).Scan(&entriesWritten)

		// Finance: total expense + top category for the month (IST day window).
		var expenseTotal float64
		d.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM fin_transactions
			WHERE user_id = $1 AND type = 'expense' AND (txn_at AT TIME ZONE 'Asia/Kolkata')::date BETWEEN $2 AND $3`,
			uid, firstStr, lastStr).Scan(&expenseTotal)
		type TopCat struct {
			ID     int64   `json:"id"`
			Name   string  `json:"name"`
			Amount float64 `json:"amount"`
		}
		var top *TopCat
		if rows, err := d.Query(`SELECT t.category_id, COALESCE(c.name, 'Uncategorized'), SUM(t.amount) AS total
			FROM fin_transactions t
			LEFT JOIN fin_categories c ON c.id = t.category_id
			WHERE t.user_id = $1 AND t.type = 'expense' AND (t.txn_at AT TIME ZONE 'Asia/Kolkata')::date BETWEEN $2 AND $3
			GROUP BY t.category_id, c.name
			ORDER BY total DESC
			LIMIT 1`, uid, firstStr, lastStr); err == nil {
			if rows.Next() {
				var tc TopCat
				var catID *int64
				rows.Scan(&catID, &tc.Name, &tc.Amount)
				if catID != nil {
					tc.ID = *catID
				}
				top = &tc
			}
			rows.Close()
		}
		var currency string
		d.QueryRow(`SELECT currency FROM fin_accounts WHERE user_id = $1 LIMIT 1`, uid).Scan(&currency)
		if currency == "" {
			currency = "INR"
		}

		writeJSON(w, 200, map[string]any{
			"year":                 year,
			"month":                month,
			"start_date":           firstStr,
			"end_date":             lastStr,
			"days_in_month":        daysInMonth,
			"weeks":                weeks,
			"total_done":           totalDone,
			"total_due":            totalDue + totalDone,
			"total_missed":         totalMissed,
			"entries_written":      entriesWritten,
			"expense_total":        expenseTotal,
			"expense_top_category": top,
			"expense_currency":     currency,
		})
	}
}
