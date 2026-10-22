package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"sajni/internal/db"
	"sajni/internal/storage"
)

// Images attached to a task: uploaded and listed under it, removed with it.

type taskAttachment struct {
	ID          int64     `json:"id"`
	URL         string    `json:"url"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type"`
	CreatedAt   time.Time `json:"created_at"`
}

func registerTaskAttachmentRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("GET /api/tasks/{id}/attachments", listTaskAttachments(deps))
	mux.HandleFunc("POST /api/tasks/{id}/attachments", addTaskAttachment(deps))
	mux.HandleFunc("DELETE /api/tasks/{id}/attachments/{aid}", deleteTaskAttachment(deps))
}

func listTaskAttachments(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		out, err := taskAttachments(r.Context(), deps.DB, userID(r.Context()), id)
		if err != nil {
			internalError(w, r, "list task attachments", err)
			return
		}
		writeJSON(w, 200, out)
	}
}

// addTaskAttachment takes one multipart "file" (an image, at most 10 MB).
func addTaskAttachment(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var owned int
		deps.DB.QueryRow("SELECT 1 FROM tasks WHERE id = $1 AND user_id = $2", id, uid).Scan(&owned)
		if owned != 1 {
			errJSON(w, 404, "task not found")
			return
		}
		f, ok := readUpload(w, r)
		if !ok {
			return
		}
		if !strings.HasPrefix(f.contentType, "image/") {
			errJSON(w, 400, "Only images can be attached.")
			return
		}
		filename, err := storeUpload(r.Context(), deps, uid, f)
		if err != nil {
			internalError(w, r, "store attachment", err)
			return
		}
		a := taskAttachment{Filename: filename, ContentType: f.contentType, URL: "/api/uploads/" + filename}
		if err := deps.DB.QueryRow(
			`INSERT INTO task_attachments (user_id, task_id, filename, content_type) VALUES ($1, $2, $3, $4)
			 RETURNING id, created_at`, uid, id, filename, f.contentType,
		).Scan(&a.ID, &a.CreatedAt); err != nil {
			deps.Storage.Delete(r.Context(), storage.UserKey(uid, "uploads", filename))
			internalError(w, r, "insert attachment", err)
			return
		}
		writeJSON(w, 201, a)
	}
}

func deleteTaskAttachment(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err1 := intParam(r, "id")
		aid, err2 := intParam(r, "aid")
		if err1 != nil || err2 != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var filename string
		err := deps.DB.QueryRow(
			`DELETE FROM task_attachments WHERE id = $1 AND task_id = $2 AND user_id = $3 RETURNING filename`,
			aid, id, uid).Scan(&filename)
		if err == nil {
			deps.Storage.Delete(r.Context(), storage.UserKey(uid, "uploads", filename))
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func taskAttachments(ctx context.Context, d *db.DB, uid string, taskID int64) ([]taskAttachment, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT id, filename, content_type, created_at FROM task_attachments
		 WHERE user_id = $1 AND task_id = $2 ORDER BY id`, uid, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []taskAttachment{}
	for rows.Next() {
		var a taskAttachment
		if err := rows.Scan(&a.ID, &a.Filename, &a.ContentType, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.URL = "/api/uploads/" + a.Filename
		out = append(out, a)
	}
	return out, rows.Err()
}
