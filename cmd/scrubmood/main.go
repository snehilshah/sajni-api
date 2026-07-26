// Command scrubmood strips leftover `mood` keys out of stored AI chat
// history. Mood was removed from the journal everywhere else, but
// ai_sessions.messages holds frozen Gemini turns — function calls and their
// responses — recorded back when the journal tools still echoed a mood
// field. Those turns are a transcript, so no live code reads them; they are
// simply the last place the string survives.
//
//	go run ./cmd/scrubmood            # reads DATABASE_URL from .env
//	go run ./cmd/scrubmood -dry-run   # report what would change, write nothing
//
// The walk is structural rather than a text substitution: the key nests at
// varying depths inside parts[].functionResponse.response.result and inside
// arrays of items, so a regex over the JSON would have to guess at
// separators. Deleting the key from every object it appears in is exact.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"os"
	"strings"

	_ "time/tzdata"

	"github.com/rs/zerolog/log"

	"sajni/internal/db"
	"sajni/internal/logger"
)

func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.Trim(strings.TrimSpace(line[eq+1:]), `"'`)
		if _, set := os.LookupEnv(key); !set {
			os.Setenv(key, val)
		}
	}
}

// stripMood removes every "mood" key from v, at any depth, and reports how
// many it dropped. Only maps are edited — a "mood" string sitting in an
// array is someone's prose, not a field, so it stays.
func stripMood(v any) (any, int) {
	switch t := v.(type) {
	case map[string]any:
		n := 0
		if _, ok := t["mood"]; ok {
			delete(t, "mood")
			n++
		}
		for k, child := range t {
			cleaned, c := stripMood(child)
			t[k] = cleaned
			n += c
		}
		return t, n
	case []any:
		n := 0
		for i, child := range t {
			cleaned, c := stripMood(child)
			t[i] = cleaned
			n += c
		}
		return t, n
	default:
		return v, 0
	}
}

func main() {
	dryRun := flag.Bool("dry-run", false, "report without writing")
	flag.Parse()

	loadDotEnv(".env")
	logger.Init()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal().Msg("DATABASE_URL is required")
	}
	database, err := db.New(dsn)
	if err != nil {
		log.Fatal().Err(err).Msg("connect failed")
	}
	defer database.Close()

	// Only sessions that mention the key at all — the JSONB->text cast is
	// cheap next to re-encoding every transcript we own.
	rows, err := database.Query(`
		SELECT id, messages FROM ai_sessions
		WHERE messages::text LIKE '%"mood"%'
		ORDER BY id`)
	if err != nil {
		log.Fatal().Err(err).Msg("select failed")
	}

	type change struct {
		id      int64
		payload []byte
		dropped int
	}
	var pending []change
	for rows.Next() {
		var id int64
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			log.Fatal().Err(err).Msg("scan failed")
		}
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			// A transcript we can't parse is one we must not rewrite.
			log.Warn().Int64("session", id).Err(err).Msg("skipping unparseable messages")
			continue
		}
		cleaned, dropped := stripMood(doc)
		if dropped == 0 {
			continue
		}
		out, err := json.Marshal(cleaned)
		if err != nil {
			rows.Close()
			log.Fatal().Int64("session", id).Err(err).Msg("re-encode failed")
		}
		pending = append(pending, change{id: id, payload: out, dropped: dropped})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		log.Fatal().Err(err).Msg("iterate failed")
	}
	rows.Close()

	total := 0
	for _, c := range pending {
		total += c.dropped
		log.Info().Int64("session", c.id).Int("keys", c.dropped).Msg("mood keys found")
	}
	if *dryRun {
		log.Info().Int("sessions", len(pending)).Int("keys", total).Msg("dry run — nothing written")
		return
	}

	for _, c := range pending {
		// updated_at deliberately untouched: this edits a stored transcript,
		// it isn't the user saying anything new.
		if _, err := database.Exec(
			`UPDATE ai_sessions SET messages = $1::jsonb WHERE id = $2`, c.payload, c.id,
		); err != nil {
			log.Fatal().Int64("session", c.id).Err(err).Msg("update failed")
		}
	}
	log.Info().Int("sessions", len(pending)).Int("keys", total).Msg("scrubbed")
}
