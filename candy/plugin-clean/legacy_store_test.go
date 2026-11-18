package clean

// legacy_store_test.go — opencharly/plugin-clean#30: the PRE-RELOCATION image store at the data root
// must be INGESTED into the current `registry` store, never removed, and the report must name it
// before anything moves.
//
// Every arm isolates BOTH roots (CHARLY_CACHE_DIR for the store, XDG_DATA_HOME for the legacy tree)
// and writes nothing outside its temporary directory (D-STORE-8).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencharly/sdk/storekit"
)

// legacyFixture builds the pre-relocation image store the way W3.6's writer did: an OCI layout whose
// `index.json` descriptors ARE image manifests, each annotated with the reference it was built as
// and the build time — the shape measured on a real host (202 entries, 66 families).
func legacyFixture(t *testing.T, dataRoot string, refs ...string) (dir string, blobs int) {
	t.Helper()
	dir = filepath.Join(dataRoot, "charly", "images")
	blobsDir := filepath.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		t.Fatalf("mkdir legacy blobs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, storekit.LayoutFile), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
		t.Fatalf("legacy marker: %v", err)
	}
	type descriptor struct {
		MediaType   string            `json:"mediaType"`
		Digest      string            `json:"digest"`
		Size        int64             `json:"size"`
		Annotations map[string]string `json:"annotations"`
	}
	var descs []descriptor
	for i, ref := range refs {
		layer := []byte(fmt.Sprintf("layer for %s", ref))
		cfg := []byte(`{"architecture":"amd64","os":"linux"}`)
		layerDigest, lerr := putLegacyBlob(t, blobsDir, layer)
		if lerr != nil {
			t.Fatalf("legacy layer: %v", lerr)
		}
		cfgDigest, cerr := putLegacyBlob(t, blobsDir, cfg)
		if cerr != nil {
			t.Fatalf("legacy config: %v", cerr)
		}
		manifest, merr := json.Marshal(map[string]any{
			"schemaVersion": 2,
			"mediaType":     "application/vnd.oci.image.manifest.v1+json",
			"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": cfgDigest, "size": len(cfg)},
			"layers":        []any{map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": layerDigest, "size": len(layer)}},
		})
		if merr != nil {
			t.Fatalf("legacy manifest: %v", merr)
		}
		manifestDigest, perr := putLegacyBlob(t, blobsDir, manifest)
		if perr != nil {
			t.Fatalf("legacy manifest blob: %v", perr)
		}
		descs = append(descs, descriptor{
			MediaType: "application/vnd.oci.image.manifest.v1+json",
			Digest:    manifestDigest,
			Size:      int64(len(manifest)),
			Annotations: map[string]string{
				storekit.AnnotationImageName: ref,
				storekit.AnnotationCreated:   fmt.Sprintf("2026-11-%02dT10:00:00Z", 10+i),
			},
		})
	}
	raw, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     storekit.MediaTypeOCIIndex,
		"manifests":     descs,
	})
	if err != nil {
		t.Fatalf("legacy index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), raw, 0o644); err != nil {
		t.Fatalf("write legacy index: %v", err)
	}
	return dir, len(descs) * 3
}

// putLegacyBlob writes one content-addressed legacy blob and returns its digest.
func putLegacyBlob(t *testing.T, blobsDir string, data []byte) (string, error) {
	t.Helper()
	dgst, err := storekit.DigestOf(data)
	if err != nil {
		return "", err
	}
	hex := strings.TrimPrefix(dgst, "sha256:")
	if werr := os.WriteFile(filepath.Join(blobsDir, hex), data, 0o644); werr != nil {
		return "", werr
	}
	return dgst, nil
}

// legacyRoots isolates both roots and returns the data root the legacy tree lives under.
func legacyRoots(t *testing.T) (cacheRoot, dataRoot string) {
	t.Helper()
	cacheRoot = t.TempDir()
	dataRoot = t.TempDir()
	t.Setenv("CHARLY_CACHE_DIR", cacheRoot)
	t.Setenv("XDG_DATA_HOME", dataRoot)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	return cacheRoot, dataRoot
}

