package builder

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"tinycld.org/core/tenantcfg"
)

// RecipeFile is the provenance record the builder writes into every committed
// artifact. Its facts (members, integrities, toolchain, hash) are computed by
// the TRUSTED parent during resolve — never copied from anything the confined
// build job reported — so an entry's directory name can be believed. See the
// package doc's trust argument.
//
// The file's SHAPE lives in core (tenantcfg) since §7 step 4: an
// artifact-booted tenant reads its own recipe to learn its built-in set, so
// the shape is router↔tenant ABI — one definition for this writer and the
// tenant's loader, like tenantcfg.DeployResult.
const RecipeFile = tenantcfg.ArtifactRecipeFile

// MemberManifestsDir is the artifact subdirectory holding each feature
// member's evaluated manifest as manifests/<slug>/manifest.json — the same
// shape the package store materializes, so the router's capability wiring
// (controlplane.CardDAVSources etc.) reads an artifact member exactly like a
// store package. Written by the trusted parent from resolve-time evaluation,
// never by the confined job. The base member ships no manifest. ABI, like
// RecipeFile: the tenant's registry reconcile reads these.
const MemberManifestsDir = tenantcfg.ArtifactManifestsDir

// Recipe is the parsed shape of RecipeFile (ABI-owned by tenantcfg).
type Recipe = tenantcfg.ArtifactRecipe

// Store is the content-addressed build-artifact cache: one immutable directory
// per recipe hash under <dir>/<hex>, holding the runtime tree a tenant boots
// from (server binary, pb_hooks/, pb_migrations/, pb_public/) plus RecipeFile.
// Two orgs whose lockfiles resolve to the same recipe share the entry by
// construction (DESIGN-org-package-agency.md D4).
type Store struct {
	dir string
}

// NewStore returns a Store rooted at dir (canonically <MT_ROOT>/builds).
func NewStore(dir string) *Store { return &Store{dir: dir} }

// hashHexRe is the only directory-name shape the artifact store accepts.
// Validating the full 64-hex form (rather than sanitizing) makes path escape
// impossible by construction — the recipe hash is always a sha256 hex digest.
var hashHexRe = regexp.MustCompile(`^[a-f0-9]{64}$`)

// hashHex converts a RecipeHash ("sha256:<hex>") into the store directory
// name, refusing anything that is not exactly the expected shape.
func hashHex(recipeHash string) (string, error) {
	hex, ok := strings.CutPrefix(recipeHash, "sha256:")
	if !ok || !hashHexRe.MatchString(hex) {
		return "", fmt.Errorf("invalid recipe hash %q", recipeHash)
	}
	return hex, nil
}

// Dir returns the artifact directory for recipeHash without checking whether
// it exists.
func (s *Store) Dir(recipeHash string) (string, error) {
	hex, err := hashHex(recipeHash)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.dir, hex), nil
}

