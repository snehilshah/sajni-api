package api

import (
	"net/http"
	"time"

	"sajni/internal/db"
)

func registerAnalyticsRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("GET /api/analytics", getAnalytics(deps))
}

func getAnalytics(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		result := map[string]any{}

		// Activity heatmap — last 365 days
		heatmap := []map[string]any{}
		rows, _ := d.Query(`
			SELECT date::text, SUM(cnt)::int as total FROM (
				SELECT created_at::date as date, COUNT(*) as cnt FROM memos WHERE user_id = $1 AND created_at >= NOW() - INTERVAL '365 days' GROUP BY created_at::date
				UNION ALL
				SELECT date as date, 1 as cnt FROM journal_entries WHERE user_id = $1 AND date >= CURRENT_DATE - INTERVAL '365 days'
				UNION ALL
				SELECT updated_at::date as date, COUNT(*) as cnt FROM tasks WHERE user_id = $1 AND status='done' AND updated_at >= NOW() - INTERVAL '365 days' GROUP BY updated_at::date
				UNION ALL
				SELECT logged_date as date, COUNT(*) as cnt FROM habit_logs WHERE user_id = $1 AND logged_date >= CURRENT_DATE - INTERVAL '365 days' GROUP BY logged_date
			) t GROUP BY date ORDER BY date ASC
		`, uid)
		if rows != nil {
			for rows.Next() {
				var date string
				var count int
				rows.Scan(&date, &count)
				heatmap = append(heatmap, map[string]any{"date": date, "count": count})
			}
			rows.Close()
		}
		result["activity_heatmap"] = heatmap

		// Module breakdown — last 30 days
		breakdown := map[string]int{}
		var cnt int
		d.QueryRow("SELECT COUNT(*) FROM memos WHERE user_id = $1 AND created_at >= NOW() - INTERVAL '30 days'", uid).Scan(&cnt)
		breakdown["memos"] = cnt
		d.QueryRow("SELECT COUNT(*) FROM journal_entries WHERE user_id = $1 AND date >= CURRENT_DATE - INTERVAL '30 days'", uid).Scan(&cnt)
		breakdown["journal"] = cnt
		d.QueryRow("SELECT COUNT(*) FROM tasks WHERE user_id = $1 AND created_at >= NOW() - INTERVAL '30 days'", uid).Scan(&cnt)
		breakdown["tasks"] = cnt
		d.QueryRow("SELECT COUNT(*) FROM habit_logs WHERE user_id = $1 AND logged_date >= CURRENT_DATE - INTERVAL '30 days'", uid).Scan(&cnt)
		breakdown["habits"] = cnt
		d.QueryRow("SELECT COUNT(*) FROM media WHERE user_id = $1 AND created_at >= NOW() - INTERVAL '30 days'", uid).Scan(&cnt)
		breakdown["media"] = cnt
		d.QueryRow("SELECT COUNT(*) FROM notes WHERE user_id = $1 AND created_at >= NOW() - INTERVAL '30 days'", uid).Scan(&cnt)
		breakdown["notes"] = cnt
		result["module_breakdown"] = breakdown

		// Habit streaks
		type HabitStreak struct {
			Name    string `json:"name"`
			Current int    `json:"current"`
			Longest int    `json:"longest"`
		}
		var streaks []HabitStreak
		hrows, _ := d.Query("SELECT id, name FROM habits WHERE user_id = $1", uid)
		if hrows != nil {
			for hrows.Next() {
				var id int64
				var name string
				hrows.Scan(&id, &name)
				current := calcStreak(d, uid, id)
				longest := calcLongestStreak(d, uid, id)
				streaks = append(streaks, HabitStreak{Name: name, Current: current, Longest: longest})
			}
			hrows.Close()
		}
		if streaks == nil {
			streaks = []HabitStreak{}
		}
		result["habit_streaks"] = streaks

		// Task velocity — last 8 weeks
		velocity := []map[string]any{}
		vrows, _ := d.Query(`
			SELECT to_char(updated_at, 'IYYY-"W"IW') as week, COUNT(*) as completed
			FROM tasks WHERE user_id = $1 AND status='done' AND updated_at >= NOW() - INTERVAL '56 days'
			GROUP BY week ORDER BY week ASC
		`, uid)
		if vrows != nil {
			for vrows.Next() {
				var week string
				var completed int
				vrows.Scan(&week, &completed)
				velocity = append(velocity, map[string]any{"week": week, "completed": completed})
			}
			vrows.Close()
		}
		result["task_velocity"] = velocity

		// Journal consistency — current month
		var daysLogged int
		d.QueryRow("SELECT COUNT(*) FROM journal_entries WHERE user_id = $1 AND date >= date_trunc('month', CURRENT_DATE)", uid).Scan(&daysLogged)
		dayNow := time.Now().Day()
		pct := 0.0
		if dayNow > 0 {
			pct = float64(daysLogged) / float64(dayNow) * 100
		}
		result["journal_consistency"] = map[string]any{
			"days_logged": daysLogged, "total_days": dayNow, "percentage": pct,
		}

		// Top tags
		type TagStat struct {
			Tag   string `json:"tag"`
			Count int    `json:"count"`
		}
		var topTags []TagStat
		trows, _ := d.Query("SELECT tag, COUNT(*) as cnt FROM tags WHERE user_id = $1 GROUP BY tag ORDER BY cnt DESC LIMIT 20", uid)
		if trows != nil {
			for trows.Next() {
				var t TagStat
				trows.Scan(&t.Tag, &t.Count)
				topTags = append(topTags, t)
			}
			trows.Close()
		}
		if topTags == nil {
			topTags = []TagStat{}
		}
		result["top_tags"] = topTags

		// Media stats
		mediaStats := map[string]int{}
		d.QueryRow("SELECT COUNT(*) FROM media WHERE user_id = $1 AND type='movie' AND status='complete'", uid).Scan(&cnt)
		mediaStats["movies_finished"] = cnt
		d.QueryRow("SELECT COUNT(*) FROM media WHERE user_id = $1 AND type='book' AND status='complete'", uid).Scan(&cnt)
		mediaStats["books_finished"] = cnt
		d.QueryRow("SELECT COUNT(*) FROM media WHERE user_id = $1 AND type='show' AND status='complete'", uid).Scan(&cnt)
		mediaStats["shows_finished"] = cnt
		d.QueryRow("SELECT COUNT(*) FROM media WHERE user_id = $1", uid).Scan(&cnt)
		mediaStats["total"] = cnt
		d.QueryRow("SELECT COUNT(*) FROM media WHERE user_id = $1 AND status='in_progress'", uid).Scan(&cnt)
		mediaStats["in_progress"] = cnt
		result["media_stats"] = mediaStats

		writeJSON(w, 200, result)
	}
}

func calcLongestStreak(d *db.DB, uid, habitID int64) int {
	rows, err := d.Query("SELECT logged_date::text FROM habit_logs WHERE user_id = $1 AND habit_id = $2 ORDER BY logged_date ASC", uid, habitID)
	if err != nil {
		return 0
	}
	defer rows.Close()

	longest := 0
	current := 0
	var prev string
	for rows.Next() {
		var dateStr string
		rows.Scan(&dateStr)
		if prev == "" {
			current = 1
		} else {
			diff := daysDiff(prev, dateStr)
			if diff == 1 {
				current++
			} else {
				current = 1
			}
		}
		if current > longest {
			longest = current
		}
		prev = dateStr
	}
	return longest
}

func daysDiff(a, b string) int {
	const layout = "2006-01-02"
	ta, e1 := time.Parse(layout, a)
	tb, e2 := time.Parse(layout, b)
	if e1 != nil || e2 != nil {
		return -1
	}
	return int(tb.Sub(ta).Hours() / 24)
}
