package clean

import (
	"testing"

	"github.com/opencharly/spec/cache"
)

// TestGCCacheStores is the deterministic coverage for the `cache` category
// engine path (gcCacheStores): it enumerates the named stores under an isolated
// cache root, reclaims each store's unreferenced blobs, and reports the live
// entry count + reclaimed figures. FAILS without the category (gcCacheStores
// undefined).
func TestGCCacheStores(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CHARLY_CACHE_DIR", root)

	// Two stores: one with a superseded (unreferenced) blob, one fresh.
	l := cache.OpenNamedLayout("project")
	if err := l.Put("k", cache.Entry{Payload: []byte("old-large-payload")}); err != nil {
		t.Fatal(err)
	}
	if err := l.Put("k", cache.Entry{Payload: []byte("new")}); err != nil {
		t.Fatal(err)
	}
	if err := cache.OpenNamedLayout("materialized").Put("m", cache.Entry{Payload: []byte("v")}); err != nil {
		t.Fatal(err)
	}

	// Dry-run: reports the reclaimable set, removes nothing.
	dry, err := gcCacheStores(true)
	if err != nil {
		t.Fatalf("gcCacheStores(dry): %v", err)
	}
	if len(dry) != 2 || dry[0].Name != "materialized" || dry[1].Name != "project" {
		t.Fatalf("stores = %+v, want [materialized project] sorted", dry)
	}
	if dry[1].Entries != 1 || dry[1].RemovedBlobs == 0 || dry[1].RemovedBytes == 0 {
		t.Fatalf("project store dry-run = %+v, want 1 entry + a reported reclaim", dry[1])
	}
	// The store is untouched (the live entry is still there).
	if _, ok := cache.OpenNamedLayout("project").Get("k"); !ok {
		t.Fatal("dry-run must not remove the live entry")
	}

	// Real GC: same set, now removed.
	real, err := gcCacheStores(false)
	if err != nil {
		t.Fatalf("gcCacheStores: %v", err)
	}
	if real[1].RemovedBlobs != dry[1].RemovedBlobs {
		t.Fatalf("real removed %d blobs, dry-run predicted %d", real[1].RemovedBlobs, dry[1].RemovedBlobs)
	}
	if _, ok := cache.OpenNamedLayout("project").Get("k"); !ok {
		t.Fatal("GC must keep the live entry")
	}

	// A cache root with no stores is an empty list, never an error.
	t.Setenv("CHARLY_CACHE_DIR", t.TempDir())
	if got, err := gcCacheStores(false); err != nil || len(got) != 0 {
		t.Fatalf("empty root = %+v, %v; want empty, nil", got, err)
	}
}
