package api

import (
	"net/http"
	"strconv"
	"time"

	"sajni/internal/db"
)

func registerHabitRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("GET /api/habits", listHabits(deps))
	mux.HandleFunc("GET /api/habits/status", habitStatusForDate(deps))
	mux.HandleFunc("GET /api/habits/logs", recentHabitLogs(deps))
	mux.HandleFunc("POST /api/habits", createHabit(deps))
	mux.HandleFunc("PUT /api/habits/{id}", updateHabit(deps))
	mux.HandleFunc("DELETE /api/habits/{id}", deleteHabit(deps))
	mux.HandleFunc("POST /api/habits/{id}/log", toggleHabitLog(deps))
	mux.HandleFunc("POST /api/habits/{id}/log/{date}", toggleHabitLogForDate(deps))
	mux.HandleFunc("GET /api/habits/{id}/logs", getHabitLogs(deps))
}

func listHabits(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		today := userNow(d, uid).Format("2006-01-02")
		rows, err := d.Query(`
			SELECT h.id, h.name, h.frequency, h.color, h.created_at,
				   EXISTS(SELECT 1 FROM habit_logs hl WHERE hl.habit_id = h.id AND hl.user_id = h.user_id AND hl.logged_date = $1) as logged_today,
				   (SELECT COUNT(*) FROM habit_logs hl WHERE hl.habit_id = h.id AND hl.user_id = h.user_id) as total_logs
			FROM habits h WHERE h.user_id = $2 ORDER BY h.created_at ASC`, today, uid)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()

		type Habit struct {
			ID            int64  `json:"id"`
			Name          string `json:"name"`
			Frequency     string `json:"frequency"`
			Color         string `json:"color"`
			CreatedAt     string `json:"created_at"`
			LoggedToday   bool   `json:"logged_today"`
			TotalLogs     int    `json:"total_logs"`
			CurrentStreak int    `json:"current_streak"`
		}
		var habits []Habit
		for rows.Next() {
			var h Habit
			if err := rows.Scan(&h.ID, &h.Name, &h.Frequency, &h.Color, &h.CreatedAt, &h.LoggedToday, &h.TotalLogs); err != nil {
				errJSON(w, 500, err.Error())
				return
			}
			h.CurrentStreak = calcStreak(d, uid, h.ID)
			habits = append(habits, h)
		}
		if habits == nil {
			habits = []Habit{}
		}
		writeJSON(w, 200, habits)
	}
}

func calcStreak(d *db.DB, uid string, habitID int64) int {
	rows, err := d.Query("SELECT logged_date::text FROM habit_logs WHERE user_id = $1 AND habit_id = $2 ORDER BY logged_date DESC", uid, habitID)
	if err != nil {
		return 0
	}
	defer rows.Close()

	streak := 0
	expected := time.Now()
	first := true
	for rows.Next() {
		var dateStr string
		rows.Scan(&dateStr)
		expDate := expected.Format("2006-01-02")
		if dateStr == expDate {
			streak++
			expected = expected.AddDate(0, 0, -1)
		} else if first && dateStr == expected.AddDate(0, 0, -1).Format("2006-01-02") {
			streak++
			expected = expected.AddDate(0, 0, -2)
		} else {
			break
		}
		first = false
	}
	return streak
}

