package api

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"strconv"
	"testing"

	"sajni/internal/auth"
	"sajni/internal/db"
)

// Needs a scratch Postgres: SAJNI_TEST_DATABASE_URL=postgres://… go test ./internal/api -run Attachment
func TestTaskAttachments(t *testing.T) {
	dsn := os.Getenv("SAJNI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SAJNI_TEST_DATABASE_URL not set")
	}
	d, err := db.New(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	uid := "01900000-0000-7000-8000-000000000009"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, 'attach@test', 'T')`, uid)
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	var task int64
	d.QueryRow(`INSERT INTO tasks (user_id, title) VALUES ($1, 'Bug') RETURNING id`, uid).Scan(&task)

	store := &uploadStore{}
	mux := http.NewServeMux()
	registerTaskRoutes(mux, Deps{DB: d, Storage: store})
	do := func(req *http.Request) *httptest.ResponseRecorder {
		req = req.WithContext(context.WithValue(req.Context(), auth.ContextKey{}, uid))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	base := "/api/tasks/" + strconv.FormatInt(task, 10)
	attach := func(contentType string) *httptest.ResponseRecorder {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="file"; filename="shot.png"`)
		h.Set("Content-Type", contentType)
		part, _ := mw.CreatePart(h)
		part.Write([]byte("png bytes"))
		mw.Close()
		req := httptest.NewRequest("POST", base+"/attachments", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		return do(req)
	}

	if rec := attach("text/plain"); rec.Code != 400 {
		t.Fatalf("non-image: %d", rec.Code)
	}
	rec := attach("image/png")
	if rec.Code != 201 {
		t.Fatalf("attach: %d %s", rec.Code, rec.Body)
	}
	var a taskAttachment
	json.Unmarshal(rec.Body.Bytes(), &a)
	if a.URL != "/api/uploads/"+a.Filename || store.key == "" {
		t.Fatalf("attachment %+v, stored %q", a, store.key)
	}

	var list []taskAttachment
	json.Unmarshal(do(httptest.NewRequest("GET", base+"/attachments", nil)).Body.Bytes(), &list)
	if len(list) != 1 || list[0].ID != a.ID {
		t.Fatalf("list: %+v", list)
	}

	// Deleting the task deletes its files.
	if rec := do(httptest.NewRequest("DELETE", base, nil)); rec.Code != 200 {
		t.Fatalf("delete task: %d", rec.Code)
	}
	if !store.deleted {
		t.Fatal("file not deleted with the task")
	}
}
