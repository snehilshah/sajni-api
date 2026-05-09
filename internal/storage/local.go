package storage

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"
)

// LocalStorage stores blobs as files under Root, with the object key
// translating directly to a relative file path.
type LocalStorage struct {
	Root string
}

func NewLocal(root string) (*LocalStorage, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create storage root: %w", err)
	}
	return &LocalStorage{Root: root}, nil
}

// safePath joins the root with the key, rejecting traversal attempts.
func (l *LocalStorage) safePath(key string) (string, error) {
	cleaned := filepath.Clean("/" + key)
	if strings.Contains(cleaned, "..") {
		return "", fmt.Errorf("invalid key")
	}
	return filepath.Join(l.Root, cleaned), nil
}

func (l *LocalStorage) Put(_ context.Context, key string, data []byte, _ string) error {
	p, err := l.safePath(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

func (l *LocalStorage) Get(_ context.Context, key string) ([]byte, string, error) {
	p, err := l.safePath(key)
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", ErrNotFound
		}
		return nil, "", err
	}
	ct := mime.TypeByExtension(filepath.Ext(p))
	if ct == "" {
		ct = "application/octet-stream"
	}
	return data, ct, nil
}

func (l *LocalStorage) Delete(_ context.Context, key string) error {
	p, err := l.safePath(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		return err
	}
	return nil
}
