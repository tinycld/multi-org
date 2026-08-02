package builder

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"tinycld.org/core/pkgbuild"
)

// nativeDir is the artifact-relative root the build pipeline stages native OTA
// bundles into (pkgbuild.stageNativeBundlesIntoRelease writes
// <release>/native/<platform>/..., and stageArtifact copies that whole release
// tree to pb_public/). The tenant's /api/app/update serves from the same path.
const nativeDir = "pb_public/native"

// hbcExt identifies the Hermes bytecode bundle among the staged files. The
// native module's locateHbc looks for exactly this, so the scan must agree.
const hbcExt = ".hbc"

// scanNativeBundles derives the artifact's BundleMeta list by READING THE
// STAGED BYTES — walking <artifactDir>/pb_public/native/<platform>/, finding
// the .hbc, and hashing every file itself.
//
// It deliberately does NOT accept the metadata the build pipeline computed.
// The pipeline runs inside the confined job, and a job executes package-author
// code (pnpm lifecycle scripts, Metro) by design; anything it *reports* is
// attacker-influenced. Bundle hashes are the client's entire integrity
// guarantee — a job that could name its own hash could have a device accept
// bytes the parent never vouched for. So this mirrors the rule the rest of the
// builder already follows for members and integrities: the trusted parent
// derives identity facts from bytes it reads, after the job has finished.
//
// Re-hashing here also means the recorded hash describes the file as COMMITTED,
// closing the window between the job's hash and what actually landed on disk.
//
// A missing native/ dir (web-only toolchain, or a build predating native
// export) returns nil — "no native bundles", which the update endpoint answers
// with 204. Never an error: mobile simply stays on its embedded bundle.
func scanNativeBundles(artifactDir, buildID, runtimeVersion string) ([]pkgbuild.BundleMeta, error) {
	root := filepath.Join(artifactDir, filepath.FromSlash(nativeDir))
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read native bundle dir: %w", err)
	}

	// A bundle whose runtime_version is empty can never match a client (every
	// device reports a concrete app version), so it would be undeliverable
	// dead weight that still occupies the manifest. Refuse rather than commit
	// an artifact advertising bundles no device can receive — the same call
	// pkgbuild.ExportNativeBundles makes.
	if runtimeVersion == "" {
		return nil, fmt.Errorf("native bundles staged under %s but runtimeVersion is empty — they would be undeliverable", nativeDir)
	}

	var out []pkgbuild.BundleMeta
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		platform := e.Name()
		// Only the two platforms the client can ask for. An unexpected dir is
		// skipped rather than fatal: it cannot be requested (the endpoint
		// allowlists the platform segment), so it is inert.
		if platform != string(pkgbuild.PlatformIOS) && platform != string(pkgbuild.PlatformAndroid) {
			continue
		}
		bm, err := scanPlatformBundle(filepath.Join(root, platform), platform, buildID, runtimeVersion)
		if err != nil {
			return nil, err
		}
		if bm == nil {
			continue
		}
		out = append(out, *bm)
	}
	// Stable order so recipe.json is byte-identical across rebuilds of the
	// same inputs — readdir order is filesystem-dependent.
	sort.Slice(out, func(i, j int) bool { return out[i].Platform < out[j].Platform })
	return out, nil
}

