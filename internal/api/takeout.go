package api

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"sajni/internal/storage"
)

// registerTakeoutRoutes mounts user data takeout (export/import) and
// account soft-delete endpoints. Export streams a zip; import accepts a
// zip; delete sets users.deleted_at so the data lives for 7 days before
// a background job purges it.
func registerTakeoutRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("GET /api/takeout", takeoutExport(deps))
	mux.HandleFunc("POST /api/takeout/import", takeoutImport(deps))
	mux.HandleFunc("POST /api/account/delete", accountSoftDelete(deps))
	mux.HandleFunc("POST /api/account/cancel-delete", accountCancelDelete(deps))
	mux.HandleFunc("GET /api/account/deletion-status", accountDeletionStatus(deps))
}

// ─── Export ──────────────────────────────────────────────────────────

func takeoutExport(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		stamp := time.Now().UTC().Format("2006-01-02")
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="sajni-takeout-%s.zip"`, stamp))

		zw := zip.NewWriter(w)
		defer zw.Close()

		ctx := r.Context()
		exporters := []func(context.Context, *zip.Writer, Deps, int64) error{
			exportReadme,
			exportMemos,
			exportTasks,
			exportTaskLists,
			exportHabits,
			exportHabitLogs,
			exportMedia,
			exportJournal,
			exportNotes,
			exportTags,
			exportFinanceAccounts,
			exportFinanceCategories,
			exportFinanceTransactions,
			exportFinanceBudgets,
			exportFinanceInvestments,
		}
		for _, fn := range exporters {
			if err := fn(ctx, zw, deps, uid); err != nil {
				// Best-effort — log via Write of an error file rather than
				// 500ing mid-stream (headers already sent).
				writeZipText(zw, "_errors.txt", fmt.Sprintf("%T: %v\n", fn, err))
			}
		}
	}
}

func writeZipText(zw *zip.Writer, name, body string) {
	f, err := zw.Create(name)
	if err != nil {
		return
	}
	io.WriteString(f, body)
}

func exportReadme(_ context.Context, zw *zip.Writer, _ Deps, _ int64) error {
	body := `# Sajni Takeout

This archive contains all your Sajni data.

Layout:
  memos.md                — every memo, separated by '---'
  tasks.csv               — your tasks, including completed ones
  task_lists.csv          — your custom task lists
  habits.csv              — habits and their colours
  habit_logs.csv          — every day you ticked a habit
  media.csv               — your movies/shows/books/games library
  notes/<title>.md        — one file per long-form note
  journal/<date>.md       — one file per journal day
  tags.csv                — tag <-> entity mappings
  finance/                — accounts, categories, transactions, budgets, investments

To restore: POST this same .zip to /api/takeout/import or use the
"Import" button on the Settings page. Best-effort — IDs are remapped.
`
	writeZipText(zw, "README.md", body)
	return nil
}

func exportMemos(ctx context.Context, zw *zip.Writer, deps Deps, uid int64) error {
	rows, err := deps.DB.QueryContext(ctx,
		`SELECT id, content, pinned, created_at FROM memos WHERE user_id = $1 ORDER BY created_at`, uid)
	if err != nil {
		return err
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var id int64
		var content string
		var pinned bool
		var created time.Time
		if err := rows.Scan(&id, &content, &pinned, &created); err != nil {
			continue
		}
		pin := ""
		if pinned {
			pin = " · pinned"
		}
		fmt.Fprintf(&b, "## %s%s\n\n%s\n\n---\n\n", created.Format(time.RFC3339), pin, content)
	}
	writeZipText(zw, "memos.md", b.String())
	return nil
}

func exportTasks(ctx context.Context, zw *zip.Writer, deps Deps, uid int64) error {
	rows, err := deps.DB.QueryContext(ctx, `
		SELECT id, title, COALESCE(description,''), status, priority, due_date,
		       COALESCE(list_id,0), important, created_at, updated_at
		FROM tasks WHERE user_id = $1 ORDER BY created_at`, uid)
	if err != nil {
		return err
	}
	defer rows.Close()
	f, err := zw.Create("tasks.csv")
	if err != nil {
		return err
	}
	cw := csv.NewWriter(f)
	cw.Write([]string{"id", "title", "description", "status", "priority", "due_date", "list_id", "important", "created_at", "updated_at"})
	for rows.Next() {
		var id, listID int64
		var title, desc, status, prio string
		var due sql.NullTime
		var important bool
		var created, updated time.Time
		if err := rows.Scan(&id, &title, &desc, &status, &prio, &due, &listID, &important, &created, &updated); err != nil {
			continue
		}
		cw.Write([]string{
			strconv.FormatInt(id, 10), title, desc, status, prio,
			nullDateStr(due), strconv.FormatInt(listID, 10),
			strconv.FormatBool(important),
			created.Format(time.RFC3339), updated.Format(time.RFC3339),
		})
	}
	cw.Flush()
	return cw.Error()
}

func exportTaskLists(ctx context.Context, zw *zip.Writer, deps Deps, uid int64) error {
	rows, err := deps.DB.QueryContext(ctx, `SELECT id, name, color, icon, sort_order, created_at FROM task_lists WHERE user_id = $1 ORDER BY sort_order`, uid)
	if err != nil {
		return err
	}
	defer rows.Close()
	return writeCSV(zw, "task_lists.csv",
		[]string{"id", "name", "color", "icon", "sort_order", "created_at"},
		func(emit func([]string)) {
			for rows.Next() {
				var id int64
				var name, color, icon string
				var sortOrder int
				var created time.Time
				if err := rows.Scan(&id, &name, &color, &icon, &sortOrder, &created); err == nil {
					emit([]string{strconv.FormatInt(id, 10), name, color, icon, strconv.Itoa(sortOrder), created.Format(time.RFC3339)})
				}
			}
		})
}

func exportHabits(ctx context.Context, zw *zip.Writer, deps Deps, uid int64) error {
	rows, err := deps.DB.QueryContext(ctx,
		`SELECT id, name, frequency, color, created_at FROM habits WHERE user_id = $1 ORDER BY created_at`, uid)
	if err != nil {
		return err
	}
	defer rows.Close()
	return writeCSV(zw, "habits.csv",
		[]string{"id", "name", "frequency", "color", "created_at"},
		func(emit func([]string)) {
			for rows.Next() {
				var id int64
				var name, freq, color string
				var created time.Time
				if err := rows.Scan(&id, &name, &freq, &color, &created); err == nil {
					emit([]string{strconv.FormatInt(id, 10), name, freq, color, created.Format(time.RFC3339)})
				}
			}
		})
}

func exportHabitLogs(ctx context.Context, zw *zip.Writer, deps Deps, uid int64) error {
	rows, err := deps.DB.QueryContext(ctx,
		`SELECT habit_id, logged_date FROM habit_logs WHERE user_id = $1 ORDER BY habit_id, logged_date`, uid)
	if err != nil {
		return err
	}
	defer rows.Close()
	return writeCSV(zw, "habit_logs.csv", []string{"habit_id", "logged_date"}, func(emit func([]string)) {
		for rows.Next() {
			var hid int64
			var d time.Time
			if err := rows.Scan(&hid, &d); err == nil {
				emit([]string{strconv.FormatInt(hid, 10), d.Format("2006-01-02")})
			}
		}
	})
}

func exportMedia(ctx context.Context, zw *zip.Writer, deps Deps, uid int64) error {
	rows, err := deps.DB.QueryContext(ctx, `
		SELECT id, title, type, status, COALESCE(rating,0), notes, platform, poster_url,
		       COALESCE(year,0), genre, external_id, episodes_watched, episodes_total,
		       seasons_watched, seasons_total, collection_id, collection_name,
		       created_at, updated_at
		FROM media WHERE user_id = $1 ORDER BY created_at`, uid)
	if err != nil {
		return err
	}
	defer rows.Close()
	return writeCSV(zw, "media.csv",
		[]string{"id", "title", "type", "status", "rating", "notes", "platform", "poster_url", "year",
			"genre", "external_id", "episodes_watched", "episodes_total", "seasons_watched", "seasons_total",
			"collection_id", "collection_name", "created_at", "updated_at"},
		func(emit func([]string)) {
			for rows.Next() {
				var id int64
				var rating, year, ew, et, sw, st int
				var title, kind, status, notes, platform, poster, genre, ext, colID, colName string
				var created, updated time.Time
				if err := rows.Scan(&id, &title, &kind, &status, &rating, &notes, &platform, &poster,
					&year, &genre, &ext, &ew, &et, &sw, &st, &colID, &colName, &created, &updated); err == nil {
					emit([]string{
						strconv.FormatInt(id, 10), title, kind, status, strconv.Itoa(rating), notes,
						platform, poster, strconv.Itoa(year), genre, ext,
						strconv.Itoa(ew), strconv.Itoa(et), strconv.Itoa(sw), strconv.Itoa(st),
						colID, colName, created.Format(time.RFC3339), updated.Format(time.RFC3339),
					})
				}
			}
		})
}

func exportJournal(ctx context.Context, zw *zip.Writer, deps Deps, uid int64) error {
	rows, err := deps.DB.QueryContext(ctx,
		`SELECT date, blob_key, COALESCE(mood,'') FROM journal_entries WHERE user_id = $1 ORDER BY date`, uid)
	if err != nil {
		return err
	}
	defer rows.Close()
	type row struct {
		date time.Time
		key  string
		mood string
	}
	var entries []row
	for rows.Next() {
		var e row
		if err := rows.Scan(&e.date, &e.key, &e.mood); err == nil {
			entries = append(entries, e)
		}
	}
	for _, e := range entries {
		body := ""
		if e.key != "" {
			data, _, err := deps.Storage.Get(ctx, e.key)
			if err == nil {
				body = string(data)
			}
		}
		fm := fmt.Sprintf("---\ndate: %s\nmood: %s\n---\n\n", e.date.Format("2006-01-02"), e.mood)
		writeZipText(zw, "journal/"+e.date.Format("2006-01-02")+".md", fm+body)
	}
	return nil
}

var safeFileRe = regexp.MustCompile(`[^A-Za-z0-9 _\-.]+`)

func safeName(s string) string {
	s = safeFileRe.ReplaceAllString(s, "_")
	s = strings.TrimSpace(s)
	if len(s) > 80 {
		s = s[:80]
	}
	if s == "" {
		s = "untitled"
	}
	return s
}

func exportNotes(ctx context.Context, zw *zip.Writer, deps Deps, uid int64) error {
	rows, err := deps.DB.QueryContext(ctx,
		`SELECT id, title, blob_key, folder, created_at, updated_at FROM notes WHERE user_id = $1 ORDER BY updated_at DESC`, uid)
	if err != nil {
		return err
	}
	defer rows.Close()
	type row struct {
		id              int64
		title, key, dir string
		created, upd    time.Time
	}
	var notes []row
	for rows.Next() {
		var n row
		if err := rows.Scan(&n.id, &n.title, &n.key, &n.dir, &n.created, &n.upd); err == nil {
			notes = append(notes, n)
		}
	}
	for _, n := range notes {
		body := ""
		if n.key != "" {
			data, _, err := deps.Storage.Get(ctx, n.key)
			if err == nil {
				body = string(data)
			}
		}
		path := "notes/"
		if n.dir != "" {
			path += strings.Trim(n.dir, "/") + "/"
		}
		path += fmt.Sprintf("%d-%s.md", n.id, safeName(n.title))
		fm := fmt.Sprintf("---\nid: %d\ntitle: %s\nfolder: %s\ncreated_at: %s\nupdated_at: %s\n---\n\n",
			n.id, n.title, n.dir, n.created.Format(time.RFC3339), n.upd.Format(time.RFC3339))
		writeZipText(zw, path, fm+body)
	}
	return nil
}

func exportTags(ctx context.Context, zw *zip.Writer, deps Deps, uid int64) error {
	rows, err := deps.DB.QueryContext(ctx,
		`SELECT entity_type, entity_id, tag FROM tags WHERE user_id = $1 ORDER BY entity_type, entity_id`, uid)
	if err != nil {
		return err
	}
	defer rows.Close()
	return writeCSV(zw, "tags.csv", []string{"entity_type", "entity_id", "tag"}, func(emit func([]string)) {
		for rows.Next() {
			var ent string
			var id int64
			var tag string
			if err := rows.Scan(&ent, &id, &tag); err == nil {
				emit([]string{ent, strconv.FormatInt(id, 10), tag})
			}
		}
	})
}

func exportFinanceAccounts(ctx context.Context, zw *zip.Writer, deps Deps, uid int64) error {
	rows, err := deps.DB.QueryContext(ctx, `
		SELECT id, name, type, institution, currency, opening_balance, COALESCE(credit_limit,0),
		       COALESCE(statement_day,0), COALESCE(due_day,0), cashback_type, cashback_value,
		       color, archived, created_at
		FROM fin_accounts WHERE user_id = $1 ORDER BY id`, uid)
	if err != nil {
		return err
	}
	defer rows.Close()
	return writeCSV(zw, "finance/accounts.csv",
		[]string{"id", "name", "type", "institution", "currency", "opening_balance", "credit_limit",
			"statement_day", "due_day", "cashback_type", "cashback_value", "color", "archived", "created_at"},
		func(emit func([]string)) {
			for rows.Next() {
				var id int64
				var name, kind, inst, ccy, cbType, color string
				var opening, climit, cbVal float64
				var stmtDay, dueDay int
				var archived bool
				var created time.Time
				if err := rows.Scan(&id, &name, &kind, &inst, &ccy, &opening, &climit, &stmtDay, &dueDay,
					&cbType, &cbVal, &color, &archived, &created); err == nil {
					emit([]string{
						strconv.FormatInt(id, 10), name, kind, inst, ccy,
						strconv.FormatFloat(opening, 'f', 2, 64),
						strconv.FormatFloat(climit, 'f', 2, 64),
						strconv.Itoa(stmtDay), strconv.Itoa(dueDay),
						cbType, strconv.FormatFloat(cbVal, 'f', 2, 64),
						color, strconv.FormatBool(archived), created.Format(time.RFC3339),
					})
				}
			}
		})
}

func exportFinanceCategories(ctx context.Context, zw *zip.Writer, deps Deps, uid int64) error {
	rows, err := deps.DB.QueryContext(ctx, `SELECT id, name, kind, color, icon FROM fin_categories WHERE user_id = $1 ORDER BY id`, uid)
	if err != nil {
		return err
	}
	defer rows.Close()
	return writeCSV(zw, "finance/categories.csv",
		[]string{"id", "name", "kind", "color", "icon"},
		func(emit func([]string)) {
			for rows.Next() {
				var id int64
				var name, kind, color, icon string
				if err := rows.Scan(&id, &name, &kind, &color, &icon); err == nil {
					emit([]string{strconv.FormatInt(id, 10), name, kind, color, icon})
				}
			}
		})
}

func exportFinanceTransactions(ctx context.Context, zw *zip.Writer, deps Deps, uid int64) error {
	rows, err := deps.DB.QueryContext(ctx, `
		SELECT id, account_id, COALESCE(category_id,0), type, amount, description, txn_date,
		       COALESCE(linked_account,0), created_at
		FROM fin_transactions WHERE user_id = $1 ORDER BY txn_date`, uid)
	if err != nil {
		return err
	}
	defer rows.Close()
	return writeCSV(zw, "finance/transactions.csv",
		[]string{"id", "account_id", "category_id", "type", "amount", "description", "txn_date", "linked_account", "created_at"},
		func(emit func([]string)) {
			for rows.Next() {
				var id, acct, cat, linked int64
				var kind, desc string
				var amt float64
				var date time.Time
				var created time.Time
				if err := rows.Scan(&id, &acct, &cat, &kind, &amt, &desc, &date, &linked, &created); err == nil {
					emit([]string{
						strconv.FormatInt(id, 10), strconv.FormatInt(acct, 10),
						strconv.FormatInt(cat, 10), kind, strconv.FormatFloat(amt, 'f', 2, 64),
						desc, date.Format("2006-01-02"), strconv.FormatInt(linked, 10),
						created.Format(time.RFC3339),
					})
				}
			}
		})
}

func exportFinanceBudgets(ctx context.Context, zw *zip.Writer, deps Deps, uid int64) error {
	rows, err := deps.DB.QueryContext(ctx, `SELECT id, name, period, start_date, end_date, total_amount FROM fin_budgets WHERE user_id = $1 ORDER BY start_date`, uid)
	if err != nil {
		return err
	}
	defer rows.Close()
	return writeCSV(zw, "finance/budgets.csv",
		[]string{"id", "name", "period", "start_date", "end_date", "total_amount"},
		func(emit func([]string)) {
			for rows.Next() {
				var id int64
				var name, period string
				var start, end time.Time
				var total float64
				if err := rows.Scan(&id, &name, &period, &start, &end, &total); err == nil {
					emit([]string{
						strconv.FormatInt(id, 10), name, period,
						start.Format("2006-01-02"), end.Format("2006-01-02"),
						strconv.FormatFloat(total, 'f', 2, 64),
					})
				}
			}
		})
}

func exportFinanceInvestments(ctx context.Context, zw *zip.Writer, deps Deps, uid int64) error {
	rows, err := deps.DB.QueryContext(ctx, `
		SELECT id, name, type, COALESCE(account_id,0), invested_amount
		FROM fin_investments WHERE user_id = $1 ORDER BY id`, uid)
	if err != nil {
		return err
	}
	defer rows.Close()
	return writeCSV(zw, "finance/investments.csv",
		[]string{"id", "name", "type", "account_id", "invested_amount"},
		func(emit func([]string)) {
			for rows.Next() {
				var id, acct int64
				var name, kind string
				var amt float64
				if err := rows.Scan(&id, &name, &kind, &acct, &amt); err == nil {
					emit([]string{
						strconv.FormatInt(id, 10), name, kind,
						strconv.FormatInt(acct, 10),
						strconv.FormatFloat(amt, 'f', 2, 64),
					})
				}
			}
		})
}

// writeCSV streams CSV rows into the zip. emit is called once per row.
func writeCSV(zw *zip.Writer, name string, header []string, fn func(emit func([]string))) error {
	f, err := zw.Create(name)
	if err != nil {
		return err
	}
	cw := csv.NewWriter(f)
	cw.Write(header)
	fn(func(rec []string) { cw.Write(rec) })
	cw.Flush()
	return cw.Error()
}

func nullDateStr(d sql.NullTime) string {
	if !d.Valid {
		return ""
	}
	return d.Time.Format("2006-01-02")
}

// ─── Import ──────────────────────────────────────────────────────────

// takeoutImport accepts a zip generated by /api/takeout and re-inserts
// rows. IDs are NOT preserved — the import treats the archive as a set
// of new records merged into the user's account. Tasks, habits, media,
// memos, notes, journal, and finance basics are supported. Tags and
// foreign-key links between rows are best-effort (mapped via id-rewrite).
func takeoutImport(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			errJSON(w, 400, "form too large or invalid")
			return
		}
		f, hdr, err := r.FormFile("file")
		if err != nil {
			errJSON(w, 400, "missing 'file' field")
			return
		}
		defer f.Close()

		zr, err := zip.NewReader(f, hdr.Size)
		if err != nil {
			errJSON(w, 400, "not a valid zip")
			return
		}

		counts := map[string]int{}
		// First pass: read all entries into a name->bytes map (most exports
		// are well under 100MB; simpler than two-pass streaming).
		entries := map[string][]byte{}
		for _, ze := range zr.File {
			if ze.FileInfo().IsDir() {
				continue
			}
			rc, err := ze.Open()
			if err != nil {
				continue
			}
			data, _ := io.ReadAll(rc)
			rc.Close()
			entries[ze.Name] = data
		}

		ctx := r.Context()

		if body, ok := entries["memos.md"]; ok {
			counts["memos"] = importMemos(ctx, deps, uid, body)
		}
		if body, ok := entries["tasks.csv"]; ok {
			counts["tasks"] = importTasks(ctx, deps, uid, body)
		}
		if body, ok := entries["habits.csv"]; ok {
			n, idMap := importHabits(ctx, deps, uid, body)
			counts["habits"] = n
			if body2, ok := entries["habit_logs.csv"]; ok {
				counts["habit_logs"] = importHabitLogs(ctx, deps, uid, body2, idMap)
			}
		}
		if body, ok := entries["media.csv"]; ok {
			counts["media"] = importMedia(ctx, deps, uid, body)
		}
		if body, ok := entries["finance/accounts.csv"]; ok {
			n, idMap := importFinAccounts(ctx, deps, uid, body)
			counts["fin_accounts"] = n
			var catMap map[int64]int64
			if body2, ok := entries["finance/categories.csv"]; ok {
				var cn int
				cn, catMap = importFinCategories(ctx, deps, uid, body2)
				counts["fin_categories"] = cn
			}
			if body3, ok := entries["finance/transactions.csv"]; ok {
				counts["fin_transactions"] = importFinTransactions(ctx, deps, uid, body3, idMap, catMap)
			}
		}
		// Notes — every file under notes/*.md.
		for name, body := range entries {
			if strings.HasPrefix(name, "notes/") && strings.HasSuffix(name, ".md") {
				if importNote(ctx, deps, uid, body) {
					counts["notes"]++
				}
			}
			if strings.HasPrefix(name, "journal/") && strings.HasSuffix(name, ".md") {
				if importJournal(ctx, deps, uid, name, body) {
					counts["journal"]++
				}
			}
		}

		writeJSON(w, 200, map[string]any{"imported": counts})
	}
}

func importMemos(ctx context.Context, deps Deps, uid int64, body []byte) int {
	parts := strings.Split(string(body), "\n---\n")
	n := 0
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// Strip leading "## ..." header line we wrote on export.
		if strings.HasPrefix(p, "## ") {
			if i := strings.Index(p, "\n"); i >= 0 {
				p = strings.TrimSpace(p[i+1:])
			}
		}
		if p == "" {
			continue
		}
		_, err := deps.DB.ExecContext(ctx, `INSERT INTO memos(user_id, content) VALUES ($1,$2)`, uid, p)
		if err == nil {
			n++
		}
	}
	return n
}

func importTasks(ctx context.Context, deps Deps, uid int64, body []byte) int {
	rows, err := parseCSV(body)
	if err != nil {
		return 0
	}
	n := 0
	for _, r := range rows {
		// header: id,title,description,status,priority,due_date,list_id,important,created_at,updated_at
		if len(r) < 8 {
			continue
		}
		title := r[1]
		if title == "" {
			continue
		}
		var due any = nil
		if r[5] != "" {
			due = r[5]
		}
		important := strings.EqualFold(r[7], "true")
		_, err := deps.DB.ExecContext(ctx, `
			INSERT INTO tasks(user_id, title, description, status, priority, due_date, important)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			uid, title, r[2], r[3], r[4], due, important)
		if err == nil {
			n++
		}
	}
	return n
}

