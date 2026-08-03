package builder

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// writeFileAt creates parents and writes content at artifactDir-relative rel.
func writeFileAt(t *testing.T, artifactDir, rel, content string) string {
	t.Helper()
	p := filepath.Join(artifactDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestScanNativeBundles_NoNativeDirIsNotAnError(t *testing.T) {
	// A web-only toolchain (or an artifact predating native export) simply has
	// no bundles — the update endpoint answers 204 and mobile keeps its
	// embedded bundle. This must never fail a build.
	got, err := scanNativeBundles(t.TempDir(), "recipe-abc123abc123", "0.4.0")
	if err != nil {
		t.Fatalf("want nil error for missing native dir, got %v", err)
	}
	if got != nil {
		t.Fatalf("want no bundles, got %+v", got)
	}
}

func TestScanNativeBundles_DerivesMetadataFromStagedBytes(t *testing.T) {
	dir := t.TempDir()
	writeFileAt(t, dir, "pb_public/native/ios/_expo/static/js/ios/index.hbc", "IOSBYTES")
	writeFileAt(t, dir, "pb_public/native/ios/assets/logo.png", "PNGBYTES")
	writeFileAt(t, dir, "pb_public/native/android/_expo/static/js/android/index.hbc", "DROIDBYTES")

	got, err := scanNativeBundles(dir, "recipe-abc123abc123", "0.4.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 bundles, got %d: %+v", len(got), got)
	}

	// Sorted by platform, so android comes first — deterministic output is what
	// keeps recipe.json byte-identical across rebuilds.
	if got[0].Platform != "android" || got[1].Platform != "ios" {
		t.Fatalf("want [android ios], got [%s %s]", got[0].Platform, got[1].Platform)
	}

	ios := got[1]
	if want := "recipe-abc123abc123-ios"; ios.BundleID != want {
		t.Errorf("BundleID = %q, want %q", ios.BundleID, want)
	}
	// The hash must be of the bytes actually on disk — that is the whole point
	// of deriving parent-side rather than trusting the job's report.
	if want := sha256Hex("IOSBYTES"); ios.BundleHash != want {
		t.Errorf("BundleHash = %q, want %q", ios.BundleHash, want)
	}
	if want := "_expo/static/js/ios/index.hbc"; ios.BundleFile != want {
		t.Errorf("BundleFile = %q, want %q", ios.BundleFile, want)
	}
	if ios.RuntimeVersion != "0.4.0" {
		t.Errorf("RuntimeVersion = %q, want 0.4.0", ios.RuntimeVersion)
	}
	if len(ios.Assets) != 1 {
		t.Fatalf("want 1 asset, got %+v", ios.Assets)
	}
	if ios.Assets[0].File != "assets/logo.png" {
		t.Errorf("asset File = %q", ios.Assets[0].File)
	}
	if want := sha256Hex("PNGBYTES"); ios.Assets[0].Hash != want {
		t.Errorf("asset Hash = %q, want %q", ios.Assets[0].Hash, want)
	}
	if ios.Assets[0].ContentType != "image/png" {
		t.Errorf("asset ContentType = %q, want image/png", ios.Assets[0].ContentType)
	}
}

func TestScanNativeBundles_ExcludesSourcemaps(t *testing.T) {
	// Sourcemaps ride along from --source-maps external and are uploaded to
	// Sentry at build time. Advertising one would make every device download
	// megabytes it cannot use, over a public pre-auth endpoint.
	dir := t.TempDir()
	writeFileAt(t, dir, "pb_public/native/ios/index.hbc", "BYTES")
	writeFileAt(t, dir, "pb_public/native/ios/index.hbc.map", "{}")

	got, err := scanNativeBundles(dir, "recipe-abc123abc123", "0.4.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 bundle, got %d", len(got))
	}
	for _, a := range got[0].Assets {
		if a.File == "index.hbc.map" {
			t.Fatalf("sourcemap was advertised as an asset: %+v", got[0].Assets)
		}
	}
	if len(got[0].Assets) != 0 {
		t.Fatalf("want no assets, got %+v", got[0].Assets)
	}
}

func TestScanNativeBundles_EmptyRuntimeVersionRefuses(t *testing.T) {
	// Bundles staged but no runtime version means nothing could ever match a
	// client. Fail the build rather than commit undeliverable dead weight.
	dir := t.TempDir()
	writeFileAt(t, dir, "pb_public/native/ios/index.hbc", "BYTES")

	if _, err := scanNativeBundles(dir, "recipe-abc123abc123", ""); err == nil {
		t.Fatal("want an error when runtimeVersion is empty")
	}
}

func TestScanNativeBundles_PlatformWithoutHbcIsSkipped(t *testing.T) {
	dir := t.TempDir()
	writeFileAt(t, dir, "pb_public/native/ios/assets/only-an-asset.png", "X")

	got, err := scanNativeBundles(dir, "recipe-abc123abc123", "0.4.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("want no bundles when no .hbc is present, got %+v", got)
	}
}

func TestScanNativeBundles_RejectsMultipleHbc(t *testing.T) {
	// The native module's locateHbc loads the FIRST .hbc it finds, so two would
	// make the bundle a device runs arbitrary. Refuse rather than guess.
	dir := t.TempDir()
	writeFileAt(t, dir, "pb_public/native/ios/a/index.hbc", "A")
	writeFileAt(t, dir, "pb_public/native/ios/b/index.hbc", "B")

	if _, err := scanNativeBundles(dir, "recipe-abc123abc123", "0.4.0"); err == nil {
		t.Fatal("want an error when a platform stages two .hbc files")
	}
}

func TestScanNativeBundles_IgnoresUnknownPlatformDirs(t *testing.T) {
	dir := t.TempDir()
	writeFileAt(t, dir, "pb_public/native/ios/index.hbc", "BYTES")
	writeFileAt(t, dir, "pb_public/native/linux/index.hbc", "NOPE")

	got, err := scanNativeBundles(dir, "recipe-abc123abc123", "0.4.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Platform != "ios" {
		t.Fatalf("want only the ios bundle, got %+v", got)
	}
}

func TestRuntimeVersionFromBase(t *testing.T) {
	t.Run("reads app.json expo.version", func(t *testing.T) {
		dir := t.TempDir()
		writeFileAt(t, dir, "app.json", `{"expo":{"version":"2.0.1"}}`)
		if got := runtimeVersionFromBase(dir); got != "2.0.1" {
			t.Fatalf("got %q, want 2.0.1", got)
		}
	})

	t.Run("never falls back to package.json", func(t *testing.T) {
		// The store/OTA version is DECOUPLED from the project version by design
		// (app.config.ts): the shell is 0.4.0 while expo.version is 2.0.1.
		// Stamping bundles with package.json's number would make every device
		// 204 forever — a silent, permanent "updates never arrive". Prefer an
		// empty result (which fails the build loudly) over a plausible wrong one.
		dir := t.TempDir()
		writeFileAt(t, dir, "app.json", `{"expo":{"name":"TinyCld"}}`)
		writeFileAt(t, dir, "package.json", `{"version":"0.4.0"}`)
		if got := runtimeVersionFromBase(dir); got != "" {
			t.Fatalf("got %q, want empty — package.json must never be used", got)
		}
	})

	t.Run("empty when app.json is absent", func(t *testing.T) {
		if got := runtimeVersionFromBase(t.TempDir()); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})
}