// scanPlatformBundle builds the BundleMeta for one platform directory. Returns
// nil when the directory holds no .hbc (nothing to advertise).
func scanPlatformBundle(dir, platform, buildID, runtimeVersion string) (*pkgbuild.BundleMeta, error) {
	var bundleRel string
	var assets []pkgbuild.AssetMeta

	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(dir, p)
		if relErr != nil {
			return relErr
		}
		// The manifest carries slash-separated paths (they become URL path
		// segments and are re-joined against the client's staging dir), so
		// normalize away any platform separator.
		rel = filepath.ToSlash(rel)

		// Sourcemaps are exported alongside each bundle (--source-maps
		// external) and uploaded to Sentry at build time. They must never be
		// advertised to clients: the device would download megabytes it cannot
		// use, and the manifest is a public, pre-auth response.
		if strings.HasSuffix(rel, hbcExt+".map") {
			return nil
		}

		hash, hErr := sha256OfFileHex(p)
		if hErr != nil {
			return fmt.Errorf("hash %s: %w", rel, hErr)
		}

		if strings.HasSuffix(rel, hbcExt) {
			if bundleRel != "" {
				// Two .hbc files means the layout is not what the client's
				// locateHbc assumes (it loads the FIRST one it finds), so the
				// bundle a device runs would be arbitrary. Refuse.
				return fmt.Errorf("multiple %s files under %s (%s and %s)", hbcExt, platform, bundleRel, rel)
			}
			bundleRel = rel
			return nil
		}

		ct := mime.TypeByExtension(path.Ext(rel))
		if ct == "" {
			ct = "application/octet-stream"
		}
		assets = append(assets, pkgbuild.AssetMeta{
			Key:         rel,
			Hash:        hash,
			ContentType: ct,
			File:        rel,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if bundleRel == "" {
		return nil, nil
	}

	bundleHash, err := sha256OfFileHex(filepath.Join(dir, filepath.FromSlash(bundleRel)))
	if err != nil {
		return nil, fmt.Errorf("hash bundle: %w", err)
	}

	// Deterministic asset order, for the same reason as the platform sort.
	sort.Slice(assets, func(i, j int) bool { return assets[i].File < assets[j].File })

	return &pkgbuild.BundleMeta{
		Platform: platform,
		// Matches what pkgbuild.parseExportMetadata mints, so host and hosted
		// bundle ids share one shape. buildID here is the builder's
		// deterministic recipe-<hash12>, which makes hosted bundle identity
		// content-addressed: two orgs resolving the same package set get the
		// same id, and a client that already runs it is correctly told 204.
		BundleID:       fmt.Sprintf("%s-%s", buildID, platform),
		BundleHash:     bundleHash,
		BundleFile:     bundleRel,
		RuntimeVersion: runtimeVersion,
		Assets:         assets,
	}, nil
}

// runtimeVersionFromBase reads the OTA runtime version from the base member
// directory the TRUSTED PARENT fetched: app.json's `expo.version`.
//
// That file is the single source of truth, and the ONLY acceptable source here.
// Two near-misses would each silently mint undeliverable bundles:
//
//   - package.json's version is a DIFFERENT number by design. app.config.ts
//     decouples the store/OTA version from the project version so a `pnpm
//     version` bump never changes what ships (the shell is 0.4.0 while
//     expo.version is 2.0.1). Falling back to it would stamp bundles with a
//     version no device reports, so every client would 204 forever.
//   - the recipe's base ResolvedMember records @tinycld/core's version (0.0.4),
//     a third distinct number — the same duality /v1/state documents.
//
// Returns "" when app.json is missing or carries no expo.version; the caller
// turns that into a hard build failure rather than committing bundles no client
// can match. (pkgbuild's appVersionFromManifest keeps a package.json fallback
// for layouts where app.json pins nothing; here that fallback is strictly
// harmful, because the shell ALWAYS ships an app.json with expo.version — so a
// miss means something is wrong, not that another source should be tried.)
func runtimeVersionFromBase(baseDir string) string {
	return jsonStringField(filepath.Join(baseDir, "app.json"), "expo", "version")
}

// jsonStringField reads a nested string field from a JSON file, returning ""
// when the file is unreadable, unparseable, or the path is absent/not a string.
func jsonStringField(path string, keys ...string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var cur any
	if err := json.Unmarshal(raw, &cur); err != nil {
		return ""
	}
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[k]
	}
	s, _ := cur.(string)
	return s
}

// sha256OfFileHex returns the lowercase hex SHA-256 of the file at p, streaming
// so a large .hbc never lands in memory whole.
func sha256OfFileHex(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
