// Package storage abstracts blob storage so the same handler code works
// against the local filesystem (dev) or Google Cloud Storage (prod).
//
// All keys must be prefixed with `user_<id>/...` to enforce tenant
// isolation; helpers are provided so callers don't need to spell the
// prefix manually.
package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"sajni/internal/config"
)

// ErrNotFound is returned by Get/Delete when the key does not exist.
var ErrNotFound = errors.New("storage: not found")

// Storage is the minimal interface every backend implements.
type Storage interface {
	Put(ctx context.Context, key string, data []byte, contentType string) error
	Get(ctx context.Context, key string) (data []byte, contentType string, err error)
	Delete(ctx context.Context, key string) error
}

// New returns the configured Storage implementation.
//
//	local (default) — filesystem-backed, root from STORAGE_LOCAL_DIR (default ./data/blobs)
//	gcs             — Google Cloud Storage, bucket from GCS_BUCKET
func New(ctx context.Context, cfg config.Storage) (Storage, error) {
	switch cfg.Backend {
	case "local":
		return NewLocal(cfg.LocalDir)
	case "gcs":
		if cfg.GCSBucket == "" {
			return nil, fmt.Errorf("STORAGE_BACKEND=gcs requires GCS_BUCKET")
		}
		return NewGCS(ctx, cfg.GCSBucket)
	default:
		return nil, fmt.Errorf("unknown STORAGE_BACKEND %q", cfg.Backend)
	}
}

// UserKey returns a tenant-scoped object key.
//
//	UserKey("018f…", "uploads", "abc.png") -> "user_018f…/uploads/abc.png"
//
// userID is the UUIDv7 string form of users.id.
func UserKey(userID string, parts ...string) string {
	out := "user_" + userID
	for _, p := range parts {
		p = strings.Trim(p, "/")
		if p == "" {
			continue
		}
		out += "/" + p
	}
	return out
}
