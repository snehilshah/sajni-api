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
	"os"
	"strings"
)

// ErrNotFound is returned by Get/Delete when the key does not exist.
var ErrNotFound = errors.New("storage: not found")

// Storage is the minimal interface every backend implements.
type Storage interface {
	Put(ctx context.Context, key string, data []byte, contentType string) error
	Get(ctx context.Context, key string) (data []byte, contentType string, err error)
	Delete(ctx context.Context, key string) error
}

// New returns a Storage implementation chosen by the STORAGE_BACKEND env var.
//
//	local (default) — filesystem-backed, root from STORAGE_LOCAL_DIR (default ./data/blobs)
//	gcs             — Google Cloud Storage, bucket from GCS_BUCKET
func New(ctx context.Context) (Storage, error) {
	switch backend := strings.ToLower(os.Getenv("STORAGE_BACKEND")); backend {
	case "", "local":
		root := os.Getenv("STORAGE_LOCAL_DIR")
		if root == "" {
			root = "./data/blobs"
		}
		return NewLocal(root)
	case "gcs":
		bucket := os.Getenv("GCS_BUCKET")
		if bucket == "" {
			return nil, fmt.Errorf("STORAGE_BACKEND=gcs requires GCS_BUCKET")
		}
		return NewGCS(ctx, bucket)
	default:
		return nil, fmt.Errorf("unknown STORAGE_BACKEND %q", backend)
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
