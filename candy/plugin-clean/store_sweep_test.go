package clean

// store_sweep_test.go — the coverage for D-STORE-7's entry policy (the `store` category) and for
// the store-IDENTITY routing that keeps an image store out of an image-blind reader
// (opencharly/plugin-clean#27).
//
// Every case injects its roots (CHARLY_CACHE_DIR, the quadlet directory, the engine collector) and
// writes nothing outside its own temporary directory (D-STORE-8).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"cuelang.org/go/cue"

	"github.com/opencharly/sdk/kit"
	"github.com/opencharly/sdk/storekit"
	"github.com/opencharly/spec/cache"
	"github.com/opencharly/spec/spec"
)

// --- fixtures ------------------------------------------------------------------------------

// putImageEntry lands ONE image in the `registry` store at cacheRoot the way a build drive does:
// a layer, a config, the image manifest, the artifact index (the entry's PAYLOAD) and the entry
// manifest — five blobs, one entry, keyed by the artifact index's digest and annotated with the
// reference the build was asked for.
func putImageEntry(t *testing.T, cacheRoot, ref, created, layer string) (key string, blobs []string) {
	t.Helper()
	s := storekit.OpenStoreDir(filepath.Join(cacheRoot, storekit.ImageStoreName))
	if err := s.EnsureLayout(); err != nil {
		t.Fatalf("EnsureLayout: %v", err)
	}
	layerDigest, err := s.PutBlob([]byte(layer))
	if err != nil {
		t.Fatalf("PutBlob(layer): %v", err)
	}
	configDigest, err := s.PutBlob([]byte(`{"architecture":"amd64","os":"linux","config":{}}`))
	if err != nil {
		t.Fatalf("PutBlob(config): %v", err)
	}
	manifest, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest":    configDigest,
			"size":      len(`{"architecture":"amd64","os":"linux","config":{}}`),
		},
		"layers": []any{map[string]any{
			"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
			"digest":    layerDigest,
			"size":      len(layer),
		}},
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	manifestDigest, err := s.PutBlob(manifest)
	if err != nil {
		t.Fatalf("PutBlob(manifest): %v", err)
	}
	index, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests": []any{map[string]any{
			"mediaType": "application/vnd.oci.image.manifest.v1+json",
			"digest":    manifestDigest,
			"size":      len(manifest),
		}},
	})
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	indexDigest, err := s.PutBlob(index)
	if err != nil {
		t.Fatalf("PutBlob(index): %v", err)
	}
	manifestEntry, err := s.PutEntry(storekit.Entry{
		Key:       indexDigest,
		MediaType: storekit.MediaTypeOCIIndex,
		Payload:   indexDigest,
		Annotations: map[string]string{
			storekit.AnnotationImageName: ref,
			storekit.AnnotationCreated:   created,
		},
	})
	if err != nil {
		t.Fatalf("PutEntry: %v", err)
	}
	return indexDigest, []string{manifestEntry, indexDigest, manifestDigest, configDigest, layerDigest}
}

// blobPresent reports whether the store holds a digest as a file.
func blobPresent(t *testing.T, cacheRoot, dgst string) bool {
	t.Helper()
	alg, hex, ok := strings.Cut(dgst, ":")
	if !ok {
		t.Fatalf("malformed digest %q", dgst)
	}
	_, err := os.Stat(filepath.Join(cacheRoot, storekit.ImageStoreName, "blobs", alg, hex))
	return err == nil
}

// seamEngine points the container-engine surface at a fixed answer.
func seamEngine(t *testing.T, refs []string, ids []string, err error) {
	t.Helper()
	orig := listContainerImageRefs
	t.Cleanup(func() { listContainerImageRefs = orig })
	listContainerImageRefs = func(string) (map[string]bool, map[string]bool, error) {
		if err != nil {
			return nil, nil, err
		}
		refSet := map[string]bool{}
		for _, r := range refs {
			refSet[r] = true
		}
		idSet := map[string]bool{}
		for _, d := range ids {
			idSet[normImageID(d)] = true
		}
		return idSet, refSet, nil
	}
}

// seamQuadlets points the quadlet surface at a directory of units.
func seamQuadlets(t *testing.T, dir string) {
	t.Helper()
	orig := quadletDir
	t.Cleanup(func() { quadletDir = orig })
	quadletDir = func() (string, error) { return dir, nil }
}

// seamNoLiveBuild pins the live-build guard to "no build in flight", so a busy host cannot make
// these cases skip for a reason that has nothing to do with them.
func seamNoLiveBuild(t *testing.T) {
	t.Helper()
	orig := liveBuildFloor
	t.Cleanup(func() { liveBuildFloor = orig })
	liveBuildFloor = func() (kit.CalVer, bool, int) { return kit.CalVer{}, false, 0 }
}