// Has reports whether a COMMITTED artifact exists for recipeHash. Presence is
// defined by RecipeFile, which Commit's rename makes appear atomically — a
// crashed or in-flight stage directory never counts.
func (s *Store) Has(recipeHash string) (bool, error) {
	dir, err := s.Dir(recipeHash)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(filepath.Join(dir, RecipeFile)); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ReadRecipe returns the committed recipe for recipeHash.
func (s *Store) ReadRecipe(recipeHash string) (Recipe, error) {
	dir, err := s.Dir(recipeHash)
	if err != nil {
		return Recipe{}, err
	}
	return readRecipeFile(filepath.Join(dir, RecipeFile))
}

func readRecipeFile(path string) (Recipe, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Recipe{}, err
	}
	var r Recipe
	if err := json.Unmarshal(raw, &r); err != nil {
		return Recipe{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return r, nil
}

// Commit moves a fully-staged artifact tree (already containing RecipeFile)
// into the store under recipeHash and returns the committed directory. srcDir
// must live on the same filesystem as the store so the final os.Rename is
// atomic — the builder stages jobs under the same root for exactly this
// reason.
//
// Losing a commit race is success, not failure: if an entry for recipeHash
// already exists, srcDir is discarded and the existing entry returned. This is
// what makes a per-hash singleflight unnecessary across differently-keyed
// build requests that happen to resolve to the same recipe.
func (s *Store) Commit(recipeHash, srcDir string) (string, error) {
	dest, err := s.Dir(recipeHash)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(srcDir, RecipeFile)); err != nil {
		return "", fmt.Errorf("commit %s: staged tree has no %s: %w", recipeHash, RecipeFile, err)
	}
	// The staged tree was produced inside the confined build job, so it is
	// owned by the JOB uid with owner-write. Committed artifacts are shared by
	// hash and booted by many orgs, so a build job that could still write into
	// builds/ (it runs the recipe's own lifecycle scripts) could overwrite a
	// SIBLING org's already-committed binary. Reclaim the tree to root before
	// it becomes visible so a committed artifact is immutable to every job
	// uid — the trust boundary the package doc's argument depends on.
	if err := chownTreeToRoot(srcDir); err != nil {
		return "", fmt.Errorf("commit %s: %w", recipeHash, err)
	}
	// Artifacts are served to (and bind-mounted read-only into) tenant
	// processes running as other uids: open the tree for traversal/read before
	// it becomes visible, preserving exec bits (the server binary).
	if err := chmodTreeWorldReadable(srcDir); err != nil {
		return "", fmt.Errorf("commit %s: %w", recipeHash, err)
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(srcDir, dest); err != nil {
		// A concurrent build of the same recipe won the rename. The entries
		// are equivalent by construction (same inputs), so adopt the winner.
		if ok, hasErr := s.Has(recipeHash); hasErr == nil && ok {
			_ = os.RemoveAll(srcDir)
			return dest, nil
		}
		return "", fmt.Errorf("commit %s: %w", recipeHash, err)
	}
	return dest, nil
}

// Entry is one committed artifact in the store.
type Entry struct {
	RecipeHash string
	Dir        string
	BuiltAt    time.Time
}

// List returns every committed entry. Uncommitted debris (a directory without
// RecipeFile — a crashed rename source never appears here, but a manually
// created dir might) is skipped, not an error.
func (s *Store) List() ([]Entry, error) {
	dirents, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Entry
	for _, de := range dirents {
		if !de.IsDir() || !hashHexRe.MatchString(de.Name()) {
			continue
		}
		dir := filepath.Join(s.dir, de.Name())
		recipe, err := readRecipeFile(filepath.Join(dir, RecipeFile))
		if err != nil {
			continue
		}
		out = append(out, Entry{RecipeHash: "sha256:" + de.Name(), Dir: dir, BuiltAt: recipe.BuiltAt})
	}
	return out, nil
}

// Remove deletes the artifact for recipeHash. Removing an absent entry is a
// no-op. Callers own the liveness policy (see Sweep) — Remove itself does not
// check references.
func (s *Store) Remove(recipeHash string) error {
	dir, err := s.Dir(recipeHash)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// chownTreeToRoot reclaims a staged artifact tree from the build-job uid to
// root (uid/gid 0). Only root can chown to root, and job-uid confinement only
// exists when the builder runs as root, so off that path (a non-root dev host,
// where jobs are unconfined and already run as the builder's own uid) this is
// a no-op — there is no distinct job uid to reclaim from. Symlinks are
// Lchown'd, never followed.
func chownTreeToRoot(root string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	return filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, 0, 0)
	})
}

// chmodTreeWorldReadable opens dirs to 0755 and files to 0644 (0755 when any
// exec bit is set, preserving the server binary's executability) so tenant
// uids can traverse and read a committed artifact.
func chmodTreeWorldReadable(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		// Symlinks keep their target's mode; chmod would follow them.
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		mode := os.FileMode(0o644)
		if d.IsDir() || info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		return os.Chmod(path, mode)
	})
}