func importHabits(ctx context.Context, deps Deps, uid int64, body []byte) (int, map[int64]int64) {
	idMap := map[int64]int64{}
	rows, err := parseCSV(body)
	if err != nil {
		return 0, idMap
	}
	n := 0
	for _, r := range rows {
		if len(r) < 4 {
			continue
		}
		oldID, _ := strconv.ParseInt(r[0], 10, 64)
		var newID int64
		err := deps.DB.QueryRowContext(ctx, `
			INSERT INTO habits(user_id, name, frequency, color) VALUES ($1,$2,$3,$4) RETURNING id`,
			uid, r[1], r[2], r[3]).Scan(&newID)
		if err == nil {
			idMap[oldID] = newID
			n++
		}
	}
	return n, idMap
}

func importHabitLogs(ctx context.Context, deps Deps, uid int64, body []byte, idMap map[int64]int64) int {
	rows, err := parseCSV(body)
	if err != nil {
		return 0
	}
	n := 0
	for _, r := range rows {
		if len(r) < 2 {
			continue
		}
		oldID, _ := strconv.ParseInt(r[0], 10, 64)
		newID, ok := idMap[oldID]
		if !ok {
			continue
		}
		_, err := deps.DB.ExecContext(ctx, `
			INSERT INTO habit_logs(user_id, habit_id, logged_date) VALUES ($1,$2,$3)
			ON CONFLICT (user_id, habit_id, logged_date) DO NOTHING`, uid, newID, r[1])
		if err == nil {
			n++
		}
	}
	return n
}