// quadletUnit writes ONE charly quadlet unit naming ref.
func quadletUnit(t *testing.T, dir, name, ref string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir quadlets: %v", err)
	}
	unit := fmt.Sprintf("[Container]\nImage=%s\nContainerName=%s\n", ref, name)
	if err := os.WriteFile(filepath.Join(dir, "charly-"+name+".container"), []byte(unit), 0o644); err != nil {
		t.Fatalf("write quadlet: %v", err)
	}
}

// --- D-STORE-7: the decision -----------------------------------------------------------------

// TestStoreSweepEvictsOnlyWhatStateNoLongerReferences is the core arm: an entry a state surface
// names survives, an entry nothing names is evicted, and the RECLAIM takes the evicted entry's
// own manifests and layers with it while the survivor's bytes stay readable.
func TestStoreSweepEvictsOnlyWhatStateNoLongerReferences(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CHARLY_CACHE_DIR", root)
	seamNoLiveBuild(t)

	live, _ := putImageEntry(t, root, "ghcr.io/opencharly/live:2026.322.0001", "2026-11-18T10:00:00Z", "the live layer")
	dead, _ := putImageEntry(t, root, "ghcr.io/opencharly/dead:2026.322.0002", "2026-11-18T11:00:00Z", "the dead layer")

	// State: one QUADLET unit names the live image. Nothing names the other.
	units := t.TempDir()
	quadletUnit(t, units, "live", "ghcr.io/opencharly/live:2026.322.0001")
	seamQuadlets(t, units)
	seamEngine(t, nil, nil, nil)

	out, err := runStoreSweep("podman", false, 1<<40)
	if err != nil {
		t.Fatalf("runStoreSweep: %v", err)
	}
	if out.Evicted != 1 {
		t.Fatalf("Evicted = %d, want 1: kept=%v evicted=%v", out.Evicted, out.kept(), out.evicted())
	}
	if out.kept()[0].Key != live {
		t.Fatalf("the referenced entry was not kept: %+v", out.kept())
	}
	if out.evicted()[0].Key != dead {
		t.Fatalf("the unreferenced entry was not evicted: %+v", out.evicted())
	}
	if !strings.Contains(out.evicted()[0].Reason, "unreferenced") {
		t.Fatalf("eviction reason = %q, want it to name the absence of a reference", out.evicted()[0].Reason)
	}

	// The SURVIVOR's bytes are all still there, and the store still resolves its reference to
	// content that exists — the property #27 destroyed.
	store := storekit.OpenStoreDir(filepath.Join(root, storekit.ImageStoreName))
	for _, d := range []string{live} {
		if !blobPresent(t, root, d) {
			t.Fatalf("the referenced entry's payload %s is gone", d)
		}
	}
	img, err := storekit.OpenImageStore()
	if err != nil {
		t.Fatalf("OpenImageStore: %v", err)
	}
	dgst, ok, lerr := img.Lookup("ghcr.io/opencharly/live:2026.322.0001")
	if lerr != nil || !ok {
		t.Fatalf("Lookup(live) = %q, %v, %v; want the entry to resolve", dgst, ok, lerr)
	}
	if _, rerr := store.ReadBlob(dgst); rerr != nil {
		t.Fatalf("the reference resolves to content that does not exist: %v", rerr)
	}
	// And the evicted entry's LAYER went with it — the reclaim is the whole artifact, not the
	// index pointer alone.
	if blobPresent(t, root, dead) {
		t.Fatalf("the evicted entry's payload %s survived", dead)
	}
}

// TestStoreSweepKeepsAReferencedEntryEvenOverBudget is the budget's hard rule: a budget NEVER
// authorises evicting a referenced entry. The sweep must meet the budget only out of what state
// does not reference, and must SAY when the referenced set alone exceeds it.
func TestStoreSweepKeepsAReferencedEntryEvenOverBudget(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CHARLY_CACHE_DIR", root)
	seamNoLiveBuild(t)

	live, _ := putImageEntry(t, root, "ghcr.io/opencharly/live:2026.322.0001", "2026-11-18T10:00:00Z", "a layer far bigger than the budget")
	putImageEntry(t, root, "ghcr.io/opencharly/dead:2026.322.0002", "2026-11-18T11:00:00Z", "junk")

	units := t.TempDir()
	quadletUnit(t, units, "live", "ghcr.io/opencharly/live:2026.322.0001")
	seamQuadlets(t, units)
	seamEngine(t, nil, nil, nil)

	// A budget of ONE byte: unreachable for any real store, and the one case where a sweep that
	// "honoured the budget" by evicting the referenced entry would look successful.
	out, err := runStoreSweep("podman", false, 1)
	if err != nil {
		t.Fatalf("runStoreSweep: %v", err)
	}
	if out.Evicted != 1 {
		t.Fatalf("Evicted = %d, want exactly the one unreferenced entry", out.Evicted)
	}
	if !blobPresent(t, root, live) {
		t.Fatalf("the referenced entry was evicted to meet the budget — the one thing the budget may never do")
	}
	if out.Retained <= out.Budget {
		t.Fatalf("Retained = %d, Budget = %d: the fixture is wrong — it must be over budget", out.Retained, out.Budget)
	}
	if got := budgetVerdict(out.Retained, out.Budget); !strings.Contains(got, "OVER budget") {
		t.Fatalf("verdict = %q, want it to report the store OVER budget", got)
	}
	// The report line carries the same verdict the sweep computed.
	lines := strings.Join(storeLines(out), "")
	if !strings.Contains(lines, "OVER budget") {
		t.Fatalf("report does not say the store is over budget:\n%s", lines)
	}
}