// TestRetiredRootIngestsTheLegacyDataRootStore is #30's positive arm: a REAL sweep re-publishes every
// entry of the pre-relocation store into the `registry` store, every reference resolves to content
// that EXISTS afterwards, and only then is the source tree removed.
//
// FAILS WITHOUT THE CHANGE: no retirement row names the data-root path, so the sweep reports nothing
// and the tree sits there unhandled — the issue's own measurement.
func TestRetiredRootIngestsTheLegacyDataRootStore(t *testing.T) {
	cacheRoot, dataRoot := legacyRoots(t)
	refs := []string{"ghcr.io/opencharly/arch:2026.322.1039", "ghcr.io/opencharly/comfyui:2026.321.2347"}
	src, _ := legacyFixture(t, dataRoot, refs...)

	out, err := sweepRetiredCacheRoots(false)
	if err != nil {
		t.Fatalf("sweepRetiredCacheRoots: %v", err)
	}
	var legacy *retiredRootOutcome
	for i := range out {
		if out[i].Name == "data-images" {
			legacy = &out[i]
		}
	}
	if legacy == nil {
		t.Fatalf("the report does not carry the legacy data-root row: %+v", out)
	}
	if !legacy.Present || !legacy.Ingested || !legacy.Removed {
		t.Fatalf("legacy = %+v, want present, ingested and removed", *legacy)
	}
	if legacy.Entries != len(refs) {
		t.Fatalf("Entries = %d, want %d", legacy.Entries, len(refs))
	}
	if legacy.Bytes <= 0 {
		t.Fatalf("Bytes = %d, want the tree's measured size", legacy.Bytes)
	}
	if _, serr := os.Stat(src); !os.IsNotExist(serr) {
		t.Fatalf("the source tree survived the verified ingest: %v", serr)
	}

	// The entries are in the REGISTRY store and every reference resolves to bytes that exist.
	store := storekit.OpenStoreDir(filepath.Join(cacheRoot, storekit.ImageStoreName))
	entries, eerr := store.Entries()
	if eerr != nil {
		t.Fatalf("Entries: %v", eerr)
	}
	if len(entries) != len(refs) {
		t.Fatalf("destination holds %d entry(ies), want %d", len(entries), len(refs))
	}
	img, oerr := storekit.OpenImageStore()
	if oerr != nil {
		t.Fatalf("OpenImageStore: %v", oerr)
	}
	for _, ref := range refs {
		dgst, ok, lerr := img.Lookup(ref)
		if lerr != nil || !ok {
			t.Fatalf("Lookup(%q) = %q, %v, %v; want the ingested entry to resolve", ref, dgst, ok, lerr)
		}
		if _, rerr := store.ReadBlob(dgst); rerr != nil {
			t.Fatalf("the ingested reference resolves to missing content: %v", rerr)
		}
	}
	// And the blobs MOVED with it: every blob the ingested entries reach is present, so the store's
	// own reader finds nothing unreferenced.
	if unreferenced, uerr := store.UnreferencedBlobs(); uerr != nil || len(unreferenced) != 0 {
		t.Fatalf("the ingested store reports unreferenced blobs (%v, %d): the graph did not come across",
			uerr, len(unreferenced))
	}
}

// TestRetiredRootLegacyIngestIsIdempotent is #30's second-run property: the path is gone, so the
// sweep reports it absent and touches nothing.
func TestRetiredRootLegacyIngestIsIdempotent(t *testing.T) {
	_, dataRoot := legacyRoots(t)
	legacyFixture(t, dataRoot, "ghcr.io/opencharly/arch:2026.322.1039")

	if _, err := sweepRetiredCacheRoots(false); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	second, err := sweepRetiredCacheRoots(false)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	for _, r := range second {
		if r.Name != "data-images" {
			continue
		}
		if r.Present || r.Ingested || r.Removed || r.Entries != 0 {
			t.Fatalf("second run = %+v, want present=false and nothing done", r)
		}
	}
}