func importMedia(ctx context.Context, deps Deps, uid int64, body []byte) int {
	rows, err := parseCSV(body)
	if err != nil {
		return 0
	}
	n := 0
	for _, r := range rows {
		// id,title,type,status,rating,notes,platform,poster_url,year,genre,external_id,
		// episodes_watched,episodes_total,seasons_watched,seasons_total,collection_id,collection_name,created_at,updated_at
		if len(r) < 17 {
			continue
		}
		rating, _ := strconv.Atoi(r[4])
		year, _ := strconv.Atoi(r[8])
		ew, _ := strconv.Atoi(r[11])
		et, _ := strconv.Atoi(r[12])
		sw, _ := strconv.Atoi(r[13])
		st, _ := strconv.Atoi(r[14])
		var ratingArg any = rating
		if rating == 0 {
			ratingArg = nil
		}
		var yearArg any = year
		if year == 0 {
			yearArg = nil
		}
		_, err := deps.DB.ExecContext(ctx, `
			INSERT INTO media(user_id, title, type, status, rating, notes, platform, poster_url,
			                  year, genre, external_id, episodes_watched, episodes_total,
			                  seasons_watched, seasons_total, collection_id, collection_name)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
			uid, r[1], r[2], r[3], ratingArg, r[5], r[6], r[7],
			yearArg, r[9], r[10], ew, et, sw, st, r[15], r[16])
		if err == nil {
			n++
		}
	}
	return n
}

func importFinAccounts(ctx context.Context, deps Deps, uid int64, body []byte) (int, map[int64]int64) {
	idMap := map[int64]int64{}
	rows, err := parseCSV(body)
	if err != nil {
		return 0, idMap
	}
	n := 0
	for _, r := range rows {
		// id,name,type,institution,currency,opening_balance,credit_limit,statement_day,due_day,
		// cashback_type,cashback_value,color,archived,created_at
		if len(r) < 13 {
			continue
		}
		oldID, _ := strconv.ParseInt(r[0], 10, 64)
		opening, _ := strconv.ParseFloat(r[5], 64)
		climit, _ := strconv.ParseFloat(r[6], 64)
		stmtDay, _ := strconv.Atoi(r[7])
		dueDay, _ := strconv.Atoi(r[8])
		cbVal, _ := strconv.ParseFloat(r[10], 64)
		archived := strings.EqualFold(r[12], "true")
		var newID int64
		err := deps.DB.QueryRowContext(ctx, `
			INSERT INTO fin_accounts(user_id, name, type, institution, currency, opening_balance,
			                         credit_limit, statement_day, due_day, cashback_type, cashback_value,
			                         color, archived)
			VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,0),NULLIF($8,0),NULLIF($9,0),$10,$11,$12,$13)
			RETURNING id`,
			uid, r[1], r[2], r[3], r[4], opening, climit, stmtDay, dueDay,
			r[9], cbVal, r[11], archived).Scan(&newID)
		if err == nil {
			idMap[oldID] = newID
			n++
		}
	}
	return n, idMap
}

func importFinCategories(ctx context.Context, deps Deps, uid int64, body []byte) (int, map[int64]int64) {
	idMap := map[int64]int64{}
	rows, err := parseCSV(body)
	if err != nil {
		return 0, idMap
	}
	n := 0
	for _, r := range rows {
		if len(r) < 5 {
			continue
		}
		oldID, _ := strconv.ParseInt(r[0], 10, 64)
		var newID int64
		err := deps.DB.QueryRowContext(ctx, `
			INSERT INTO fin_categories(user_id, name, kind, color, icon)
			VALUES ($1,$2,$3,$4,$5) RETURNING id`,
			uid, r[1], r[2], r[3], r[4]).Scan(&newID)
		if err == nil {
			idMap[oldID] = newID
			n++
		}
	}
	return n, idMap
}

func importFinTransactions(ctx context.Context, deps Deps, uid int64, body []byte, acctMap, catMap map[int64]int64) int {
	rows, err := parseCSV(body)
	if err != nil {
		return 0
	}
	n := 0
	for _, r := range rows {
		// id,account_id,category_id,type,amount,description,txn_date,linked_account,created_at
		if len(r) < 8 {
			continue
		}
		oldAcct, _ := strconv.ParseInt(r[1], 10, 64)
		newAcct, ok := acctMap[oldAcct]
		if !ok {
			continue
		}
		var catArg any = nil
		if cat, _ := strconv.ParseInt(r[2], 10, 64); cat != 0 {
			if mapped, ok := catMap[cat]; ok {
				catArg = mapped
			}
		}
		var linkedArg any = nil
		if linked, _ := strconv.ParseInt(r[7], 10, 64); linked != 0 {
			if mapped, ok := acctMap[linked]; ok {
				linkedArg = mapped
			}
		}
		amt, _ := strconv.ParseFloat(r[4], 64)
		_, err := deps.DB.ExecContext(ctx, `
			INSERT INTO fin_transactions(user_id, account_id, category_id, type, amount, description, txn_date, linked_account)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			uid, newAcct, catArg, r[3], amt, r[5], r[6], linkedArg)
		if err == nil {
			n++
		}
	}
	return n
}