// TestStoreSweepWithinBudget is the other half of the budget's contract: when the store the sweep
// leaves behind fits, the verdict says so. A sweep that never compared at all would report
// "within budget" here and "within budget" in the case above too — so the two cases together are
// what pin the comparison.
func TestStoreSweepWithinBudget(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CHARLY_CACHE_DIR", root)
	seamNoLiveBuild(t)

	putImageEntry(t, root, "ghcr.io/opencharly/live:2026.322.0001", "2026-11-18T10:00:00Z", "small")
	units := t.TempDir()
	quadletUnit(t, units, "live", "ghcr.io/opencharly/live:2026.322.0001")
	seamQuadlets(t, units)
	seamEngine(t, nil, nil, nil)

	out, err := runStoreSweep("podman", false, 1<<40)
	if err != nil {
		t.Fatalf("runStoreSweep: %v", err)
	}
	if out.Evicted != 0 {
		t.Fatalf("Evicted = %d, want 0", out.Evicted)
	}
	if got := budgetVerdict(out.Retained, out.Budget); got != "within budget" {
		t.Fatalf("verdict = %q, want within budget", got)
	}
}

// TestStoreSweepKeepsAnEntryReferencedByContentAddress pins the DIGEST half of the keep set: the
// engine reports each container's image ID, and an artifact whose content address a container
// names is live even when its reference annotation is one no surface mentions (a retag, a build
// tag an operator dropped).
func TestStoreSweepKeepsAnEntryReferencedByContentAddress(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CHARLY_CACHE_DIR", root)
	seamNoLiveBuild(t)

	key, _ := putImageEntry(t, root, "ghcr.io/opencharly/retagged:2026.322.0003", "2026-11-18T10:00:00Z", "layer")
	seamQuadlets(t, t.TempDir())
	seamEngine(t, nil, []string{key}, nil) // the engine's ImageID is the artifact's content address

	out, err := runStoreSweep("podman", false, 1<<40)
	if err != nil {
		t.Fatalf("runStoreSweep: %v", err)
	}
	if out.Evicted != 0 {
		t.Fatalf("Evicted = %d, want 0: the engine names this artifact's content address", out.Evicted)
	}
	if !strings.Contains(out.kept()[0].Reason, "content address") {
		t.Fatalf("reason = %q, want it to name the content address", out.kept()[0].Reason)
	}
}

// TestStoreSweepEvictsASupersededRebuildOfAReferencedRef pins the TAG → DIGEST resolution: the
// state names ONE TAGGED reference, the store holds two builds under that annotation, and the
// NEWEST is the one the store resolves the name to — so the older build is reclaimable churn, not a
// live artifact.
//
// ⚑ Both sides carry a TAG here on purpose. The previous form of this test used an UNTAGGED
// annotation on both sides, which is why it passed while `planStoreEviction` evicted a TAGGED entry
// whose repository the state named bare — the hole the reviewer found. A bare-vs-bare fixture
// cannot see that hole: with a bare state reference the bare-repository rule keeps every tag, and
// the superseded path is never reached. The tagged form is what exercises Lookup; the tagged-entry
// / bare-reference pair is TestStoreSweepKeepsEveryTagOfABareRepositoryReference's job.
func TestStoreSweepEvictsASupersededRebuildOfAReferencedRef(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CHARLY_CACHE_DIR", root)
	seamNoLiveBuild(t)

	const pinned = "ghcr.io/opencharly/vision-gate:2026.322.0001"
	oldKey, _ := putImageEntry(t, root, pinned, "2026-11-17T10:00:00Z", "old layer")
	newKey, _ := putImageEntry(t, root, pinned, "2026-11-18T10:00:00Z", "new layer")

	units := t.TempDir()
	quadletUnit(t, units, "gate", pinned)
	seamQuadlets(t, units)
	seamEngine(t, nil, nil, nil)

	out, err := runStoreSweep("podman", false, 1<<40)
	if err != nil {
		t.Fatalf("runStoreSweep: %v", err)
	}
	if len(out.kept()) != 1 || out.kept()[0].Key != newKey {
		t.Fatalf("kept = %+v, want only the newest build of the tag", out.kept())
	}
	if len(out.evicted()) != 1 || out.evicted()[0].Key != oldKey {
		t.Fatalf("evicted = %+v, want the superseded build", out.evicted())
	}
	if !strings.Contains(out.evicted()[0].Reason, "superseded") {
		t.Fatalf("reason = %q, want it to say the entry was superseded", out.evicted()[0].Reason)
	}
}

