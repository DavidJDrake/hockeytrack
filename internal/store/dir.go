package store

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DirArchive is a Lister over a local directory, for a mirror of the raw
// bucket (`aws s3 sync s3://<bucket>/raw ./mirror/raw`) or a local index.
// Keys are paths relative to Root with forward slashes, so the same code
// serves S3 and disk.
type DirArchive struct {
	Root string
}

func (d DirArchive) List(_ context.Context, prefix string) ([]string, error) {
	var keys []string
	err := filepath.WalkDir(d.Root, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && path == d.Root {
				return filepath.SkipAll // an empty index is a directory not yet created
			}
			return err
		}
		if e.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(d.Root, path)
		if err != nil {
			return err
		}
		if key := filepath.ToSlash(rel); strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
		return nil
	})
	return keys, err
}

func (d DirArchive) Get(_ context.Context, key string) ([]byte, error) {
	return os.ReadFile(filepath.Join(d.Root, filepath.FromSlash(key)))
}

// Put writes the object under Root, creating directories as needed. Keys
// are validated against escaping Root, since an index key is derived from
// archive listings rather than typed by hand.
func (d DirArchive) Put(_ context.Context, key string, body []byte) error {
	path := filepath.Join(d.Root, filepath.FromSlash(key))
	if rel, err := filepath.Rel(d.Root, path); err != nil || strings.HasPrefix(rel, "..") {
		return &fs.PathError{Op: "put", Path: key, Err: fs.ErrInvalid}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644)
}