func createHabit(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var body struct {
			Name      string `json:"name"`
			Frequency string `json:"frequency"`
			Color     string `json:"color"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if body.Frequency == "" {
			body.Frequency = "daily"
		}
		if body.Color == "" {
			body.Color = "#2D5A4F"
		}
		var id int64
		err := d.QueryRow(
			"INSERT INTO habits (user_id, name, frequency, color) VALUES ($1, $2, $3, $4) RETURNING id",
			uid, body.Name, body.Frequency, body.Color,
		).Scan(&id)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 201, map[string]int64{"id": id})
	}
}

func updateHabit(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var body struct {
			Name      *string `json:"name"`
			Frequency *string `json:"frequency"`
			Color     *string `json:"color"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if body.Name != nil {
			d.Exec("UPDATE habits SET name = $1 WHERE id = $2 AND user_id = $3", *body.Name, id, uid)
		}
		if body.Frequency != nil {
			d.Exec("UPDATE habits SET frequency = $1 WHERE id = $2 AND user_id = $3", *body.Frequency, id, uid)
		}
		if body.Color != nil {
			d.Exec("UPDATE habits SET color = $1 WHERE id = $2 AND user_id = $3", *body.Color, id, uid)
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func deleteHabit(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		// habit_logs cascade via FK ON DELETE CASCADE.
		d.Exec("DELETE FROM habits WHERE id = $1 AND user_id = $2", id, uid)
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func toggleHabitLog(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		today := userNow(d, uid).Format("2006-01-02")
		toggleLog(d, uid, id, today, w)
	}
}

func toggleHabitLogForDate(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		date := pathParam(r, "date")
		if date == "" {
			errJSON(w, 400, "date required")
			return
		}
		toggleLog(d, uid, id, date, w)
	}
}

// toggleLog flips today's log row for habit/user; returns logged: bool.
func toggleLog(d *db.DB, uid string, habitID int64, date string, w http.ResponseWriter) {
	// Verify the habit belongs to the user.
	var owns int
	d.QueryRow("SELECT COUNT(*) FROM habits WHERE id = $1 AND user_id = $2", habitID, uid).Scan(&owns)
	if owns == 0 {
		errJSON(w, 404, "habit not found")
		return
	}
	var exists int
	d.QueryRow("SELECT COUNT(*) FROM habit_logs WHERE user_id = $1 AND habit_id = $2 AND logged_date = $3", uid, habitID, date).Scan(&exists)
	if exists > 0 {
		d.Exec("DELETE FROM habit_logs WHERE user_id = $1 AND habit_id = $2 AND logged_date = $3", uid, habitID, date)
		writeJSON(w, 200, map[string]bool{"logged": false})
	} else {
		d.Exec("INSERT INTO habit_logs (user_id, habit_id, logged_date) VALUES ($1, $2, $3)", uid, habitID, date)
		writeJSON(w, 200, map[string]bool{"logged": true})
	}
}

// recentHabitLogs returns logged dates for ALL of the user's habits within the
// last `days` window in a single query, keyed by habit id (as a string so it
// serializes as a JSON object). Replaces the per-habit N+1 the Today page used
// to fire (one /habits/{id}/logs call per habit).
func recentHabitLogs(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		days := queryParam(r, "days")
		if days == "" {
			days = "30"
		}
		rows, err := d.Query(
			`SELECT habit_id, logged_date::text FROM habit_logs
			 WHERE user_id = $1 AND logged_date >= CURRENT_DATE - ($2::int * INTERVAL '1 day')
			 ORDER BY logged_date ASC`,
			uid, days,
		)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()

		out := map[string][]string{}
		for rows.Next() {
			var hid int64
			var dt string
			rows.Scan(&hid, &dt)
			key := strconv.FormatInt(hid, 10)
			out[key] = append(out[key], dt)
		}
		writeJSON(w, 200, out)
	}
}

func getHabitLogs(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		days := queryParam(r, "days")
		if days == "" {
			days = "30"
		}
		rows, err := d.Query(
			"SELECT logged_date::text FROM habit_logs WHERE user_id = $1 AND habit_id = $2 AND logged_date >= CURRENT_DATE - ($3::int * INTERVAL '1 day') ORDER BY logged_date ASC",
			uid, id, days,
		)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()

		var dates []string
		for rows.Next() {
			var d string
			rows.Scan(&d)
			dates = append(dates, d)
		}
		if dates == nil {
			dates = []string{}
		}
		writeJSON(w, 200, dates)
	}
}

func habitStatusForDate(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		date := queryParam(r, "date")
		if date == "" {
			date = userNow(d, uid).Format("2006-01-02")
		}
		rows, err := d.Query(`
			SELECT h.id, h.name, h.frequency, h.color,
				   EXISTS(SELECT 1 FROM habit_logs hl WHERE hl.habit_id = h.id AND hl.user_id = h.user_id AND hl.logged_date = $1) as logged
			FROM habits h WHERE h.user_id = $2 ORDER BY h.created_at ASC`, date, uid)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()

		type HabitStatus struct {
			ID        int64  `json:"id"`
			Name      string `json:"name"`
			Frequency string `json:"frequency"`
			Color     string `json:"color"`
			Logged    bool   `json:"logged"`
		}
		var result []HabitStatus
		for rows.Next() {
			var h HabitStatus
			if err := rows.Scan(&h.ID, &h.Name, &h.Frequency, &h.Color, &h.Logged); err != nil {
				errJSON(w, 500, err.Error())
				return
			}
			result = append(result, h)
		}
		if result == nil {
			result = []HabitStatus{}
		}
		writeJSON(w, 200, result)
	}
}
