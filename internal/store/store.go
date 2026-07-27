package store

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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

// pkgNameRe matches an npm-style package name: a bare slug or one @scope/slug
// segment. versionRe matches a semver-ish string. Both deliberately exclude
// path separators, `.` and `..` as whole segments, and anything else that would
// mean something to filepath.Join.
//
// Names and versions arrive from a lockfile or a publish request body and are
// interpolated straight into a filesystem path, so a `../..` segment escapes
// the store root — a publish could write anywhere the router can reach, and a
// resolve could hand a tenant a directory outside the store. Both inputs are
// superuser-supplied today, which bounds the severity, not the bug.
var (
	pkgNameRe = regexp.MustCompile(`^(@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*$`)
	versionRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.+-]*$`)
)

// validRef rejects a name/version pair that could escape the store root.
func validRef(name, version string) error {
	if !pkgNameRe.MatchString(name) {
		return fmt.Errorf("invalid package name %q", name)
	}
	if !versionRe.MatchString(version) {
		return fmt.Errorf("invalid package version %q", version)
	}
	// A segment of exactly "." or ".." passes the character classes above
	// (both are made of allowed runes), so reject them explicitly.
	for _, seg := range strings.Split(name, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("invalid package name %q", name)
		}
	}
	if version == "." || version == ".." {
		return fmt.Errorf("invalid package version %q", version)
	}
	return nil
}

func (s *PackageStore) versionPath(name, version string) (string, error) {
	if err := validRef(name, version); err != nil {
		return "", err
	}
	return filepath.Join(s.root, "packages", name, version), nil
}

// Publish writes a package version immutably. Re-publishing an existing version
// is an error — versions never change once written.
func (s *PackageStore) Publish(name, version string, files map[string][]byte) error {
	dir, err := s.versionPath(name, version)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("package %s@%s already published (immutable)", name, version)
	}
	for rel, content := range files {
		// The file paths inside the archive are the same escape one level
		// down. Resolve and confirm the result is still under dir rather than
		// pattern-matching the input: filepath.Join already collapses `..`,
		// so a containment check catches every spelling.
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if !isWithin(dir, full) {
			return fmt.Errorf("package %s@%s: file path %q escapes the version directory", name, version, rel)
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// isWithin reports whether path is dir itself or lives beneath it.
func isWithin(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// VersionDir returns the on-disk directory for a published version.
func (s *PackageStore) VersionDir(name, version string) (string, error) {
	dir, err := s.versionPath(name, version)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("package %s@%s not found: %w", name, version, err)
	}
	return dir, nil
}