var noteFrontRe = regexp.MustCompile(`(?s)^---\n(.*?)\n---\n+`)

func importNote(ctx context.Context, deps Deps, uid int64, body []byte) bool {
	s := string(body)
	title := ""
	folder := ""
	if m := noteFrontRe.FindStringSubmatch(s); m != nil {
		for _, line := range strings.Split(m[1], "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "title:") {
				title = strings.TrimSpace(strings.TrimPrefix(line, "title:"))
			}
			if strings.HasPrefix(line, "folder:") {
				folder = strings.TrimSpace(strings.TrimPrefix(line, "folder:"))
			}
		}
		s = strings.TrimPrefix(s, m[0])
	}
	if title == "" {
		title = "Imported note"
	}
	key := storage.UserKey(uid, "notes", fmt.Sprintf("%d.md", time.Now().UnixNano()))
	if err := deps.Storage.Put(ctx, key, []byte(s), "text/markdown"); err != nil {
		return false
	}
	_, err := deps.DB.ExecContext(ctx,
		`INSERT INTO notes(user_id, title, blob_key, folder) VALUES ($1,$2,$3,$4)`,
		uid, title, key, folder)
	return err == nil
}

func importJournal(ctx context.Context, deps Deps, uid int64, name string, body []byte) bool {
	// Extract YYYY-MM-DD from filename `journal/<date>.md`.
	base := strings.TrimSuffix(strings.TrimPrefix(name, "journal/"), ".md")
	if _, err := time.Parse("2006-01-02", base); err != nil {
		return false
	}
	s := string(body)
	mood := ""
	if m := noteFrontRe.FindStringSubmatch(s); m != nil {
		for _, line := range strings.Split(m[1], "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "mood:") {
				mood = strings.TrimSpace(strings.TrimPrefix(line, "mood:"))
			}
		}
		s = strings.TrimPrefix(s, m[0])
	}
	key := storage.UserKey(uid, "journal", base+".md")
	if err := deps.Storage.Put(ctx, key, []byte(s), "text/markdown"); err != nil {
		return false
	}
	var moodArg any = nil
	if mood != "" {
		moodArg = mood
	}
	_, err := deps.DB.ExecContext(ctx, `
		INSERT INTO journal_entries(user_id, date, blob_key, mood)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, date) DO UPDATE SET blob_key = EXCLUDED.blob_key, mood = EXCLUDED.mood, updated_at = NOW()`,
		uid, base, key, moodArg)
	return err == nil
}

