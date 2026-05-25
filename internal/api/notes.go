package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"sajni/internal/storage"
)

func registerNoteRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("GET /api/notes/folders", listFolders(deps))
	mux.HandleFunc("POST /api/notes/folders", createFolder(deps))
	mux.HandleFunc("DELETE /api/notes/folders", deleteFolder(deps))
	mux.HandleFunc("POST /api/notes/folders/rename", renameFolder(deps))

	mux.HandleFunc("GET /api/notes", listNotes(deps))
	mux.HandleFunc("GET /api/notes/{id}", getNote(deps))
	mux.HandleFunc("POST /api/notes", createNote(deps))
	mux.HandleFunc("PUT /api/notes/{id}", updateNote(deps))
	mux.HandleFunc("DELETE /api/notes/{id}", deleteNote(deps))
}

func normalizeFolder(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	parts := strings.Split(p, "/")
	out := make([]string, 0, len(parts))
	for _, seg := range parts {
		seg = strings.TrimSpace(seg)
		if seg == "" || seg == "." || seg == ".." {
			continue
		}
		seg = strings.ReplaceAll(seg, "\\", "_")
		out = append(out, seg)
	}
	return strings.Join(out, "/")
}

func safeFileName(title string) string {
	safe := strings.ReplaceAll(title, "/", "_")
	safe = strings.ReplaceAll(safe, "\\", "_")
	if safe == "" {
		safe = "untitled"
	}
	return safe
}

// noteKey produces the storage object key for a given note.
func noteKey(uid string, folder, title string) string {
	parts := []string{"notes"}
	if folder != "" {
		parts = append(parts, folder)
	}
	parts = append(parts, safeFileName(title)+".md")
	return storage.UserKey(uid, parts...)
}

func listNotes(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())

		args := []any{uid}
		clauses := []string{"n.user_id = $1"}
		ph := 2
		base := "SELECT n.id, n.title, n.folder, n.created_at, n.updated_at FROM notes n"

		if s := queryParam(r, "search"); s != "" {
			clauses = append(clauses, "n.title ILIKE $"+itoa(ph))
			args = append(args, "%"+s+"%")
			ph++
		}
		if tag := queryParam(r, "tag"); tag != "" {
			base = "SELECT n.id, n.title, n.folder, n.created_at, n.updated_at FROM notes n INNER JOIN tags t ON t.user_id = n.user_id AND t.entity_type = 'note' AND t.entity_id = n.id"
			clauses = append(clauses, "t.tag = $"+itoa(ph))
			args = append(args, tag)
			ph++
		}
		if folder := queryParam(r, "folder"); folder != "" {
			clauses = append(clauses, "n.folder = $"+itoa(ph))
			args = append(args, normalizeFolder(folder))
			ph++
		}

		q := base + " WHERE " + strings.Join(clauses, " AND ") + " ORDER BY n.updated_at DESC"
		rows, err := d.Query(q, args...)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()

		type Note struct {
			ID        int64    `json:"id"`
			Title     string   `json:"title"`
			Folder    string   `json:"folder"`
			Tags      []string `json:"tags"`
			CreatedAt string   `json:"created_at"`
			UpdatedAt string   `json:"updated_at"`
		}
		var notes []Note
		for rows.Next() {
			var n Note
			if err := rows.Scan(&n.ID, &n.Title, &n.Folder, &n.CreatedAt, &n.UpdatedAt); err != nil {
				errJSON(w, 500, err.Error())
				return
			}
			tagRows, err := d.Query("SELECT tag FROM tags WHERE user_id = $1 AND entity_type = 'note' AND entity_id = $2", uid, n.ID)
			if err == nil {
				for tagRows.Next() {
					var t string
					tagRows.Scan(&t)
					n.Tags = append(n.Tags, t)
				}
				tagRows.Close()
			}
			if n.Tags == nil {
				n.Tags = []string{}
			}
			notes = append(notes, n)
		}
		if notes == nil {
			notes = []Note{}
		}
		writeJSON(w, 200, notes)
	}
}

