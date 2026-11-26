package storage

import (
	"context"
	"errors"
	"io"
	"strings"
)

// ErrNotFound is returned by BundleReader.Open for a missing object.
var ErrNotFound = errors.New("object not found")

// ObjectInfo describes a stored object.
type ObjectInfo struct {
	Size        int64
	ContentType string
}

// BundleReader reads built component files. The API serves them through its
// /bundles proxy so visibility is enforced on every request.
type BundleReader interface {
	Open(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error)
}

// Bundles is the reader the API uses; set at startup by InitBundles.
var Bundles BundleReader

// InitBundles connects the API to object storage (and makes the bucket
// private).
func InitBundles() error {
	u, err := NewS3Uploader()
	if err != nil {
		return err
	}
	Bundles = u
	return nil
}

// KeyFromURL extracts the object key ("components/<slug>/<version>/...")
// from any stored bundle URL, whatever host or base it was built with.
func KeyFromURL(u string) (string, bool) {
	i := strings.Index(u, "/components/")
	if i < 0 {
		return "", false
	}
	key := u[i+1:]
	if q := strings.IndexAny(key, "?#"); q >= 0 {
		key = key[:q]
	}
	return key, true
}