// TestStoreSweepKeepsEveryTagOfABareRepositoryReference is the reviewer's blocking finding made
// falsifiable: a state surface that names a BARE repository — a quadlet's `Image=vision-gate`, or a
// digest-only reference that `normImageRef` folds to a bare repository — must keep that repository's
// TAGGED store entries. The entries here carry tags and the state names only the repository, which is
// exactly the pair the old `if hasTag { continue }` branch evicted.
//
// FAILS WITHOUT THE FIX: both entries fall through to "unreferenced" and are evicted, i.e. a running
// deployment's image is destroyed because the record that kept it carried no tag.
func TestStoreSweepKeepsEveryTagOfABareRepositoryReference(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CHARLY_CACHE_DIR", root)
	seamNoLiveBuild(t)

	first, _ := putImageEntry(t, root, "ghcr.io/opencharly/vision-gate:2026.322.0001", "2026-11-17T10:00:00Z", "first layer")
	second, _ := putImageEntry(t, root, "ghcr.io/opencharly/vision-gate:2026.323.0002", "2026-11-18T10:00:00Z", "second layer")

	// The state names the REPOSITORY, not a build of it.
	units := t.TempDir()
	quadletUnit(t, units, "gate", "ghcr.io/opencharly/vision-gate")
	seamQuadlets(t, units)
	seamEngine(t, nil, nil, nil)

	out, err := runStoreSweep("podman", false, 1<<40)
	if err != nil {
		t.Fatalf("runStoreSweep: %v", err)
	}
	if out.Evicted != 0 {
		t.Fatalf("Evicted = %d, want 0: a bare-repository reference keeps every tag of it (evicted=%+v)", out.Evicted, out.evicted())
	}
	kept := map[string]bool{}
	for _, d := range out.kept() {
		kept[d.Key] = true
	}
	for _, want := range []string{first, second} {
		if !kept[want] {
			t.Fatalf("entry %s was not kept: kept=%+v", shortDigest(want), out.kept())
		}
		if !blobPresent(t, root, want) {
			t.Fatalf("the referenced repository lost the bytes of %s", shortDigest(want))
		}
	}
	if !strings.Contains(out.kept()[0].Reason, "names the repository") {
		t.Fatalf("reason = %q, want it to say the state named the repository", out.kept()[0].Reason)
	}
}

// TestStoreBlobBytesRefusesAnUnmeasurableStore: a store that cannot be LISTED is not a store of zero
// bytes, and the budget verdict must never print "within budget" for a size nobody measured. An
// unmeasurable `blobs` path is the reachable form of that (the directory is a file).
func TestStoreBlobBytesRefusesAnUnmeasurableStore(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "blobs"), []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if got, err := storeBlobBytes(storekit.OpenStoreDir(dir)); err == nil {
		t.Fatalf("storeBlobBytes = %d, nil; want an error for a store whose blobs cannot be listed", got)
	}
}

// TestStoreSweepRefusesAnUnmeasurableStore is the same property at the sweep's boundary: it must
// refuse BEFORE evicting (so nothing is deleted on a store whose size and budget could not be
// reported), and the refusal must say which store and why.
func TestStoreSweepRefusesAnUnmeasurableStore(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CHARLY_CACHE_DIR", root)
	seamNoLiveBuild(t)
	if err := os.MkdirAll(filepath.Join(root, storekit.ImageStoreName), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, storekit.ImageStoreName, "blobs"), []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	seamQuadlets(t, t.TempDir())
	seamEngine(t, nil, nil, nil)

	_, err := runStoreSweep("podman", false, 1<<40)
	if err == nil {
		t.Fatal("the sweep measured nothing and proceeded; it must refuse")
	}
	if !strings.Contains(err.Error(), "cannot be measured") {
		t.Fatalf("refusal = %q, want it to name the measurement", err)
	}
}