func getNote(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}

		type Note struct {
			ID        int64          `json:"id"`
			Title     string         `json:"title"`
			Folder    string         `json:"folder"`
			Content   string         `json:"content"`
			Tags      []string       `json:"tags"`
			Backlinks []BacklinkInfo `json:"backlinks"`
			CreatedAt string         `json:"created_at"`
			UpdatedAt string         `json:"updated_at"`
		}

		var n Note
		var blobKey string
		err = d.QueryRow("SELECT id, title, folder, blob_key, created_at, updated_at FROM notes WHERE id = $1 AND user_id = $2", id, uid).
			Scan(&n.ID, &n.Title, &n.Folder, &blobKey, &n.CreatedAt, &n.UpdatedAt)
		if err != nil {
			errJSON(w, 404, "not found")
			return
		}

		if blobKey != "" {
			if data, _, gerr := deps.Storage.Get(r.Context(), blobKey); gerr == nil {
				n.Content = string(data)
			}
		}

		tagRows, _ := d.Query("SELECT tag FROM tags WHERE user_id = $1 AND entity_type = 'note' AND entity_id = $2", uid, n.ID)
		if tagRows != nil {
			for tagRows.Next() {
				var t string
				tagRows.Scan(&t)
				n.Tags = append(n.Tags, t)
			}
			tagRows.Close()
		}
		if n.Tags == nil {
			n.Tags = []string{}
		}

		n.Backlinks = getIncomingBacklinks(d, uid, "note", n.ID)
		writeJSON(w, 200, n)
	}
}

func createNote(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var body struct {
			Title   string `json:"title"`
			Content string `json:"content"`
			Folder  string `json:"folder"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}

		folder := normalizeFolder(body.Folder)
		key := noteKey(uid, folder, body.Title)

		if err := deps.Storage.Put(r.Context(), key, []byte(body.Content), "text/markdown"); err != nil {
			errJSON(w, 500, "store note: "+err.Error())
			return
		}

		var id int64
		err := d.QueryRow(
			"INSERT INTO notes (user_id, title, blob_key, folder) VALUES ($1, $2, $3, $4) RETURNING id",
			uid, body.Title, key, folder,
		).Scan(&id)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		syncTags(d, uid, "note", id, body.Content)
		syncBacklinks(d, uid, "note", id, body.Content)
		writeJSON(w, 201, map[string]any{"id": id, "folder": folder})
	}
}

func updateNote(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}

		var body struct {
			Title   *string `json:"title"`
			Content *string `json:"content"`
			Folder  *string `json:"folder"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}

		var (
			currentTitle  string
			currentFolder string
			blobKey       string
		)
		err = d.QueryRow("SELECT title, folder, blob_key FROM notes WHERE id = $1 AND user_id = $2", id, uid).Scan(&currentTitle, &currentFolder, &blobKey)
		if err != nil {
			errJSON(w, 404, "not found")
			return
		}

		nextTitle := currentTitle
		if body.Title != nil {
			nextTitle = *body.Title
		}
		nextFolder := currentFolder
		if body.Folder != nil {
			nextFolder = normalizeFolder(*body.Folder)
		}

		if nextTitle != currentTitle || nextFolder != currentFolder {
			newKey := noteKey(uid, nextFolder, nextTitle)
			if newKey != blobKey {
				// "Move" by re-uploading content under the new key, then deleting the old one.
				var content []byte
				if blobKey != "" {
					if data, _, gerr := deps.Storage.Get(r.Context(), blobKey); gerr == nil {
						content = data
					}
				}
				if err := deps.Storage.Put(r.Context(), newKey, content, "text/markdown"); err != nil {
					errJSON(w, 500, "rename note: "+err.Error())
					return
				}
				if blobKey != "" {
					_ = deps.Storage.Delete(r.Context(), blobKey)
				}
				blobKey = newKey
			}
			d.Exec(
				"UPDATE notes SET title = $1, folder = $2, blob_key = $3, updated_at = NOW() WHERE id = $4 AND user_id = $5",
				nextTitle, nextFolder, blobKey, id, uid,
			)
		}

		if body.Content != nil {
			if blobKey == "" {
				errJSON(w, 500, "no blob key")
				return
			}
			if err := deps.Storage.Put(r.Context(), blobKey, []byte(*body.Content), "text/markdown"); err != nil {
				errJSON(w, 500, fmt.Sprintf("store note: %v", err))
				return
			}
			d.Exec("UPDATE notes SET updated_at = NOW() WHERE id = $1 AND user_id = $2", id, uid)
			syncTags(d, uid, "note", id, *body.Content)
			syncBacklinks(d, uid, "note", id, *body.Content)
		}

		writeJSON(w, 200, map[string]any{"status": "ok", "folder": nextFolder, "title": nextTitle})
	}
}

