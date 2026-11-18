package clean

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/opencharly/sdk/storekit"
	"github.com/opencharly/spec/cache"
)

// storekitMustCacheDir resolves the storekit cache root or fails the test.
func storekitMustCacheDir(t *testing.T) string {
	t.Helper()
	dir, err := storekit.CacheDir()
	if err != nil {
		t.Fatalf("storekit.CacheDir: %v", err)
	}
	return dir
}

// TestGCCacheStoresReachesTheStorekitRoot is the coverage for the CACHE-rooted half of
// gcCacheStores: a storekit store is enumerated, its reader is chosen from its OWN index, and its
// superseded payload is reclaimed by the store's own reachability walk.
//
// It is a separate test from TestGCCacheStores because the two stores there are `spec/cache`
// layouts, and a single fixture cannot prove both ROUTES. (Its old form asserted that the cache
// half SKIPPED whenever the two roots coincided; that skip WAS the defect of
// opencharly/plugin-clean#27 — the storekit half is now never skipped — and the coincident-root
// case is covered by TestCleanCacheImageStoreSurvivesTheOneRootOverride.)
//
// FAILS WITHOUT the change: before the storekit enumeration existed, gcCacheStores returned
// nothing for a storekit store — the store was not enumerated at all — so the assertions on
// RemovedBlobs/RemovedBytes and on the surviving blob all fail.
func TestGCCacheStoresReachesTheStorekitRoot(t *testing.T) {
	cacheRoot := t.TempDir()

	t.Setenv("CHARLY_CACHE_DIR", "")
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	if storekitMustCacheDir(t) == "" {
		t.Fatal("fixture is wrong: the storekit cache root did not resolve")
	}
	// The two enumerators now share ONE resolver (spec/storeroot, D-STORE-1), so the roots always
	// coincide and `discoverStores` DEDUPES them into one inventory. Assert the premise rather
	// than assume it: if a future resolver change pulled them apart, this test would still be
	// valid, but the dedupe it relies on would not be the thing being exercised.
	if cfgRoot, cerr := cache.StoreRoot(); cerr == nil {
		if cfgAbs, aerr := filepath.Abs(cfgRoot); aerr == nil && cfgAbs == filepath.Clean(storekitMustCacheDir(t)) {
			t.Logf("the two store roots coincide at %s (the D-STORE-1 one-resolver case); discoverStores dedupes them", cfgAbs)
		}
	}

	// A storekit store with a superseded payload: the documented leak. PutEntry
	// replaces the index pointer and leaves the previous payload as a blob, which is
	// exactly what UnreferencedBlobs must name.
	s, err := storekit.OpenStore("storekit-store")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if err := os.MkdirAll(s.Dir(), 0o755); err != nil {
		t.Fatalf("mkdir store: %v", err)
	}
	// The layout marker. `storekit` writes it from `indexEntry` (its ONE index writer, which
	// calls EnsureLayout) as of opencharly/sdk#556; the fixture still writes it explicitly so the
	// test does not depend on which writer created the store, and so the marker's presence is a
	// property this test asserts rather than one it inherits.
	if err := os.WriteFile(filepath.Join(s.Dir(), storekit.LayoutFile), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
		t.Fatalf("write %s marker: %v", storekit.LayoutFile, err)
	}
	oldDigest, err := s.PutBlob([]byte("the-superseded-payload"))
	if err != nil {
		t.Fatalf("PutBlob(old): %v", err)
	}
	if _, err := s.PutEntry(storekit.Entry{Key: "k", MediaType: "application/vnd.test+json", Payload: oldDigest}); err != nil {
		t.Fatalf("PutEntry(old): %v", err)
	}
	liveDigest, err := s.PutBlob([]byte("the-live-payload"))
	if err != nil {
		t.Fatalf("PutBlob(live): %v", err)
	}
	if _, err := s.PutEntry(storekit.Entry{Key: "k", MediaType: "application/vnd.test+json", Payload: liveDigest}); err != nil {
		t.Fatalf("PutEntry(live): %v", err)
	}

	// Dry run: the store is reached, the unreferenced payload is reported, and the
	// number the dry run reports is the number the real run must later remove.
	dry, _, err := gcCacheStores(true)
	if err != nil {
		t.Fatalf("gcCacheStores(dry): %v", err)
	}
	if len(dry) != 1 || dry[0].Info.Name != "storekit-store" {
		t.Fatalf("stores = %+v, want exactly [storekit-store] from the cache root", dry)
	}
	if dry[0].Info.Entries != 1 {
		t.Fatalf("Entries = %d, want 1 (only the current entry is in the index)", dry[0].Info.Entries)
	}
	// TWO blobs, not one, and the count is the LESS interesting half of it: a re-put
	// orphans the superseded PAYLOAD and the superseded ENTRY MANIFEST, because the
	// index keeps one descriptor per key and the older manifest is a blob like any
	// other. Measured here as 158 bytes where the payload alone is 22 — the extra 136
	// is the old manifest. Asserting the payload's bytes are INCLUDED rather than
	// equal to the total states the property without hard-coding the manifest's size,
	// which is a detail of storekit's own encoding.
	if dry[0].Info.RemovedBlobs != 2 {
		t.Fatalf("RemovedBlobs = %d, want 2 (the superseded payload AND its manifest)", dry[0].Info.RemovedBlobs)
	}
	if dry[0].Info.RemovedBytes < int64(len("the-superseded-payload")) {
		t.Fatalf("RemovedBytes = %d, want at least the payload's %d", dry[0].Info.RemovedBytes, len("the-superseded-payload"))
	}
	// A dry run touches nothing: the superseded blob is still on disk.
	if _, err := os.Stat(filepath.Join(s.Dir(), "blobs", "sha256", digestHex(oldDigest))); err != nil {
		t.Fatalf("dry-run removed a blob: %v", err)
	}

	// Real run: the same set, now removed — and the LIVE payload survives, which is
	// the assertion that would catch a sweep that subtracted the wrong set.
	real, _, err := gcCacheStores(false)
	if err != nil {
		t.Fatalf("gcCacheStores: %v", err)
	}
	if len(real) != 1 || real[0].Info.RemovedBlobs != dry[0].Info.RemovedBlobs || real[0].Info.RemovedBytes != dry[0].Info.RemovedBytes {
		t.Fatalf("real = %+v, dry-run = %+v: the two must agree", real, dry)
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), "blobs", "sha256", digestHex(oldDigest))); !os.IsNotExist(err) {
		t.Fatalf("the superseded blob survived a real run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), "blobs", "sha256", digestHex(liveDigest))); err != nil {
		t.Fatalf("the LIVE payload was removed: %v", err)
	}
}

// digestHex strips the `sha256:` prefix so a digest can be used as a path element
// under blobs/sha256/. The store's own blobPath does this internally; the test needs
// the path to assert on disk, and spelling it here keeps the assertion independent of
// the production helper it is testing.
func digestHex(dgst string) string {
	for i := 0; i < len(dgst); i++ {
		if dgst[i] == ':' {
			return dgst[i+1:]
		}
	}
	return dgst
}