// TestStoreSweepDryRunMakesTheSameDecisionAndTouchesNothing: the dry run's whole value is that its
// plan IS the real plan, and its only licence to be called a dry run is that no byte moved.
func TestStoreSweepDryRunMakesTheSameDecisionAndTouchesNothing(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CHARLY_CACHE_DIR", root)
	seamNoLiveBuild(t)

	putImageEntry(t, root, "ghcr.io/opencharly/live:2026.322.0001", "2026-11-18T10:00:00Z", "keep me")
	deadKey, _ := putImageEntry(t, root, "ghcr.io/opencharly/dead:2026.322.0002", "2026-11-18T11:00:00Z", "evict me")

	units := t.TempDir()
	quadletUnit(t, units, "live", "ghcr.io/opencharly/live:2026.322.0001")
	seamQuadlets(t, units)
	seamEngine(t, nil, nil, nil)

	dry, err := runStoreSweep("podman", true, 1<<40)
	if err != nil {
		t.Fatalf("dry runStoreSweep: %v", err)
	}
	if !dry.DryRun {
		t.Fatal("the outcome does not record that it was a dry run")
	}
	if dry.Retained != 0 {
		t.Fatalf("Retained = %d under --dry-run: the post-sweep size is not measurable without sweeping, so it must stay unset", dry.Retained)
	}
	if !blobPresent(t, root, deadKey) {
		t.Fatal("the dry run evicted an entry")
	}
	real, err := runStoreSweep("podman", false, 1<<40)
	if err != nil {
		t.Fatalf("real runStoreSweep: %v", err)
	}
	if dry.Evicted != real.Evicted {
		t.Fatalf("dry run planned %d eviction(s), the real run made %d", dry.Evicted, real.Evicted)
	}
	if len(dry.Decisions) != len(real.Decisions) {
		t.Fatalf("dry run decided %d entries, the real run %d", len(dry.Decisions), len(real.Decisions))
	}
	for i := range dry.Decisions {
		if dry.Decisions[i].Key != real.Decisions[i].Key || dry.Decisions[i].Keep != real.Decisions[i].Keep {
			t.Fatalf("decision %d differs: dry=%+v real=%+v", i, dry.Decisions[i], real.Decisions[i])
		}
	}
}

// --- D-STORE-7: the refusals -----------------------------------------------------------------

// TestStoreSweepRefusesWhenTheEngineCannotBeListed is the refusal the contract names: an
// unreadable state surface is NOT an empty one, and the sweep must leave every entry alone.
func TestStoreSweepRefusesWhenTheEngineCannotBeListed(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CHARLY_CACHE_DIR", root)
	seamNoLiveBuild(t)

	key, _ := putImageEntry(t, root, "ghcr.io/opencharly/live:2026.322.0001", "2026-11-18T10:00:00Z", "layer")
	seamQuadlets(t, t.TempDir())
	seamEngine(t, nil, nil, errors.New("cannot connect to Podman socket"))

	_, err := runStoreSweep("podman", false, 1<<40)
	if err == nil {
		t.Fatal("the sweep ran against an unreadable engine — it must refuse")
	}
	if !strings.Contains(err.Error(), "refusing to evict") || !strings.Contains(err.Error(), "container engine") {
		t.Fatalf("refusal = %q, want it to say it is refusing and name the surface", err)
	}
	if !blobPresent(t, root, key) {
		t.Fatal("an entry was evicted while the state surface was unreadable")
	}
}

// TestStoreSweepRefusesAQuadletItCannotInterpret: charly always writes Image= into a quadlet it
// creates, so a charly-named unit without one is a deployment record this sweep does not
// understand — and a deployment it cannot name is exactly the one it must not evict.
func TestStoreSweepRefusesAQuadletItCannotInterpret(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CHARLY_CACHE_DIR", root)
	seamNoLiveBuild(t)

	key, _ := putImageEntry(t, root, "ghcr.io/opencharly/live:2026.322.0001", "2026-11-18T10:00:00Z", "layer")
	units := t.TempDir()
	if err := os.WriteFile(filepath.Join(units, "charly-broken.container"), []byte("[Container]\nContainerName=broken\n"), 0o644); err != nil {
		t.Fatalf("write quadlet: %v", err)
	}
	seamQuadlets(t, units)
	seamEngine(t, nil, nil, nil)

	_, err := runStoreSweep("podman", false, 1<<40)
	if err == nil {
		t.Fatal("the sweep ran past a quadlet it could not interpret")
	}
	if !strings.Contains(err.Error(), "charly-broken.container") {
		t.Fatalf("refusal = %q, want it to name the unit", err)
	}
	if !blobPresent(t, root, key) {
		t.Fatal("an entry was evicted while a state record was unreadable")
	}
}

// TestStoreSweepDeclinesUnderALiveBuild: an artifact's blobs are published BEFORE its entry, so a
// sweep between the two would free bytes an in-flight entry has not indexed yet. The guard must
// DECLINE (a skip line, no eviction) rather than run.
func TestStoreSweepDeclinesUnderALiveBuild(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CHARLY_CACHE_DIR", root)

	key, _ := putImageEntry(t, root, "ghcr.io/opencharly/dead:2026.322.0002", "2026-11-18T11:00:00Z", "layer")
	orig := liveBuildFloor
	t.Cleanup(func() { liveBuildFloor = orig })
	liveBuildFloor = func() (kit.CalVer, bool, int) { return kit.CalVer{}, false, 2 }

	out, err := runStoreSweep("podman", false, 1<<40)
	if err != nil {
		t.Fatalf("runStoreSweep: %v", err)
	}
	if out.Skip == nil || out.Skip.live != 2 {
		t.Fatalf("Skip = %+v, want a skip naming the 2 builds in flight", out.Skip)
	}
	if !blobPresent(t, root, key) {
		t.Fatal("a sweep under a live build evicted an entry")
	}
	if lines := strings.Join(storeLines(out), ""); !strings.Contains(lines, "SKIPPED") {
		t.Fatalf("report does not carry the skip:\n%s", lines)
	}
}

