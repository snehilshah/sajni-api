package storage

import (
	"context"
	"errors"
	"fmt"
	"io"

	"cloud.google.com/go/storage"
)

// GCSStorage stores blobs in a Google Cloud Storage bucket.
type GCSStorage struct {
	client *storage.Client
	bucket string
}

func NewGCS(ctx context.Context, bucket string) (*GCSStorage, error) {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("create gcs client: %w", err)
	}
	return &GCSStorage{client: client, bucket: bucket}, nil
}

func (g *GCSStorage) obj(key string) *storage.ObjectHandle {
	return g.client.Bucket(g.bucket).Object(key)
}

func (g *GCSStorage) Put(ctx context.Context, key string, data []byte, contentType string) error {
	w := g.obj(key).NewWriter(ctx)
	if contentType != "" {
		w.ContentType = contentType
	}
	if _, err := w.Write(data); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
}

func (g *GCSStorage) Get(ctx context.Context, key string) ([]byte, string, error) {
	r, err := g.obj(key).NewReader(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil, "", ErrNotFound
		}
		return nil, "", err
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, "", err
	}
	return data, r.Attrs.ContentType, nil
}

func (g *GCSStorage) Delete(ctx context.Context, key string) error {
	if err := g.obj(key).Delete(ctx); err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return ErrNotFound
		}
		return err
	}
	return nil
}