// TestRetiredRootLegacyIngestDryRunNamesItAndTouchesNothing: property 1 of #30 — the report names the
// path, its entry count and its bytes, and a dry run moves nothing.
func TestRetiredRootLegacyIngestDryRunNamesItAndTouchesNothing(t *testing.T) {
	cacheRoot, dataRoot := legacyRoots(t)
	src, _ := legacyFixture(t, dataRoot, "ghcr.io/opencharly/arch:2026.322.1039")

	dry, err := sweepRetiredCacheRoots(true)
	if err != nil {
		t.Fatalf("dry sweep: %v", err)
	}
	var legacy *retiredRootOutcome
	for i := range dry {
		if dry[i].Name == "data-images" {
			legacy = &dry[i]
		}
	}
	if legacy == nil || !legacy.Present || legacy.Entries != 1 || legacy.Bytes <= 0 {
		t.Fatalf("dry run = %+v, want the legacy tree named with its entry count and bytes", legacy)
	}
	if legacy.Ingested || legacy.Removed {
		t.Fatalf("dry run claims work it did not do: %+v", *legacy)
	}
	if _, serr := os.Stat(src); serr != nil {
		t.Fatalf("the dry run touched the source tree: %v", serr)
	}
	if ents, _ := storekit.OpenStoreDir(filepath.Join(cacheRoot, storekit.ImageStoreName)).Entries(); len(ents) != 0 {
		t.Fatalf("the dry run ingested %d entry(ies)", len(ents))
	}
	// The REPORT line says it, with the destination and the fact that the source stays.
	var buf bytes.Buffer
	if perr := printRetentionResult(&buf, "would remove", cleanPlan{cache: true}, retentionOutcome{
		CacheSweep:    []cacheStoreOutcome{},
		RetiredRoots:  []retiredRootOutcome{*legacy},
		RefusedStores: nil,
	}); perr != nil {
		t.Fatalf("print: %v", perr)
	}
	got := buf.String()
	if !strings.Contains(got, "would ingest 1 entry(ies)") || !strings.Contains(got, "the source tree stays until the ingest verifies") {
		t.Fatalf("the report does not name the pending ingest:\n%s", got)
	}
}

// TestRetiredRootLegacyIngestRefusesACorruptStore: property 2 — never blind-delete. An index the
// sweep cannot read is content it cannot account for, so it is refused BY NAME and left exactly as
// it was.
func TestRetiredRootLegacyIngestRefusesACorruptStore(t *testing.T) {
	_, dataRoot := legacyRoots(t)
	src := filepath.Join(dataRoot, "charly", "images")
	if err := os.MkdirAll(filepath.Join(src, "blobs", "sha256"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "index.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("index: %v", err)
	}
	blob := filepath.Join(src, "blobs", "sha256", strings.Repeat("c", 64))
	if err := os.WriteFile(blob, []byte("real bytes"), 0o644); err != nil {
		t.Fatalf("blob: %v", err)
	}

	_, err := sweepRetiredCacheRoots(false)
	if err == nil {
		t.Fatal("the sweep ingested/removed a store whose index it could not read")
	}
	if !strings.Contains(err.Error(), src) {
		t.Fatalf("refusal = %q, want it to name the tree", err)
	}
	if _, serr := os.Stat(blob); serr != nil {
		t.Fatalf("a refused legacy store lost bytes: %v", serr)
	}
}

// TestRetiredRootLegacyIngestRefusesAnEntryItCannotName: a legacy descriptor with no
// `io.containerd.image.name` cannot be re-keyed under a name the sweep would have to invent.
func TestRetiredRootLegacyIngestRefusesAnEntryItCannotName(t *testing.T) {
	_, dataRoot := legacyRoots(t)
	src, _ := legacyFixture(t, dataRoot, "ghcr.io/opencharly/arch:2026.322.1039")
	raw, err := os.ReadFile(filepath.Join(src, "index.json"))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	var idx map[string]any
	if uerr := json.Unmarshal(raw, &idx); uerr != nil {
		t.Fatalf("parse index: %v", uerr)
	}
	descs := idx["manifests"].([]any)
	descs[0].(map[string]any)["annotations"] = map[string]any{}
	patched, _ := json.Marshal(idx)
	if werr := os.WriteFile(filepath.Join(src, "index.json"), patched, 0o644); werr != nil {
		t.Fatalf("rewrite index: %v", werr)
	}

	_, err = sweepRetiredCacheRoots(false)
	if err == nil {
		t.Fatal("the sweep ingested an entry whose reference it could not name")
	}
	if !strings.Contains(err.Error(), storekit.AnnotationImageName) {
		t.Fatalf("refusal = %q, want it to name the missing annotation", err)
	}
	if _, serr := os.Stat(src); serr != nil {
		t.Fatalf("a refused legacy store was removed: %v", serr)
	}
}

// TestRetiredRootLegacyRowIsTheOnlyPlaceTheDataPathAppears is the resolver answer, asserted: the
// pre-relocation path is a RECORDED FACT in the retirement record and is expanded by exactly ONE
// function. A second appearance anywhere in the package would be a second copy of a path the
// contract has no root for.
func TestRetiredRootLegacyRowIsTheOnlyPlaceTheDataPathAppears(t *testing.T) {
	_, dataRoot := legacyRoots(t)
	dir, err := retiredRootDir(retiredCacheRoot{Place: retiredInLegacyDataRoot, Elems: []string{"charly", "images"}})
	if err != nil {
		t.Fatalf("retiredRootDir: %v", err)
	}
	if want := filepath.Join(dataRoot, "charly", "images"); dir != want {
		t.Fatalf("retiredRootDir = %q, want %q", dir, want)
	}
	// The cache-root row still resolves through the ONE cache-root resolver.
	if dir, err := retiredRootDir(retiredCacheRoot{Place: retiredInCacheRoot, Elems: []string{"images"}}); err != nil {
		t.Fatalf("cache-root row: %v", err)
	} else if want := filepath.Join(os.Getenv("CHARLY_CACHE_DIR"), "images"); dir != want {
		t.Fatalf("cache-root dir = %q, want %q", dir, want)
	}
	// The record declares the elements; the resolver expands them. Nothing in Go spells out the
	// absolute data path.
	for _, r := range retiredCacheRoots() {
		if r.Place == retiredInLegacyDataRoot && len(r.Elems) != 2 {
			t.Fatalf("legacy row carries %v, want the two elements the old version wrote", r.Elems)
		}
	}
}

// TestRetiredRootLegacyIngestRefusesAnIndexNamingNoEntries: an index that names nothing while the tree
// holds blobs is content this ingest cannot account for — the blobs would be removed with no record of
// what they were.
func TestRetiredRootLegacyIngestRefusesAnIndexNamingNoEntries(t *testing.T) {
	_, dataRoot := legacyRoots(t)
	src := filepath.Join(dataRoot, "charly", "images")
	blobsDir := filepath.Join(src, "blobs", "sha256")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "index.json"), []byte(`{"schemaVersion":2,"manifests":[]}`), 0o644); err != nil {
		t.Fatalf("index: %v", err)
	}
	orphan := filepath.Join(blobsDir, strings.Repeat("d", 64))
	if err := os.WriteFile(orphan, []byte("bytes nothing names"), 0o644); err != nil {
		t.Fatalf("blob: %v", err)
	}

	_, err := sweepRetiredCacheRoots(false)
	if err == nil {
		t.Fatal("the sweep removed a legacy tree whose index named no entries")
	}
	if !strings.Contains(err.Error(), "names no entries") {
		t.Fatalf("refusal = %q, want it to say the index names no entries", err)
	}
	if _, serr := os.Stat(orphan); serr != nil {
		t.Fatalf("an unaccountable legacy tree lost bytes: %v", serr)
	}
}