// TestStoreCategoryIsUnreachableFromTheWireRequest: `charly clean`'s categories are not all
// wire-reachable. A peer's post-build or post-run prune must NEVER evict a store entry — it has no
// budget, no state inventory and no business deleting artifacts — so the `store` category travels
// on the resolved PLAN only, and a wire request can never select it.
func TestStoreCategoryIsUnreachableFromTheWireRequest(t *testing.T) {
	// An empty request is the shape every peer passes over the wire (plugin-box's post-build
	// prune, plugin-check's post-run prune): the categories travel as booleans on the SHARED
	// #RetentionRequest, and `store` has no field there — it cannot be asked for.
	out := runRetentionOutcome(spec.RetentionRequest{}, cleanPlan{})
	if out.Store != nil {
		t.Fatalf("a wire request reached the store category: %+v", out.Store)
	}
	if (cleanPlan{}).store {
		t.Fatal("the zero plan runs the store category")
	}
}

// --- the authored budget ---------------------------------------------------------------------

// TestStoreBudgetIsAuthoredInTheSchema: D-STORE-7's budget is an AUTHORED value, and this pins
// that the Go reads it rather than carrying a constant. Mutating schema/clean.cue's
// `store_budget.bytes` moves this value; there is no second place it could come from.
func TestStoreBudgetIsAuthoredInTheSchema(t *testing.T) {
	got, err := declaredStoreBudget()
	if err != nil {
		t.Fatalf("declaredStoreBudget: %v", err)
	}
	if got <= 0 {
		t.Fatalf("budget = %d, want a positive byte count", got)
	}
	// The declared value, read the same way the decoder reads it, must be what the decoder
	// returned — a decoder that fell back to a constant would disagree with the schema here.
	v, err := pluginSchema()
	if err != nil {
		t.Fatalf("pluginSchema: %v", err)
	}
	want, cerr := v.LookupPath(cue.ParsePath("#CleanPlugin.store_budget.bytes")).Int64()
	if cerr != nil {
		t.Fatalf("the schema's store_budget.bytes is not an int: %v", cerr)
	}
	if got != want {
		t.Fatalf("declaredStoreBudget() = %d, the schema declares %d: the Go is not derived from the schema", got, want)
	}
}

// --- #27: routing by store identity ------------------------------------------------------------

// TestClassifierRoutesEachStoreByItsOwnIndex: the reader is chosen from what the store SAYS it
// holds, never from the directory it sits under.
func TestClassifierRoutesEachStoreByItsOwnIndex(t *testing.T) {
	t.Run("storekit store", func(t *testing.T) {
		dir := t.TempDir()
		s := storekit.OpenStoreDir(dir)
		dgst, err := s.PutBlob([]byte("payload"))
		if err != nil {
			t.Fatalf("PutBlob: %v", err)
		}
		if _, err := s.PutEntry(storekit.Entry{Key: "k", MediaType: "application/vnd.test+json", Payload: dgst}); err != nil {
			t.Fatalf("PutEntry: %v", err)
		}
		if reader, reason := classifyStore(dir); reader != storeReaderStorekit {
			t.Fatalf("classifyStore = %v (%q); want storekit", reader, reason)
		}
	})

	t.Run("spec/cache layout", func(t *testing.T) {
		dir := t.TempDir()
		l := cache.OpenLayout(dir)
		if err := l.Put("k", cache.Entry{Payload: []byte("payload")}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if reader, reason := classifyStore(dir); reader != storeReaderSpecCache {
			t.Fatalf("classifyStore = %v (%q); want spec/cache", reader, reason)
		}
	})

	t.Run("empty store", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, storekit.LayoutFile), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
			t.Fatalf("marker: %v", err)
		}
		if reader, _ := classifyStore(dir); reader != storeReaderStorekit {
			t.Fatalf("classifyStore(empty) = %v; want storekit (a no-op either way, and the stricter walk)", reader)
		}
	})

	t.Run("unreadable index is refused, never guessed", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte("{not json"), 0o644); err != nil {
			t.Fatalf("index: %v", err)
		}
		reader, reason := classifyStore(dir)
		if reader != storeReaderUnrouted || reason == "" {
			t.Fatalf("classifyStore = %v (%q); want unrouted with a reason", reader, reason)
		}
	})
}