func deleteNote(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}

		var blobKey string
		d.QueryRow("SELECT blob_key FROM notes WHERE id = $1 AND user_id = $2", id, uid).Scan(&blobKey)
		if blobKey != "" {
			if err := deps.Storage.Delete(r.Context(), blobKey); err != nil && !errors.Is(err, storage.ErrNotFound) {
				// best-effort
			}
		}

		d.Exec("DELETE FROM tags WHERE user_id = $1 AND entity_type = 'note' AND entity_id = $2", uid, id)
		d.Exec("DELETE FROM backlinks WHERE user_id = $1 AND source_type = 'note' AND source_id = $2", uid, id)
		d.Exec("DELETE FROM notes WHERE id = $1 AND user_id = $2", id, uid)
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

/* ---------- Folders ---------- */

func listFolders(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		seen := map[string]struct{}{}
		var folders []string

		rows, err := d.Query("SELECT DISTINCT folder FROM notes WHERE user_id = $1 AND folder != ''", uid)
		if err == nil {
			for rows.Next() {
				var f string
				rows.Scan(&f)
				if _, dup := seen[f]; !dup {
					seen[f] = struct{}{}
					folders = append(folders, f)
				}
			}
			rows.Close()
		}

		rows2, err := d.Query("SELECT path FROM note_folders WHERE user_id = $1", uid)
		if err == nil {
			for rows2.Next() {
				var p string
				rows2.Scan(&p)
				if _, dup := seen[p]; !dup {
					seen[p] = struct{}{}
					folders = append(folders, p)
				}
			}
			rows2.Close()
		}

		// Synthesize parents.
		for f := range seen {
			parts := strings.Split(f, "/")
			for i := 1; i < len(parts); i++ {
				p := strings.Join(parts[:i], "/")
				if _, dup := seen[p]; !dup {
					seen[p] = struct{}{}
					folders = append(folders, p)
				}
			}
		}

		// Sort lexicographically.
		for i := 1; i < len(folders); i++ {
			for j := i; j > 0 && folders[j-1] > folders[j]; j-- {
				folders[j-1], folders[j] = folders[j], folders[j-1]
			}
		}

		if folders == nil {
			folders = []string{}
		}
		writeJSON(w, 200, folders)
	}
}

func createFolder(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var body struct {
			Path string `json:"path"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		path := normalizeFolder(body.Path)
		if path == "" {
			errJSON(w, 400, "empty path")
			return
		}
		d.Exec("INSERT INTO note_folders (user_id, path) VALUES ($1, $2) ON CONFLICT DO NOTHING", uid, path)
		writeJSON(w, 201, map[string]string{"path": path})
	}
}

func deleteFolder(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var body struct {
			Path string `json:"path"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		path := normalizeFolder(body.Path)
		if path == "" {
			errJSON(w, 400, "empty path")
			return
		}

		var cnt int
		d.QueryRow("SELECT COUNT(*) FROM notes WHERE user_id = $1 AND (folder = $2 OR folder LIKE $3)", uid, path, path+"/%").Scan(&cnt)
		if cnt > 0 {
			errJSON(w, 409, fmt.Sprintf("folder not empty: %d notes inside", cnt))
			return
		}
		d.Exec("DELETE FROM note_folders WHERE user_id = $1 AND (path = $2 OR path LIKE $3)", uid, path, path+"/%")
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func renameFolder(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var body struct {
			From string `json:"from"`
			To   string `json:"to"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		from := normalizeFolder(body.From)
		to := normalizeFolder(body.To)
		if from == "" || to == "" || from == to {
			errJSON(w, 400, "invalid path")
			return
		}

		// Move notes in DB (folder field).
		d.Exec("UPDATE notes SET folder = $1 WHERE user_id = $2 AND folder = $3", to, uid, from)
		d.Exec(
			"UPDATE notes SET folder = $1 || SUBSTRING(folder FROM $2) WHERE user_id = $3 AND folder LIKE $4",
			to+"/", len(from)+2, uid, from+"/%",
		)
		// Move folder records.
		d.Exec("UPDATE note_folders SET path = $1 WHERE user_id = $2 AND path = $3", to, uid, from)
		d.Exec(
			"UPDATE note_folders SET path = $1 || SUBSTRING(path FROM $2) WHERE user_id = $3 AND path LIKE $4",
			to+"/", len(from)+2, uid, from+"/%",
		)

		// Best-effort rewrite of blob_keys to new folder. Rather than touching
		// storage objects (some backends don't support rename), we let the
		// keys drift. The next note save will rewrite under the new key.

		writeJSON(w, 200, map[string]string{"from": from, "to": to})
	}
}
