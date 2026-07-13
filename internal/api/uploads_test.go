package api

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sajni/internal/auth"
	"sajni/internal/storage"
)

type uploadStore struct {
	data []byte
}

func (s *uploadStore) Put(_ context.Context, _ string, data []byte, _ string) error {
	s.data = append([]byte(nil), data...)
	return nil
}

func (s *uploadStore) Get(context.Context, string) ([]byte, string, error) {
	return nil, "", storage.ErrNotFound
}

func (s *uploadStore) Delete(context.Context, string) error {
	return nil
}

func multipartUpload(t *testing.T, size int64) (*http.Request, string) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", "photo.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(bytes.Repeat([]byte("x"), int(size))); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/uploads", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	ctx := context.WithValue(req.Context(), auth.ContextKey{}, "00000000-0000-7000-8000-000000000001")
	return req.WithContext(ctx), w.FormDataContentType()
}

func TestUploadFileLimits(t *testing.T) {
	tests := []struct {
		name string
		size int64
		want int
	}{
		{name: "below limit", size: 1024, want: http.StatusCreated},
		{name: "exact limit", size: maxUploadBytes, want: http.StatusCreated},
		{name: "over limit", size: maxUploadBytes + 1, want: http.StatusRequestEntityTooLarge},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &uploadStore{}
			req, _ := multipartUpload(t, tt.size)
			res := httptest.NewRecorder()

			uploadFile(Deps{Storage: store})(res, req)

			if res.Code != tt.want {
				t.Fatalf("status = %d, want %d; body=%s", res.Code, tt.want, res.Body.String())
			}
		})
	}
}

func TestUploadRejectsMalformedMultipart(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/uploads", strings.NewReader("not multipart"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=missing")
	ctx := context.WithValue(req.Context(), auth.ContextKey{}, "00000000-0000-7000-8000-000000000001")
	res := httptest.NewRecorder()

	uploadFile(Deps{Storage: &uploadStore{}})(res, req.WithContext(ctx))

	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusBadRequest)
	}
}

func TestUploadRejectsOversizedEnvelope(t *testing.T) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	padding, err := w.CreateFormField("padding")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := padding.Write(bytes.Repeat([]byte("x"), int(maxRequestBytes))); err != nil {
		t.Fatal(err)
	}
	file, err := w.CreateFormFile("file", "photo.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/uploads", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	ctx := context.WithValue(req.Context(), auth.ContextKey{}, "00000000-0000-7000-8000-000000000001")
	res := httptest.NewRecorder()

	uploadFile(Deps{Storage: &uploadStore{}})(res, req.WithContext(ctx))

	if res.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusRequestEntityTooLarge)
	}
}
