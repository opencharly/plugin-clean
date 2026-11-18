package clean

// store_sweep_test.go — the coverage for D-STORE-7's entry policy (the `store` category) and for
// the store-IDENTITY routing that keeps an image store out of an image-blind reader
// (opencharly/plugin-clean#27).
//
// Every case injects its roots (CHARLY_CACHE_DIR, the quadlet directory, the engine collector) and
// writes nothing outside its own temporary directory (D-STORE-8).

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
// state names a rolling tag, the store holds two builds under it, and the NEWEST is the one the
// store resolves the name to — so the older build is reclaimable churn, not a live artifact.
func TestStoreSweepEvictsASupersededRebuildOfAReferencedRef(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CHARLY_CACHE_DIR", root)
	seamNoLiveBuild(t)

	oldKey, _ := putImageEntry(t, root, "ghcr.io/opencharly/vision-gate", "2026-11-17T10:00:00Z", "old layer")
	newKey, _ := putImageEntry(t, root, "ghcr.io/opencharly/vision-gate", "2026-11-18T10:00:00Z", "new layer")

	units := t.TempDir()
	quadletUnit(t, units, "gate", "ghcr.io/opencharly/vision-gate")
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
		reader, reason, err := classifyStore(dir)
		if err != nil || reader != storeReaderStorekit {
			t.Fatalf("classifyStore = %v (%q), %v; want storekit", reader, reason, err)
		}
	})

	t.Run("spec/cache layout", func(t *testing.T) {
		dir := t.TempDir()
		l := cache.OpenLayout(dir)
		if err := l.Put("k", cache.Entry{Payload: []byte("payload")}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		reader, reason, err := classifyStore(dir)
		if err != nil || reader != storeReaderSpecCache {
			t.Fatalf("classifyStore = %v (%q), %v; want spec/cache", reader, reason, err)
		}
	})

	t.Run("empty store", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, storekit.LayoutFile), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
			t.Fatalf("marker: %v", err)
		}
		reader, _, err := classifyStore(dir)
		if err != nil || reader != storeReaderStorekit {
			t.Fatalf("classifyStore(empty) = %v, %v; want storekit (a no-op either way, and the stricter walk)", reader, err)
		}
	})

	t.Run("unreadable index is refused, never guessed", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte("{not json"), 0o644); err != nil {
			t.Fatalf("index: %v", err)
		}
		reader, reason, err := classifyStore(dir)
		if err != nil {
			t.Fatalf("classifyStore: %v", err)
		}
		if reader != storeReaderUnrouted || reason == "" {
			t.Fatalf("classifyStore = %v (%q); want unrouted with a reason", reader, reason)
		}
	})
}

// TestCleanCacheImageStoreSurvivesTheOneRootOverride is opencharly/plugin-clean#27: with
// CHARLY_CACHE_DIR set — the one override D-STORE-1 blesses, and how a bed isolates its cache — the
// two store roots coincide, and a real `charly clean --cache` must NOT hand the image store to the
// image-blind `spec/cache` reader. Every blob the entry reaches must survive, and `Lookup` must
// still resolve to content that exists.
//
// FAILS WITHOUT THE FIX (measured by mutating the router back to the two-half form): the sweep
// removes the manifest, the config and the layer, leaves one blob and leaves the entry answering
// `Lookup(ref)` for bytes that are gone.
func TestCleanCacheImageStoreSurvivesTheOneRootOverride(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CHARLY_CACHE_DIR", root)

	key, _ := putImageEntry(t, root, "localhost/w36-named-live:2026.296.1200", "2026-10-23T12:00:00Z", "the layer of a live image")
	store := storekit.OpenStoreDir(filepath.Join(root, storekit.ImageStoreName))
	entry, err := store.ReadEntry(key)
	if err != nil {
		t.Fatalf("ReadEntry: %v", err)
	}
	// What the entry reaches, spelled out from the fixture: layer, config, manifest, index, and the
	// entry manifest itself (whose digest PutEntry returns).
	blobs, err := store.Blobs()
	if err != nil {
		t.Fatalf("Blobs: %v", err)
	}
	if len(blobs) < 5 {
		t.Fatalf("fixture is wrong: %d blob(s), want at least the 5 an image entry reaches", len(blobs))
	}
	if unreferenced, uerr := store.UnreferencedBlobs(); uerr != nil || len(unreferenced) != 0 {
		t.Fatalf("fixture is wrong: the store reports %d unreferenced blob(s) (%v) before any sweep", len(unreferenced), uerr)
	}

	// A REAL run of the cache category — not a dry run, which the issue measures as predicting the
	// same destruction.
	infos, refused, err := gcCacheStores(false)
	if err != nil {
		t.Fatalf("gcCacheStores: %v", err)
	}
	if len(refused) != 0 {
		t.Fatalf("refused = %+v, want none", refused)
	}
	if len(infos) != 1 || infos[0].Name != storekit.ImageStoreName {
		t.Fatalf("stores = %+v, want the one image store", infos)
	}
	if infos[0].RemovedBlobs != 0 || infos[0].RemovedBytes != 0 {
		t.Fatalf("the image store lost %d blob(s) (%d B): nothing it holds was unreferenced",
			infos[0].RemovedBlobs, infos[0].RemovedBytes)
	}
	for _, d := range blobs {
		if !blobPresent(t, root, d) {
			t.Fatalf("blob %s was destroyed by the cache sweep", d)
		}
	}
	// And the reference still answers with content that exists — THE tell of the defect: the store
	// used to keep answering `Lookup(ref) = (digest, true)` for a digest whose bytes were gone.
	img, err := storekit.OpenImageStore()
	if err != nil {
		t.Fatalf("OpenImageStore: %v", err)
	}
	dgst, ok, lerr := img.Lookup("localhost/w36-named-live:2026.296.1200")
	if lerr != nil || !ok {
		t.Fatalf("Lookup = %q, %v, %v; want the entry to resolve", dgst, ok, lerr)
	}
	if _, rerr := store.ReadBlob(dgst); rerr != nil {
		t.Fatalf("the entry resolves to a payload that no longer exists: %v", rerr)
	}
	if entry.Payload == "" {
		t.Fatal("fixture is wrong: the entry has no payload")
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
}