// TestRetiredRootLegacyIngestRefusesAMissingManifest: an index entry whose manifest blob is already
// gone is a tree this ingest cannot faithfully re-publish; removing it afterwards would delete the only
// record of an image that never made it across.
func TestRetiredRootLegacyIngestRefusesAMissingManifest(t *testing.T) {
	_, dataRoot := legacyRoots(t)
	src, _ := legacyFixture(t, dataRoot, "ghcr.io/opencharly/arch:2026.322.1039")

	raw, err := os.ReadFile(filepath.Join(src, "index.json"))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	var idx struct {
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
	}
	if uerr := json.Unmarshal(raw, &idx); uerr != nil {
		t.Fatalf("parse index: %v", uerr)
	}
	if len(idx.Manifests) != 1 {
		t.Fatalf("fixture is wrong: %d descriptor(s), want 1", len(idx.Manifests))
	}
	missing := idx.Manifests[0].Digest
	blobPath := filepath.Join(src, "blobs", filepath.FromSlash(strings.Replace(missing, ":", "/", 1)))
	if rerr := os.Remove(blobPath); rerr != nil {
		t.Fatalf("remove the manifest blob: %v", rerr)
	}

	_, err = sweepRetiredCacheRoots(false)
	if err == nil {
		t.Fatal("the sweep ingested a tree whose indexed manifest it does not hold")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("refusal = %q, want it to name the missing digest %s", err, missing)
	}
	if _, serr := os.Stat(src); serr != nil {
		t.Fatalf("a refused legacy tree was removed: %v", serr)
	}
}
