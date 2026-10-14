package clean

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencharly/sdk/storekit"
)

// retired_roots_test.go — the retirement instrument (the `cache` category's second half,
// opencharly/charly-registry#11). Each arm pins a property the sweep exists for: it reports before
// it removes, it removes ONLY the declared roots, and a root that is already gone is a no-op rather
// than an error — because "already swept" is the state a re-run reaches.

// retiredRootFixture isolates every root the sweep touches and creates the retired root's real
// shape: the pre-rename image BLOB store (`<cache>/images/sha256/<hex>`), which carries no
// `oci-layout` marker at all — the measurement that made a DISCOVERY mechanism the wrong instrument
// for this object and a declared retirement the right one.
func retiredRootFixture(t *testing.T) (cacheRoot, imagesDir string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CHARLY_CACHE_DIR", root)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	images := filepath.Join(root, "images", "sha256")
	if err := os.MkdirAll(images, 0o755); err != nil {
		t.Fatalf("mkdir retired root: %v", err)
	}
	for _, name := range []string{strings.Repeat("a", 64), strings.Repeat("b", 64)} {
		if err := os.WriteFile(filepath.Join(images, name), make([]byte, 2048), 0o644); err != nil {
			t.Fatalf("write blob: %v", err)
		}
	}
	return root, filepath.Join(root, "images")
}

// TestRetiredCacheRootSweepReportsThenRemoves pins the idiom the rest of this category uses: the dry
// run SIZES the root and removes nothing, the real run removes it, and a second real run reports it
// absent instead of failing.
func TestRetiredCacheRootSweepReportsThenRemoves(t *testing.T) {
	_, imagesDir := retiredRootFixture(t)

	dry, err := sweepRetiredCacheRoots(true)
	if err != nil {
		t.Fatalf("sweepRetiredCacheRoots(dry): %v", err)
	}
	if len(dry) != 1 || dry[0].Name != "images" {
		t.Fatalf("the record must yield exactly the declared root, got %+v", dry)
	}
	if !dry[0].Present || dry[0].Removed {
		t.Fatalf("dry run = %+v, want present and NOT removed", dry[0])
	}
	if want := int64(4096); dry[0].Bytes != want {
		t.Fatalf("dry-run bytes = %d, want %d (the two 2048-byte blobs)", dry[0].Bytes, want)
	}
	if _, err := os.Stat(imagesDir); err != nil {
		t.Fatalf("the dry run removed the root (%v) — it must report only", err)
	}
	if dry[0].Migration == "" || dry[0].Reason == "" {
		t.Fatalf("the record must carry why it is retired and where the content went: %+v", dry[0])
	}

	real, err := sweepRetiredCacheRoots(false)
	if err != nil {
		t.Fatalf("sweepRetiredCacheRoots(real): %v", err)
	}
	if !real[0].Present || !real[0].Removed {
		t.Fatalf("real run = %+v, want present and removed", real[0])
	}
	if _, err := os.Stat(imagesDir); !os.IsNotExist(err) {
		t.Fatalf("the real run left the retired root at %q (stat err=%v)", imagesDir, err)
	}

	again, err := sweepRetiredCacheRoots(false)
	if err != nil {
		t.Fatalf("a repeated sweep must be a no-op, not an error: %v", err)
	}
	if again[0].Present || again[0].Removed {
		t.Fatalf("repeated sweep = %+v, want absent and not removed", again[0])
	}
}

// TestRetiredCacheRootSweepTouchesNothingElse is the arm that keeps this a RETIREMENT rather than a
// cache wipe: a live named store beside the retired root — its blobs, its index and a marker-carrying
// child layout — survives untouched.
func TestRetiredCacheRootSweepTouchesNothingElse(t *testing.T) {
	cacheRoot, _ := retiredRootFixture(t)
	store := filepath.Join(cacheRoot, "registry")
	if err := os.MkdirAll(filepath.Join(store, "blobs", "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "index.json"), []byte(`{"schemaVersion":2,"manifests":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "blobs", "sha256", strings.Repeat("c", 64)), []byte("live blob"), 0o644); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(store, "some_ref_v1")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := sweepRetiredCacheRoots(false); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	for _, p := range []string{
		filepath.Join(store, "index.json"),
		filepath.Join(store, "blobs", "sha256", strings.Repeat("c", 64)),
		filepath.Join(child, "oci-layout"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("the retirement sweep took %s (%v) — it must touch only the DECLARED retired root", p, err)
		}
	}
}

// TestRetiredRootNamesCannotEscapeTheCacheRoot pins the invariant the sweep relies on rather than
// re-implementing it: every name in the record is resolved through storekit.StoreDir, which rejects
// a separator or a `..`, so a bad entry in the record can never remove something outside the cache
// root.
func TestRetiredRootNamesCannotEscapeTheCacheRoot(t *testing.T) {
	retiredRootFixture(t)
	for _, name := range []string{"../evil", "a/b", "..", ""} {
		if _, err := storekit.StoreDir(name); err == nil {
			t.Fatalf("StoreDir(%q) must refuse a name that is not one path element", name)
		}
	}
	for _, r := range retiredCacheRoots() {
		if _, err := storekit.StoreDir(r.Name); err != nil {
			t.Fatalf("the record's own entry %q does not resolve: %v", r.Name, err)
		}
	}
}

// TestCleanReportPrintsTheRetiredRoots pins the OPERATOR surface: the `--cache` report names each
// retired root's state — would-remove under a dry run, removed on a real run, absent on a re-run —
// because a silent omission would read as a sweep with nothing to do.
func TestCleanReportPrintsTheRetiredRoots(t *testing.T) {
	// The tag IS the verb, exactly as the sibling store line uses it, so each arm is rendered the
	// way the CLI renders it: the dry run passes the dry tag, a real run the done tag.
	dry := retentionOutcome{RetiredRoots: []retiredRootOutcome{
		{Name: "images", Dir: "/cache/images", Bytes: 1024, Present: true, Reason: "r", Migration: "m"},
		{Name: "images", Dir: "/cache/images", Reason: "r", Migration: "m"},
	}}
	var sb strings.Builder
	if err := printRetentionResult(&sb, "would reclaim", false, false, false, true, false, dry); err != nil {
		t.Fatalf("printRetentionResult(dry): %v", err)
	}
	got := sb.String()
	for _, want := range []string{"cache retired images: would reclaim 1.0 KiB", "cache retired images: absent at /cache/images (already swept)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the dry report does not carry %q:\n%s", want, got)
		}
	}

	real := retentionOutcome{RetiredRoots: []retiredRootOutcome{
		{Name: "images", Dir: "/cache/images", Bytes: 2048, Present: true, Removed: true, Reason: "r", Migration: "m"},
	}}
	sb.Reset()
	if err := printRetentionResult(&sb, "reclaimed", false, false, false, true, false, real); err != nil {
		t.Fatalf("printRetentionResult(real): %v", err)
	}
	if got := sb.String(); !strings.Contains(got, "cache retired images: reclaimed 2.0 KiB") {
		t.Fatalf("the real report does not carry the reclaimed line:\n%s", got)
	}
}