// coincidentRootImageStore is the fixture opencharly/plugin-clean#27's tests share: a cache root
// where the two store roots COINCIDE (CHARLY_CACHE_DIR is D-STORE-1's one override, and under the
// one resolver that is every host), holding one storekit image entry with the blobs a build
// produces and the reference it was built as.
func coincidentRootImageStore(t *testing.T) (root, ref, key string, blobs []string) {
	t.Helper()
	root = t.TempDir()
	t.Setenv("CHARLY_CACHE_DIR", root)
	ref = "localhost/w36-named-live:2026.296.1200"
	key, blobs = putImageEntry(t, root, ref, "2026-10-23T12:00:00Z", "the layer of a live image")
	store := storekit.OpenStoreDir(filepath.Join(root, storekit.ImageStoreName))
	if unreferenced, uerr := store.UnreferencedBlobs(); uerr != nil || len(unreferenced) != 0 {
		t.Fatalf("fixture is wrong: the store reports %d unreferenced blob(s) (%v) before any sweep", len(unreferenced), uerr)
	}
	return root, ref, key, blobs
}

// imageStoreTree lists a store directory's IMMEDIATE entries, sorted — the set D-STORE-3
// (DESIGN.md:2484) constrains: "a store tree is that layout and nothing else — no `.lock`, no
// `locks/`, and no sidecar file of any kind".
func imageStoreTree(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the store tree %s: %v", dir, err)
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names
}

// TestCleanCacheSweepsTheImageStoreWithItsOwnReader is opencharly/plugin-clean#27's FIRST effect:
// with CHARLY_CACHE_DIR set (the two roots coincide), a real `charly clean --cache` must leave
// every blob the surviving entry reaches still present — and leave a store tree that is the layout
// and NOTHING else, because the reader that swept it was storekit, which keeps its index lock in
// the runtime root, rather than `spec/cache`, whose `GCStats` writes `.lock` INSIDE the tree.
//
// The reader is asserted on the ROUTER (`discoverStores`), not inferred from the outcome: an
// assertion that only counted blobs would go green if a future refactor routed the store to a
// third, harmless reader, and an assertion that only the report was right would go green while
// the wrong reader deleted bytes under it.
//
// FAILS WITHOUT THE FIX (mutation: route every store to the image-blind reader): the store loses 4
// of its 5 blobs and the tree gains a `.lock`, which is the measurement in the issue.
func TestCleanCacheSweepsTheImageStoreWithItsOwnReader(t *testing.T) {
	root, _, _, blobs := coincidentRootImageStore(t)
	dir := filepath.Join(root, storekit.ImageStoreName)

	stores, err := discoverStores()
	if err != nil {
		t.Fatalf("discoverStores: %v", err)
	}
	var found *discoveredStore
	for i := range stores {
		if stores[i].Name == storekit.ImageStoreName {
			found = &stores[i]
		}
	}
	if found == nil {
		t.Fatalf("the image store is not in the inventory: %+v", stores)
	}
	if found.Reader != storeReaderStorekit {
		t.Fatalf("the image store at %s is routed to %s — only storekit can read an entry manifest", found.Dir, found.Reader)
	}

	// A REAL run — not a dry run, which the issue measures as predicting the same destruction.
	out, refused, err := gcCacheStores(false)
	if err != nil {
		t.Fatalf("gcCacheStores: %v", err)
	}
	if len(refused) != 0 {
		t.Fatalf("refused = %+v, want none", refused)
	}
	if len(out) != 1 || out[0].Info.Name != storekit.ImageStoreName {
		t.Fatalf("stores = %+v, want the one image store", out)
	}
	if out[0].Reader != storeReaderStorekit {
		// The tree is in the message because the WRONG reader's second effect is the D-STORE-3 one:
		// `spec/cache` writes `.lock` inside the store it sweeps.
		t.Fatalf("the report says %s swept the image store, want storekit; its tree is now %v", out[0].Reader, imageStoreTree(t, dir))
	}
	if out[0].Info.RemovedBlobs != 0 || out[0].Info.RemovedBytes != 0 {
		// The tree is named in the SAME message because #27 recorded two effects of one cause, and a
		// failure that only counted blobs would leave the D-STORE-3 half unproven.
		t.Fatalf("the image store lost %d blob(s) (%d B) and its tree is now %v: nothing it holds was unreferenced, and a store tree carries no lock and no sidecar (D-STORE-3)",
			out[0].Info.RemovedBlobs, out[0].Info.RemovedBytes, imageStoreTree(t, dir))
	}
	for _, d := range blobs {
		if !blobPresent(t, root, d) {
			t.Fatalf("blob %s was destroyed by the cache sweep", d)
		}
	}
	if got, want := imageStoreTree(t, dir), []string{"blobs", "index.json", "oci-layout"}; !slices.Equal(got, want) {
		t.Fatalf("the store tree is %v, want exactly %v — a store tree carries no lock and no sidecar (D-STORE-3)", got, want)
	}
}

