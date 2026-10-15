package clean

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/opencharly/sdk/storekit"
)

// TestGCCacheStoresReachesTheStorekitRoot is the coverage for HALF TWO of
// gcCacheStores — the CACHE-rooted `storekit` stores that the config-rooted loop
// cannot see. It is a separate test because the two halves run under DIFFERENT
// roots, and a single test that pins one root cannot exercise both:
//
//	TestGCCacheStores             sets CHARLY_CACHE_DIR -> both resolvers agree ->
//	                              half two SKIPS, correctly, and half one is covered.
//	TestGCCacheStoresReaches…     leaves it UNSET and points XDG_CACHE_HOME and
//	                              XDG_CONFIG_HOME apart -> half two RUNS and is covered.
//
// Neither test alone covers the function, and that is the point: the skip in half two
// is a real branch with a real reason (the roots coincide when the override is set),
// so it needs its own fixture rather than a mutation of the other test's.
//
// FAILS WITHOUT the change: before it, gcCacheStores returned nothing for a
// storekit store — the store simply was not enumerated — so the assertions on
// RemovedBlobs/RemovedBytes and on the surviving blob all fail.
func TestGCCacheStoresReachesTheStorekitRoot(t *testing.T) {
	cacheRoot := t.TempDir()
	cfgRoot := t.TempDir()

	// The roots must genuinely differ, or this test would silently become a second
	// copy of TestGCCacheStores. Assert the fixture's premise before relying on it.
	t.Setenv("CHARLY_CACHE_DIR", "")
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	t.Setenv("XDG_CONFIG_HOME", cfgRoot)
	if sameStoreRoot() {
		t.Fatalf("fixture is wrong: the two roots still coincide (%q) — this test cannot "+
			"exercise half two", cacheRoot)
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
	// The layout marker, written by the fixture because `storekit` itself never writes
	// it: PutBlob/PutEntry create `blobs/` and `index.json` and stop there, while
	// NamedStores() enumerates on `oci-layout` alone (measured — a store created
	// through the API's own writers is invisible to its own enumeration until the
	// marker exists). spec/cache's sibling writes it in its Layout constructor, so
	// this is a gap in storekit rather than a convention the caller should know.
	// Filed as its own finding; the test needs the marker to be a store at all.
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
	dry, err := gcCacheStores(true)
	if err != nil {
		t.Fatalf("gcCacheStores(dry): %v", err)
	}
	if len(dry) != 1 || dry[0].Name != "storekit-store" {
		t.Fatalf("stores = %+v, want exactly [storekit-store] from the cache root", dry)
	}
	if dry[0].Entries != 1 {
		t.Fatalf("Entries = %d, want 1 (only the current entry is in the index)", dry[0].Entries)
	}
	// TWO blobs, not one, and the count is the LESS interesting half of it: a re-put
	// orphans the superseded PAYLOAD and the superseded ENTRY MANIFEST, because the
	// index keeps one descriptor per key and the older manifest is a blob like any
	// other. Measured here as 158 bytes where the payload alone is 22 — the extra 136
	// is the old manifest. Asserting the payload's bytes are INCLUDED rather than
	// equal to the total states the property without hard-coding the manifest's size,
	// which is a detail of storekit's own encoding.
	if dry[0].RemovedBlobs != 2 {
		t.Fatalf("RemovedBlobs = %d, want 2 (the superseded payload AND its manifest)", dry[0].RemovedBlobs)
	}
	if dry[0].RemovedBytes < int64(len("the-superseded-payload")) {
		t.Fatalf("RemovedBytes = %d, want at least the payload's %d", dry[0].RemovedBytes, len("the-superseded-payload"))
	}
	// A dry run touches nothing: the superseded blob is still on disk.
	if _, err := os.Stat(filepath.Join(s.Dir(), "blobs", "sha256", digestHex(oldDigest))); err != nil {
		t.Fatalf("dry-run removed a blob: %v", err)
	}

	// Real run: the same set, now removed — and the LIVE payload survives, which is
	// the assertion that would catch a sweep that subtracted the wrong set.
	real, err := gcCacheStores(false)
	if err != nil {
		t.Fatalf("gcCacheStores: %v", err)
	}
	if len(real) != 1 || real[0].RemovedBlobs != dry[0].RemovedBlobs || real[0].RemovedBytes != dry[0].RemovedBytes {
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
