package analyze

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DirSource reads a local mirror of the raw bucket, such as the output of
// `aws s3 sync s3://<bucket>/raw ./raw`. Keys are paths relative to Root
// with forward slashes, so the same List/Get code serves both.
type DirSource struct {
	Root string
}

func (d DirSource) List(_ context.Context, prefix string) ([]string, error) {
	var keys []string
	err := filepath.WalkDir(d.Root, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
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

func (d DirSource) Get(_ context.Context, key string) ([]byte, error) {
	return os.ReadFile(filepath.Join(d.Root, filepath.FromSlash(key)))
}