// TestCleanCacheLookupResolvesToExistingContent is opencharly/plugin-clean#27's SECOND, sharper
// tell: after the sweep the store must not still ANSWER for bytes that are gone. The defect left
// `Lookup(ref) = (digest, true)` over a digest whose manifest, config and layers had been deleted,
// so every later consumer resolved through the entry to nothing. This asserts the positive the
// defect cannot satisfy — the resolved digest's own payload still reads back — which is why the
// two tests are a PAIR and not one: the blob count can be right while the reference is dead, and
// the reference can be live while bytes are missing from the graph.
//
// FAILS WITHOUT THE FIX (same mutation): `Lookup` still returns `true`, and this test fails on the
// READ, naming the digest that no longer exists.
func TestCleanCacheLookupResolvesToExistingContent(t *testing.T) {
	root, ref, key, _ := coincidentRootImageStore(t)

	if _, _, err := gcCacheStores(false); err != nil {
		t.Fatalf("gcCacheStores: %v", err)
	}
	store := storekit.OpenStoreDir(filepath.Join(root, storekit.ImageStoreName))
	img, err := storekit.OpenImageStore()
	if err != nil {
		t.Fatalf("OpenImageStore: %v", err)
	}
	dgst, ok, lerr := img.Lookup(ref)
	if lerr != nil || !ok {
		t.Fatalf("Lookup(%q) = %q, %v, %v; want the entry to resolve", ref, dgst, ok, lerr)
	}
	if dgst != key {
		t.Fatalf("Lookup(%q) = %s, want the entry's key %s", ref, shortDigest(dgst), shortDigest(key))
	}
	if _, rerr := store.ReadBlob(dgst); rerr != nil {
		t.Fatalf("the entry answers Lookup(%q) = %s but that payload no longer exists: %v — the tell of opencharly/plugin-clean#27", ref, shortDigest(dgst), rerr)
	}
	entry, rerr := store.ReadEntry(key)
	if rerr != nil {
		t.Fatalf("ReadEntry(%s): %v", shortDigest(key), rerr)
	}
	if entry.Payload == "" {
		t.Fatal("the surviving entry has no payload")
	}
	if _, perr := store.ReadBlob(entry.Payload); perr != nil {
		t.Fatalf("the surviving entry's payload does not read back: %v", perr)
	}
}

// TestCleanCacheRefusesAStoreItCannotClassify: a store whose index mixes descriptor media types is
// reported and left untouched. Sweeping it with either reader is how #27 destroyed an image, one
// shape over.
func TestCleanCacheRefusesAStoreItCannotClassify(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CHARLY_CACHE_DIR", root)

	dir := filepath.Join(root, "mixed")
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, storekit.LayoutFile), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
		t.Fatalf("marker: %v", err)
	}
	idx := `{"schemaVersion":2,"manifests":[{"mediaType":"application/vnd.opencharly.store.entry.v1+json","digest":"sha256:aa"},{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:bb"}]}`
	if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte(idx), 0o644); err != nil {
		t.Fatalf("index: %v", err)
	}
	// REAL bytes in the refused store: "nothing was swept" has to be observable, or the refusal is
	// a line the sweep prints while deleting anyway.
	refusedBlob := filepath.Join(dir, "blobs", "sha256", "aa")
	if err := os.WriteFile(refusedBlob, []byte("the refused store's bytes"), 0o644); err != nil {
		t.Fatalf("refused blob: %v", err)
	}

	infos, refused, err := gcCacheStores(false)
	if err != nil {
		t.Fatalf("gcCacheStores: %v", err)
	}
	if len(infos) != 0 {
		t.Fatalf("stores swept = %+v, want none: the mixed store must be refused", infos)
	}
	if len(refused) != 1 || refused[0].Name != "mixed" {
		t.Fatalf("refused = %+v, want [mixed]", refused)
	}
	if !strings.Contains(refused[0].Reason, "mixes") {
		t.Fatalf("reason = %q, want it to say the index mixes media types", refused[0].Reason)
	}
	// Not a byte removed from it.
	if _, serr := os.Stat(refusedBlob); serr != nil {
		t.Fatalf("a REFUSED store lost bytes: %v", serr)
	}
	// And the REPORT carries the refusal by name, not only the Go value: an operator reading
	// `charly clean --cache` must see which store was left alone and why.
	var buf bytes.Buffer
	if perr := printRetentionResult(&buf, "removed", cleanPlan{cache: true}, retentionOutcome{CacheSweep: infos, RefusedStores: refused}); perr != nil {
		t.Fatalf("printRetentionResult: %v", perr)
	}
	line := buf.String()
	if strings.Contains(line, "no named cache stores found") {
		t.Fatalf("the report says no named stores were found while refusing one:\n%s", line)
	}
	if !strings.Contains(line, "cache mixed: REFUSED at "+dir) {
		t.Fatalf("the report does not name the refused store:\n%s", line)
	}
	if !strings.Contains(line, "mixes") {
		t.Fatalf("the report does not carry the refusal reason:\n%s", line)
	}
}
