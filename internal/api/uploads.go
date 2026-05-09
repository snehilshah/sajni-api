package api

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"time"

	"sajni/internal/storage"
)

func registerUploadRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("POST /api/uploads", uploadFile(deps))
	mux.HandleFunc("GET /api/uploads/{filename}", serveUpload(deps))
}

func uploadFile(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		// Limit to 10MB
		r.ParseMultipartForm(10 << 20)

		file, header, err := r.FormFile("file")
		if err != nil {
			errJSON(w, 400, "no file uploaded")
			return
		}
		defer file.Close()

		data, err := io.ReadAll(file)
		if err != nil {
			errJSON(w, 500, "read file: "+err.Error())
			return
		}

		hash := sha256.Sum256(data)
		ext := filepath.Ext(header.Filename)
		if ext == "" {
			ext = ".png"
		}
		filename := fmt.Sprintf("%d-%x%s", time.Now().UnixMilli(), hash[:8], ext)

		key := storage.UserKey(uid, "uploads", filename)
		ct := header.Header.Get("Content-Type")
		if err := deps.Storage.Put(r.Context(), key, data, ct); err != nil {
			errJSON(w, 500, "store file: "+err.Error())
			return
		}

		writeJSON(w, 201, map[string]string{
			"url":      "/api/uploads/" + filename,
			"filename": filename,
		})
	}
}

func serveUpload(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		filename := pathParam(r, "filename")
		if filename == "" {
			errJSON(w, 400, "missing filename")
			return
		}
		// Strip any path separators a client tries to sneak in.
		filename = filepath.Base(filename)
		key := storage.UserKey(uid, "uploads", filename)
		data, ct, err := deps.Storage.Get(r.Context(), key)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			errJSON(w, 500, err.Error())
			return
		}
		if ct == "" {
			ct = "application/octet-stream"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "private, max-age=3600")
		w.Write(data)
	}
}
