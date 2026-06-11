package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"sajni/internal/db"
)

// Insights = cross-module correlation engine. Cheap SQL aggregations
// over each rolling window, plus optional Gemini narration for the top
// findings. The schedule is per-window (7d / 14d / 30d / 180d / 365d)
// and entries are upserted by (user, window, kind) so daily re-runs do
// not flood the table.

const insightSystemPrompt = `You write one short personal-coach line for each finding.
Tone: warm, specific, second person. No emoji. No fluff. <= 22 words.
Use the numbers verbatim, don't invent any.`

func registerInsightRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("GET /api/insights", listInsights(deps))
	mux.HandleFunc("POST /api/insights/run", runInsightsHandler(deps))
	mux.HandleFunc("POST /api/insights/{id}/dismiss", dismissInsight(deps))
	mux.HandleFunc("POST /api/insights/{id}/pin", pinInsight(deps))
	mux.HandleFunc("POST /api/insights/{id}/unpin", unpinInsight(deps))

	// Time-travel semantic lookup over user history — same logic the AI
	// time_travel tool runs, exposed as a plain endpoint for the Insights
	// search box.
	mux.HandleFunc("GET /api/time-travel", timeTravelHandler(deps))
}

// RegisterInsightCronHandler mounts the unauthenticated webhook used by
// Cloud Scheduler (or any external trigger). Header X-Insight-Cron must
// match INSIGHT_CRON_SECRET. Call from main once the root mux exists.
func RegisterInsightCronHandler(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("POST /internal/insights/run", insightCronHandler(deps))
}

// windowDays maps a window key to its lookback in days.
var windowDays = map[string]int{
	"1w": 7, "2w": 14, "1m": 30, "6m": 180, "1y": 365,
}

// detected is one finding emitted by a detector. body may be polished
// by Gemini before persistence.
type detected struct {
	kind, title, body string
	score             float64
	evidence          map[string]any
}

func listInsights(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		win := queryParam(r, "window")
		q := `SELECT id, window_key, kind, title, body, score, evidence, pinned, generated_at::text
			FROM insights WHERE user_id = $1 AND dismissed_at IS NULL`
		args := []any{uid}
		if win != "" {
			q += " AND window_key = $2"
			args = append(args, win)
		}
		q += " ORDER BY pinned DESC, generated_at DESC LIMIT 50"
		rows, err := d.Query(q, args...)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()
		type Row struct {
			ID          int64           `json:"id"`
			Window      string          `json:"window"`
			Kind        string          `json:"kind"`
			Title       string          `json:"title"`
			Body        string          `json:"body"`
			Score       float64         `json:"score"`
			Evidence    json.RawMessage `json:"evidence"`
			Pinned      bool            `json:"pinned"`
			GeneratedAt string          `json:"generated_at"`
		}
		out := []Row{}
		for rows.Next() {
			var row Row
			rows.Scan(&row.ID, &row.Window, &row.Kind, &row.Title, &row.Body,
				&row.Score, &row.Evidence, &row.Pinned, &row.GeneratedAt)
			out = append(out, row)
		}
		writeJSON(w, 200, out)
	}
}

func runInsightsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		win := queryParam(r, "window")
		if win == "" {
			win = "1w"
		}
		if _, ok := windowDays[win]; !ok {
			errJSON(w, 400, "invalid window")
			return
		}
		n, err := RunInsightsForUser(r.Context(), deps, uid, win)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"window": win, "generated": n})
	}
}