func parseCSV(body []byte) ([][]string, error) {
	rd := csv.NewReader(strings.NewReader(string(body)))
	rd.FieldsPerRecord = -1
	all, err := rd.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(all) <= 1 {
		return nil, nil
	}
	return all[1:], nil
}

// ─── Account soft-delete ─────────────────────────────────────────────

const accountDeleteGrace = 7 * 24 * time.Hour

func accountSoftDelete(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		_, err := deps.DB.Exec(`UPDATE users SET deleted_at = NOW() WHERE id = $1 AND deleted_at IS NULL`, uid)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{
			"status":         "scheduled",
			"purge_after":    time.Now().Add(accountDeleteGrace).UTC().Format(time.RFC3339),
			"grace_duration": accountDeleteGrace.String(),
		})
	}
}

func accountCancelDelete(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		_, err := deps.DB.Exec(`UPDATE users SET deleted_at = NULL WHERE id = $1`, uid)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "cancelled"})
	}
}

func accountDeletionStatus(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var deletedAt sql.NullTime
		if err := deps.DB.QueryRow(`SELECT deleted_at FROM users WHERE id = $1`, uid).Scan(&deletedAt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				errJSON(w, 404, "user not found")
				return
			}
			errJSON(w, 500, err.Error())
			return
		}
		if !deletedAt.Valid {
			writeJSON(w, 200, map[string]any{"scheduled": false})
			return
		}
		purge := deletedAt.Time.Add(accountDeleteGrace)
		writeJSON(w, 200, map[string]any{
			"scheduled":   true,
			"deleted_at":  deletedAt.Time.UTC().Format(time.RFC3339),
			"purge_after": purge.UTC().Format(time.RFC3339),
		})
	}
}

// PurgeExpiredDeletedUsers wipes users whose deleted_at is older than the
// grace window. ON DELETE CASCADE on every per-user table handles the
// associated content. Call this from a background ticker in cmd/main.
func PurgeExpiredDeletedUsers(ctx context.Context, deps Deps) (int64, error) {
	res, err := deps.DB.ExecContext(ctx,
		`DELETE FROM users WHERE deleted_at IS NOT NULL AND deleted_at < NOW() - INTERVAL '7 days'`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
