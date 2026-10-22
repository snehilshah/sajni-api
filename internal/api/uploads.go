package api

import (
	"context"
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
		f, ok := readUpload(w, r)
		if !ok {
			return
		}
		filename, err := storeUpload(r.Context(), deps, userID(r.Context()), f)
		if err != nil {
			internalError(w, r, "store upload", err)
			return
		}
		writeJSON(w, 201, map[string]string{
			"url":      "/api/uploads/" + filename,
			"filename": filename,
		})
	}
}

type uploadedFile struct {
	data        []byte
	name        string
	contentType string
}

// readUpload reads the multipart "file" field (at most 10 MB). On failure
// it has already written the error response.
func readUpload(w http.ResponseWriter, r *http.Request) (uploadedFile, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) || errors.Is(err, multipart.ErrMessageTooLarge) {
			errJSON(w, http.StatusRequestEntityTooLarge, "Files can be at most 10 MB.")
			return uploadedFile{}, false
		}
		errJSON(w, http.StatusBadRequest, "invalid multipart form")
		return uploadedFile{}, false
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		errJSON(w, 400, "Choose a file to upload.")
		return uploadedFile{}, false
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxUploadBytes+1))
	if err != nil {
		internalError(w, r, "read upload", err)
		return uploadedFile{}, false
	}
	if int64(len(data)) > maxUploadBytes {
		errJSON(w, http.StatusRequestEntityTooLarge, "Files can be at most 10 MB.")
		return uploadedFile{}, false
	}
	return uploadedFile{data: data, name: header.Filename, contentType: header.Header.Get("Content-Type")}, true
}

// storeUpload saves the file under the user's uploads/ and returns its
// filename (what GET /api/uploads/{filename} serves).
func storeUpload(ctx context.Context, deps Deps, uid string, f uploadedFile) (string, error) {
	hash := sha256.Sum256(f.data)
	ext := filepath.Ext(f.name)
	if ext == "" {
		ext = ".png"
	}
	filename := fmt.Sprintf("%d-%x%s", time.Now().UnixMilli(), hash[:8], ext)
	return filename, deps.Storage.Put(ctx, storage.UserKey(uid, "uploads", filename), f.data, f.contentType)
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