func dismissInsight(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		d.Exec(`UPDATE insights SET dismissed_at = NOW() WHERE id = $1 AND user_id = $2`, id, uid)
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func pinInsight(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, _ := intParam(r, "id")
		d.Exec(`UPDATE insights SET pinned = TRUE WHERE id = $1 AND user_id = $2`, id, uid)
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func unpinInsight(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, _ := intParam(r, "id")
		d.Exec(`UPDATE insights SET pinned = FALSE WHERE id = $1 AND user_id = $2`, id, uid)
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func insightCronHandler(deps Deps) http.HandlerFunc {
	expected := os.Getenv("INSIGHT_CRON_SECRET")
	return func(w http.ResponseWriter, r *http.Request) {
		if expected == "" || r.Header.Get("X-Insight-Cron") != expected {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		n, err := RunDailyInsightCron(r.Context(), deps)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, 200, map[string]int{"generated": n})
	}
}

// RunDailyInsightCron iterates every user. Each window has its own
// cadence (1w → every day; 2w → every 2d; 1m → every 7d; 6m → every
// 30d; 1y → every 60d). Cadence is enforced by checking insight_runs.
func RunDailyInsightCron(ctx context.Context, deps Deps) (int, error) {
	d := deps.DB
	cadenceDays := map[string]int{"1w": 1, "2w": 2, "1m": 7, "6m": 30, "1y": 60}

	rows, err := d.QueryContext(ctx, `SELECT id FROM users WHERE deleted_at IS NULL`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var uids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		uids = append(uids, id)
	}
	total := 0
	for _, uid := range uids {
		for win, days := range cadenceDays {
			var lastRun *time.Time
			d.QueryRowContext(ctx, `SELECT run_at FROM insight_runs
				WHERE user_id = $1 AND window_key = $2 ORDER BY run_at DESC LIMIT 1`,
				uid, win).Scan(&lastRun)
			if lastRun != nil && time.Since(*lastRun) < time.Duration(days)*24*time.Hour {
				continue
			}
			n, err := RunInsightsForUser(ctx, deps, uid, win)
			if err != nil {
				log.Warn().Err(err).Str("uid", uid).Str("window", win).Msg("insights run failed")
				continue
			}
			total += n
		}
	}
	return total, nil
}

// RunInsightsForUser computes a small bundle of correlation/aggregate
// detectors for the given window and persists them. The AI service, if
// present, polishes the body lines into a single warm sentence; without
// it the raw detector text is used as-is.
func RunInsightsForUser(ctx context.Context, deps Deps, uid string, window string) (int, error) {
	days, ok := windowDays[window]
	if !ok {
		return 0, fmt.Errorf("bad window")
	}
	d := deps.DB
	cutoff := time.Now().AddDate(0, 0, -days).Format("2006-01-02")
	var out []detected

	if s, err := dailyMoodTaskSeries(ctx, d, uid, cutoff); err == nil && len(s) >= 5 {
		r := pearson(s)
		if !math.IsNaN(r) && math.Abs(r) >= 0.35 {
			dir := "lower"
			if r > 0 {
				dir = "higher"
			}
			out = append(out, detected{
				kind:  "mood_vs_tasks",
				title: "Mood ↔ task completion",
				body: fmt.Sprintf("Across the last %dd, your mood tends to be %s on days when you complete more tasks (r=%.2f).",
					days, dir, r),
				score:    math.Abs(r),
				evidence: map[string]any{"r": r, "samples": len(s), "window_days": days},
			})
		}
	}

	out = append(out, detectHabitStreaks(ctx, d, uid, cutoff, days)...)
	out = append(out, detectSpendingSpikes(ctx, d, uid, cutoff, days)...)
	out = append(out, detectJournalCadence(ctx, d, uid, cutoff, days)...)

	if deps.AI != nil && len(out) > 0 {
		polished := tryNarrateInsights(ctx, deps, out)
		if len(polished) == len(out) {
			for i := range out {
				if polished[i] != "" {
					out[i].body = polished[i]
				}
			}
		}
	}

	for _, ins := range out {
		evJSON, _ := json.Marshal(ins.evidence)
		// Soft-replace older same-kind entries in this window so only the
		// latest copy is live; then insert the fresh row.
		d.ExecContext(ctx, `UPDATE insights SET dismissed_at = NOW()
			WHERE user_id=$1 AND window_key=$2 AND kind=$3 AND dismissed_at IS NULL`,
			uid, window, ins.kind)
		_, err := d.ExecContext(ctx, `INSERT INTO insights
			(user_id, window_key, kind, title, body, score, evidence, generated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,NOW())`,
			uid, window, ins.kind, ins.title, ins.body, ins.score, evJSON)
		if err != nil {
			log.Warn().Err(err).Msg("insight insert failed")
		}
	}

	d.ExecContext(ctx, `INSERT INTO insight_runs (user_id, window_key, status) VALUES ($1,$2,'ok')`,
		uid, window)

	return len(out), nil
}

func detectHabitStreaks(ctx context.Context, d *db.DB, uid string, cutoff string, days int) []detected {
	rows, err := d.QueryContext(ctx, `SELECT h.name,
		COUNT(*) FILTER (WHERE l.logged_date >= $2) AS done
		FROM habits h LEFT JOIN habit_logs l
		  ON l.habit_id = h.id AND l.user_id = h.user_id
		WHERE h.user_id = $1
		GROUP BY h.name ORDER BY done DESC LIMIT 3`,
		uid, cutoff)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []detected
	for rows.Next() {
		var name string
		var done int
		rows.Scan(&name, &done)
		rate := float64(done) / float64(days)
		if rate >= 0.6 {
			out = append(out, detected{
				kind:  "habit_strong_" + name,
				title: "Strong habit: " + name,
				body: fmt.Sprintf("You logged %s on %d of the last %d days (%d%%). Keep the streak.",
					name, done, days, int(rate*100)),
				score:    rate,
				evidence: map[string]any{"habit": name, "done": done, "window_days": days},
			})
		}
	}
	return out
}

func detectSpendingSpikes(ctx context.Context, d *db.DB, uid string, cutoff string, days int) []detected {
	rows, err := d.QueryContext(ctx, `WITH recent AS (
		SELECT category_id, SUM(amount) AS amt
		FROM fin_transactions WHERE user_id=$1 AND type='expense' AND (txn_at AT TIME ZONE 'Asia/Kolkata')::date >= $2
		GROUP BY category_id),
	baseline AS (
		SELECT category_id, AVG(daily) AS avg_daily FROM (
			SELECT category_id, (txn_at AT TIME ZONE 'Asia/Kolkata')::date AS d, SUM(amount) AS daily
			FROM fin_transactions WHERE user_id=$1 AND type='expense' AND (txn_at AT TIME ZONE 'Asia/Kolkata')::date < $2
			GROUP BY category_id, d
		) x GROUP BY category_id)
	SELECT c.name, r.amt, COALESCE(b.avg_daily,0) * $3
	FROM recent r LEFT JOIN baseline b ON b.category_id = r.category_id
	LEFT JOIN fin_categories c ON c.id = r.category_id
	WHERE COALESCE(b.avg_daily,0) > 0
	ORDER BY r.amt - (COALESCE(b.avg_daily,0) * $3) DESC LIMIT 3`,
		uid, cutoff, days)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []detected
	for rows.Next() {
		var name *string
		var actual, expected float64
		rows.Scan(&name, &actual, &expected)
		if expected <= 0 || actual <= expected*1.4 {
			continue
		}
		label := "Uncategorized"
		if name != nil {
			label = *name
		}
		delta := actual - expected
		out = append(out, detected{
			kind:  "spend_spike_" + label,
			title: "Spending spike: " + label,
			body: fmt.Sprintf("%s spend over the last %dd is %.0f vs an expected %.0f (+%.0f).",
				label, days, actual, expected, delta),
			score:    (actual - expected) / expected,
			evidence: map[string]any{"category": label, "actual": actual, "expected": expected},
		})
	}
	return out
}

func detectJournalCadence(ctx context.Context, d *db.DB, uid string, cutoff string, days int) []detected {
	if days < 14 {
		return nil
	}
	var entries int
	d.QueryRowContext(ctx, `SELECT COUNT(*) FROM journal_entries WHERE user_id=$1 AND date >= $2`,
		uid, cutoff).Scan(&entries)
	if entries == 0 {
		return nil
	}
	rate := float64(entries) / float64(days)
	if rate >= 0.25 {
		return nil
	}
	return []detected{{
		kind:  "journal_low_cadence",
		title: "Journaling has slowed",
		body: fmt.Sprintf("You journaled %d times in the last %dd. Even a one-line entry helps the pattern engine.",
			entries, days),
		score:    1 - rate,
		evidence: map[string]any{"entries": entries, "window_days": days},
	}}
}

// timeTravelHandler mirrors the AI time_travel tool: UNION over journals,
// memos, notes, transactions, and media with an ILIKE on the query. Each
// leg is row-capped and the merged result is sorted by date desc.
func timeTravelHandler(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		q := strings.TrimSpace(queryParam(r, "q"))
		if q == "" {
			errJSON(w, 400, "missing q")
			return
		}
		from := queryParam(r, "from")
		to := queryParam(r, "to")
		typesParam := queryParam(r, "types")
		allow := map[string]bool{}
		if typesParam != "" {
			for _, t := range strings.Split(typesParam, ",") {
				allow[strings.TrimSpace(t)] = true
			}
		}
		all := len(allow) == 0
		like := "%" + q + "%"

		type Hit struct {
			Type    string `json:"type"`
			ID      int64  `json:"id"`
			Date    string `json:"date"`
			Title   string `json:"title"`
			Excerpt string `json:"excerpt"`
		}
		var hits []Hit
		filterDate := func(col, raw string) (string, []any) {
			if raw == "" {
				return "", nil
			}
			return " AND " + col + " >= $3", []any{raw}
		}
		_ = filterDate
		// We keep date filters as inline params to keep this MVP readable.
		dfA := ""
		dfB := ""
		dargs := []any{}
		if from != "" {
			dfA = " AND %COL% >= $" + fmt.Sprintf("%d", len(dargs)+3)
			dargs = append(dargs, from)
		}
		if to != "" {
			dfB = " AND %COL% <= $" + fmt.Sprintf("%d", len(dargs)+3)
			dargs = append(dargs, to)
		}
		date := func(col string) string {
			return strings.ReplaceAll(dfA, "%COL%", col) + strings.ReplaceAll(dfB, "%COL%", col)
		}

		if all || allow["journal"] {
			rows, _ := d.Query(`SELECT id, date::text, COALESCE(location_label,''), COALESCE(mood,'')
				FROM journal_entries WHERE user_id=$1 AND (location_label ILIKE $2 OR mood ILIKE $2)`+date("date")+
				` ORDER BY date DESC LIMIT 20`, append([]any{uid, like}, dargs...)...)
			if rows != nil {
				for rows.Next() {
					var id int64
					var date, loc, mood string
					rows.Scan(&id, &date, &loc, &mood)
					excerpt := loc
					if excerpt == "" {
						excerpt = "mood: " + mood
					}
					hits = append(hits, Hit{Type: "journal", ID: id, Date: date, Title: date, Excerpt: excerpt})
				}
				rows.Close()
			}
		}
		if all || allow["memo"] {
			rows, _ := d.Query(`SELECT id, created_at::date::text, content FROM memos
				WHERE user_id=$1 AND content ILIKE $2`+date("created_at::date")+
				` ORDER BY created_at DESC LIMIT 20`, append([]any{uid, like}, dargs...)...)
			if rows != nil {
				for rows.Next() {
					var id int64
					var d, c string
					rows.Scan(&id, &d, &c)
					hits = append(hits, Hit{Type: "memo", ID: id, Date: d, Title: trunc(c, 60), Excerpt: trunc(c, 240)})
				}
				rows.Close()
			}
		}
		if all || allow["note"] {
			rows, _ := d.Query(`SELECT id, updated_at::date::text, title FROM notes
				WHERE user_id=$1 AND title ILIKE $2`+date("updated_at::date")+
				` ORDER BY updated_at DESC LIMIT 20`, append([]any{uid, like}, dargs...)...)
			if rows != nil {
				for rows.Next() {
					var id int64
					var d, t string
					rows.Scan(&id, &d, &t)
					hits = append(hits, Hit{Type: "note", ID: id, Date: d, Title: t, Excerpt: t})
				}
				rows.Close()
			}
		}
		if all || allow["transaction"] {
			rows, _ := d.Query(`SELECT t.id, (t.txn_at AT TIME ZONE 'Asia/Kolkata')::date::text, t.description, a.name FROM fin_transactions t
				JOIN fin_accounts a ON a.id = t.account_id
				WHERE t.user_id=$1 AND t.description ILIKE $2`+date("(t.txn_at AT TIME ZONE 'Asia/Kolkata')::date")+
				` ORDER BY t.txn_at DESC LIMIT 20`, append([]any{uid, like}, dargs...)...)
			if rows != nil {
				for rows.Next() {
					var id int64
					var d, desc, account string
					rows.Scan(&id, &d, &desc, &account)
					hits = append(hits, Hit{Type: "transaction", ID: id, Date: d, Title: desc, Excerpt: account})
				}
				rows.Close()
			}
		}
		if all || allow["media"] {
			rows, _ := d.Query(`SELECT id, updated_at::date::text, title, type FROM media
				WHERE user_id=$1 AND title ILIKE $2`+date("updated_at::date")+
				` ORDER BY updated_at DESC LIMIT 20`, append([]any{uid, like}, dargs...)...)
			if rows != nil {
				for rows.Next() {
					var id int64
					var d, t, mtype string
					rows.Scan(&id, &d, &t, &mtype)
					hits = append(hits, Hit{Type: "media", ID: id, Date: d, Title: t, Excerpt: mtype})
				}
				rows.Close()
			}
		}
		// Sort by date desc.
		for i := 1; i < len(hits); i++ {
			for j := i; j > 0 && hits[j].Date > hits[j-1].Date; j-- {
				hits[j], hits[j-1] = hits[j-1], hits[j]
			}
		}
		limit := 25
		if v := queryParam(r, "limit"); v != "" {
			if n, err := fmt.Sscanf(v, "%d", &limit); err == nil && n > 0 {
				// no-op
			}
		}
		if len(hits) > limit {
			hits = hits[:limit]
		}
		writeJSON(w, 200, map[string]any{"items": hits, "count": len(hits), "query": q})
	}
}

// trunc returns a rune-safe truncation with a trailing ellipsis when cut.
func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// dailyMoodTaskSeries returns [moodScore, completionRate] per day where
// a journal entry with a non-empty mood exists. Mood string maps to a
// 1..5 numeric scale so we can compute Pearson r against the daily task
// completion rate.
func dailyMoodTaskSeries(ctx context.Context, d *db.DB, uid string, cutoff string) ([][2]float64, error) {
	moodMap := map[string]float64{
		"great": 5, "happy": 5, "excited": 5, "energized": 5,
		"good": 4, "focused": 4, "calm": 4,
		"okay": 3, "neutral": 3, "meh": 3,
		"tired": 2, "stressed": 2, "anxious": 2, "sad": 2,
		"awful": 1, "angry": 1, "exhausted": 1,
	}
	rows, err := d.QueryContext(ctx, `
		WITH days AS (
			SELECT date::date AS d, COALESCE(mood,'') AS mood
			FROM journal_entries WHERE user_id=$1 AND date >= $2 AND mood IS NOT NULL AND mood <> ''
		),
		t AS (
			SELECT due_date::date AS d,
			       COUNT(*) AS total,
			       COUNT(*) FILTER (WHERE status='done') AS done
			FROM tasks WHERE user_id=$1 AND due_date >= $2
			GROUP BY due_date)
		SELECT d.d, d.mood, COALESCE(t.done,0), COALESCE(t.total,0)
		FROM days d LEFT JOIN t ON t.d = d.d
		WHERE COALESCE(t.total,0) > 0
		ORDER BY d.d`, uid, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]float64
	for rows.Next() {
		var date time.Time
		var mood string
		var done, total int
		rows.Scan(&date, &mood, &done, &total)
		mscore, ok := moodMap[strings.ToLower(mood)]
		if !ok {
			continue
		}
		out = append(out, [2]float64{mscore, float64(done) / float64(total)})
	}
	return out, nil
}

// pearson returns the Pearson correlation coefficient of {x, y} pairs.
// NaN on degenerate input (zero variance, < 2 points).
func pearson(pts [][2]float64) float64 {
	n := float64(len(pts))
	if n < 2 {
		return math.NaN()
	}
	var sx, sy float64
	for _, p := range pts {
		sx += p[0]
		sy += p[1]
	}
	mx, my := sx/n, sy/n
	var num, dx2, dy2 float64
	for _, p := range pts {
		dx, dy := p[0]-mx, p[1]-my
		num += dx * dy
		dx2 += dx * dx
		dy2 += dy * dy
	}
	if dx2 == 0 || dy2 == 0 {
		return math.NaN()
	}
	return num / math.Sqrt(dx2*dy2)
}

// tryNarrateInsights asks Gemini to rewrite each finding into one warm
// second-person line. Bounded by a 5s context so a slow LLM never
// blocks the daily cron. Returns the same number of entries as the
// input (empty string on parse miss).
func tryNarrateInsights(ctx context.Context, deps Deps, items []detected) []string {
	if deps.AI == nil || len(items) == 0 {
		return nil
	}
	var b strings.Builder
	for i, it := range items {
		fmt.Fprintf(&b, "%d) %s\n   facts: %s\n", i+1, it.title, it.body)
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	text, err := deps.AI.QuickGenerate(cctx, insightSystemPrompt, b.String())
	if err != nil {
		log.Debug().Err(err).Msg("insight narration skipped")
		return nil
	}
	out := make([]string, len(items))
	idx := 0
	for _, ln := range strings.Split(text, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		ln = strings.TrimLeft(ln, "0123456789).-* \t")
		if idx < len(out) {
			out[idx] = ln
			idx++
		}
	}
	return out
}
