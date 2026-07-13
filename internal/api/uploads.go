package api

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"time"

	"sajni/internal/storage"
)

const (
	maxUploadBytes  int64 = 10 << 20
	maxRequestBytes int64 = maxUploadBytes + 1<<20
)

func registerUploadRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("POST /api/uploads", uploadFile(deps))
	mux.HandleFunc("GET /api/uploads/{filename}", serveUpload(deps))
}

func uploadFile(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) || errors.Is(err, multipart.ErrMessageTooLarge) {
				errJSON(w, http.StatusRequestEntityTooLarge, "file exceeds 10 MiB limit")
				return
			}
			errJSON(w, http.StatusBadRequest, "invalid multipart form")
			return
		}
		if r.MultipartForm != nil {
			defer r.MultipartForm.RemoveAll()
		}

		file, header, err := r.FormFile("file")
		if err != nil {
			errJSON(w, 400, "no file uploaded")
			return
		}
		defer file.Close()

		data, err := io.ReadAll(io.LimitReader(file, maxUploadBytes+1))
		if err != nil {
			internalError(w, r, "read upload", err)
			return
		}
		if int64(len(data)) > maxUploadBytes {
			errJSON(w, http.StatusRequestEntityTooLarge, "file exceeds 10 MiB limit")
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
			internalError(w, r, "store upload", err)
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
			internalError(w, r, "load upload", err)
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
