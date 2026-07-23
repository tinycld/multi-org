package store

import (
	"fmt"
	"os"
	"path/filepath"
)

// PackageStore is a content-addressed, write-once store of versioned packages.
// A version lives at <root>/packages/<name>/<version>/ and is never modified
// once published.
type PackageStore struct {
	root string // <root>/packages/<name>/<version>/...
}

func New(root string) *PackageStore {
	return &PackageStore{root: root}
}

func (s *PackageStore) versionPath(name, version string) string {
	return filepath.Join(s.root, "packages", name, version)
}

// Publish writes a package version immutably. Re-publishing an existing version
// is an error — versions never change once written.
func (s *PackageStore) Publish(name, version string, files map[string][]byte) error {
	dir := s.versionPath(name, version)
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("package %s@%s already published (immutable)", name, version)
	}
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// VersionDir returns the on-disk directory for a published version.
func (s *PackageStore) VersionDir(name, version string) (string, error) {
	dir := s.versionPath(name, version)
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("package %s@%s not found: %w", name, version, err)
	}
	return dir, nil
}
