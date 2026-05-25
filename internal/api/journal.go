package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"sajni/internal/db"
	"sajni/internal/storage"
)

func registerJournalRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("GET /api/journal", listJournal(deps))
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
