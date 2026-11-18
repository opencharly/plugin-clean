package clean

// store_eviction.go — D-STORE-7's ENTRY policy for the charly image store.
//
// The contract row (`DESIGN.md:2488`):
//
//	D-STORE-7  `charly clean` keeps what state references and evicts the rest within a size
//	           budget; locks die with their holder.
//
// # Why this is not `keep_images`
//
// `keep_images` counts TAG ROWS in the ENGINE's storage: it groups engine images by their
// `ai.opencharly.box` label and keeps the newest N. The charly image store (the `registry`
// named store, opencharly/sdk#556) is a different surface with a different shape: one ENTRY per
// build, keyed by the artifact's digest, never replaced by a rebuild. Nothing counted those
// entries, so a store measured at 283 GiB / 172 entries on the host this was written on grew by
// one artifact per rebuild forever. D-STORE-7 is not a recency budget — it is a REFERENCE-driven
// keep set with a ceiling, and the two rules are different in a way that matters: "the newest 3"
// would delete a running deployment's image if it happened to be the fourth.
//
// # What "state references" means, and why the answer must be able to say "I do not know"
//
// A store entry is an ARTIFACT, not a deployment. What makes an artifact live is that some
// record charly itself wrote still names it, and this sweep consults the HOST's charly state,
// by name and by resolver:
//
//	1. THE CONTAINER ENGINE (`podman ps -a` / the resolved RUN engine). Every container, running
//	   or stopped, reports the image ref it was created from and that image's ID — the runtime
//	   truth a deploy actually runs, and the surface `charly status --json` publishes as
//	   `image_ref`. Read through `listContainerImageRefs`, the SAME collector the dangling-image
//	   sweep has always used (R2: one reader).
//	2. THE QUADLET UNITS (`deploykit.QuadletDir()`, `charly-*.container`, `Image=`). A deploy that
//	   is ENABLED but whose container does not (yet) exist has no engine row at all, so the unit
//	   file charly wrote is the only record of the image it will start. This is the surface
//	   `charly status --all` synthesises its `enabled` rows from, and it is read here directly
//	   because `status --json` carries no `image_ref` for such a row.
//
// THE REFUSAL IS PART OF THE CONTRACT, not a fallback. An unreadable surface is not an empty
// one: "assume nothing is referenced" would delete the image a running deployment was built
// from, which is the worst failure this command can have. So every read failure — the engine
// cannot be listed, a quadlet exists but cannot be parsed, the store's own index and entries
// disagree — aborts the WHOLE sweep with an error naming the surface and the path, and nothing is
// evicted. A `--dry-run` refuses on exactly the same inputs, because a dry run that predicted an
// eviction the real run then refused would be a lie about the plan.
//
// THE RESOLUTION TAG → DIGEST is the STORE's own (`ImageStore.Lookup`), never this file's: the
// state names a REFERENCE (a full CalVer tag, a rolling tag, a bare repository), while the store
// keys on the artifact's digest, and `Lookup` is already "the newest entry whose annotation names
// it" — the one answer an index can give. It is also what makes a REBUILD of a referenced ref
// reclaimable: when the engine still names `vision-gate` and the store holds five entries under
// that annotation, the store resolves the name to the newest and the four superseded ones are
// evicted.
//
// # What is deliberately NOT in the inventory
//
// DECLARED configuration (a project `charly.yml`/`vm.yml`) is not state: it says what the operator
// WANTS, and it names a box, not the artifact a build produced. A `kind: vm` whose disk was built
// from a container image is the one place where a store artifact can be needed without any host
// record naming it — the VM's disk is the product and its source ref lives only in the authored
// entity — so a bootc-sourced VM is outside this inventory's reach; opencharly/plugin-vm#98
// carries that gap. The sweep's scope is stated rather than guessed, because a guessed surface is
// how a keep set becomes a delete set.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/opencharly/sdk/deploykit"
	"github.com/opencharly/sdk/storekit"
)

// quadletDir resolves the user-level quadlet directory. A package-level var for the same reason
// `resolveEngine` / `listContainerImageRefs` are: a test proves the refusal path and the keep set
// against an injected tree, and nothing outside its temporary directory (D-STORE-8).
var quadletDir = deploykit.QuadletDir

// storeRefResolver is the store's own reference → digest resolution (`ImageStore.Lookup`).
type storeRefResolver interface {
	Lookup(ref string) (digest string, ok bool, err error)
}

// storeStateSurface is ONE named, charly-written record the sweep read, with what it named.
type storeStateSurface struct {
	// Name is the surface's user-facing name ("container engine", "quadlet units").
	Name string
	// Where is the exact place it was read from — the engine binary's verb, or the directory.
	Where string
	// Refs and Digests are how many references it contributed (deduplicated across surfaces).
	Refs    int
	Digests int
	// Absent records a surface that legitimately has nothing to say on this host (a host with no
	// quadlet units), so the report can tell it from a surface that was not looked at.
	Absent bool
}

// storeStateInventory is every image reference the host's charly state still names.
type storeStateInventory struct {
	Surfaces []storeStateSurface
	// refs maps a NORMALISED reference (see normImageRef) to the surface that named it.
	refs map[string]string
	// repos maps a normalised reference that carries NO tag to its surface: a state record that
	// names a bare repository references whatever that repository means to the store, so every
	// tag of it is kept (the conservative direction — a false keep costs disk, a false evict
	// costs a deployment).
	repos map[string]string
	// digests maps an image ID / content digest (`sha256:<hex>`, or a short engine ID) to the
	// surface that named it.
	digests map[string]string
}

// newStoreStateInventory returns an inventory whose maps are ready to fill.
func newStoreStateInventory() storeStateInventory {
	return storeStateInventory{
		refs:    map[string]string{},
		repos:   map[string]string{},
		digests: map[string]string{},
	}
}

// add records one surface's references. Duplicates across surfaces keep the FIRST surface's name,
// so a reference named by both the engine and a quadlet is reported against the engine (the one a
// reader would check first) rather than flapping between runs. Each surface's own counts are what
// THAT surface added, not the inventory total restated under every surface — a reader checking
// "did the quadlets contribute anything" needs the second, not the first.
func (in *storeStateInventory) add(surface storeStateSurface, refs []string, digests []string) {
	addedRefs := 0
	for _, ref := range refs {
		norm, hasTag := normImageRef(ref)
		if norm == "" {
			continue
		}
		if hasTag {
			if _, seen := in.refs[norm]; !seen {
				if _, seenRepo := in.repos[norm]; !seenRepo {
					in.refs[norm] = surface.Name
					addedRefs++
				}
			}
			continue
		}
		if _, seenRepo := in.repos[norm]; !seenRepo {
			if _, seen := in.refs[norm]; !seen {
				in.repos[norm] = surface.Name
				addedRefs++
			}
		}
	}
	addedDigests := 0
	for _, d := range digests {
		hex := normImageID(d)
		if hex == "" {
			continue
		}
		if _, seen := in.digests[hex]; !seen {
			in.digests[hex] = surface.Name
			addedDigests++
		}
	}
	surface.Refs = addedRefs
	surface.Digests = addedDigests
	in.Surfaces = append(in.Surfaces, surface)
}

// readStoreStateInventory reads every surface the sweep trusts, and REFUSES rather than returning
// a partial inventory: a partial set is indistinguishable from "nothing references this", which
// is the one answer this command may never produce by accident.
func readStoreStateInventory(engineBin string) (storeStateInventory, error) {
	in := newStoreStateInventory()

	ids, refs, err := listContainerImageRefs(engineBin)
	if err != nil {
		return in, fmt.Errorf("the container engine cannot be listed (%v) — an unreadable state surface is not an empty one", err)
	}
	in.add(storeStateSurface{Name: "container engine", Where: engineBin + " ps -a"},
		sortedKeys(refs), sortedKeys(ids))

	dir, err := quadletDir()
	if err != nil {
		return in, fmt.Errorf("the quadlet directory cannot be resolved: %w", err)
	}
	unitRefs, present, err := quadletStateRefs(dir)
	if err != nil {
		return in, err
	}
	in.add(storeStateSurface{
		Name:   "quadlet units",
		Where:  dir,
		Absent: !present,
	}, unitRefs, nil)

	return in, nil
}

// quadletStateRefs reads the `Image=` line of every charly quadlet unit in dir, and reports
// whether the directory exists at all.
//
// A unit that charly wrote but this sweep cannot read is a REFUSAL, not a skip: the file exists,
// so something is deployed through it, and a unit whose image cannot be named is exactly the case
// where assuming nothing is referenced deletes a live image. A unit with no `Image=` line is
// refused on the same grounds — charly always writes one, so its absence is a state record this
// sweep does not understand.
func quadletStateRefs(dir string) (refs []string, present bool, err error) {
	ents, rerr := os.ReadDir(dir)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("the quadlet directory %s cannot be read: %w", dir, rerr)
	}
	present = true
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "charly-") || !strings.HasSuffix(name, ".container") {
			continue
		}
		path := filepath.Join(dir, name)
		raw, ferr := os.ReadFile(path)
		if ferr != nil {
			return nil, true, fmt.Errorf("the quadlet unit %s cannot be read (%v) — refusing to treat an unreadable deployment record as an empty one", path, ferr)
		}
		image := quadletImageLine(string(raw))
		if image == "" {
			return nil, true, fmt.Errorf("the quadlet unit %s carries no Image= line — refusing to treat a deployment record this sweep cannot interpret as referencing nothing", path)
		}
		refs = append(refs, image)
	}
	sort.Strings(refs)
	return refs, present, nil
}

// quadletImageLine extracts a systemd unit's `Image=` value: the LAST assignment wins, matching
// systemd's own rule, and a commented or `Image=`-prefixed line that is not an assignment is
// ignored.
func quadletImageLine(unit string) string {
	var got string
	for _, line := range strings.Split(unit, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		value, ok := strings.CutPrefix(line, "Image=")
		if !ok {
			continue
		}
		if v := strings.TrimSpace(value); v != "" {
			got = v
		}
	}
	return got
}

// normImageRef folds a reference into the ONE form the state surfaces and the store agree on: the
// REPOSITORY TAIL plus the tag, with the registry host and the namespace path dropped. It reports
// whether a tag was present.
//
//	ghcr.io/opencharly/comfyui:2026.321.2347  ->  ("comfyui:2026.321.2347", true)
//	comfyui:2026.321.2347                     ->  ("comfyui:2026.321.2347", true)
//	localhost/openwebui-tts:2026.318.2016     ->  ("openwebui-tts:2026.318.2016", true)
//	oci-archive://ghcr.io/x/y:2026.1.1        ->  ("y:2026.1.1", true)
//	docker.io/library/busybox                 ->  ("busybox", false)
//
// The fold deliberately OVER-matches: two registries' `comfyui:2026.1.1` are one name here, so a
// state reference keeps a store entry that a stricter comparison would have evicted. That is the
// safe direction, and the surfaces this sweep reads are charly's own — they do not name two
// different registries' images under one tail.
func normImageRef(ref string) (norm string, hasTag bool) {
	s := strings.TrimSpace(ref)
	if s == "" {
		return "", false
	}
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	// A digest pin (`repo:tag@sha256:…`) keeps its tag half: the tag is what state and the store's
	// annotation share, and the digest is matched separately.
	if i := strings.Index(s, "@"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimPrefix(s, "library/")
	if s == "" {
		return "", false
	}
	if i := strings.LastIndex(s, ":"); i >= 0 && i < len(s)-1 {
		return s, true
	}
	return s, false
}

// repoOf is the REPOSITORY half of a normalised reference: `comfyui:2026.1.1` -> `comfyui`. A
// normalised reference carries no registry host and no digest pin, so the only separator left is the
// tag's, and a reference that had none normalises to its repository already.
func repoOf(norm string) string {
	repo, _, _ := strings.Cut(norm, ":")
	return repo
}

// storeEntryDecision is ONE entry's verdict, with the reason a reader can check.
type storeEntryDecision struct {
	// Key is the entry's key — for an image entry, the artifact's content address.
	Key string
	// Ref is the annotated image reference (`io.containerd.image.name`), "" when absent.
	Ref string
	// Created is the entry's recorded build time.
	Created string
	// Keep is the verdict.
	Keep bool
	// Reason is WHY, in the operator's terms.
	Reason string
}

// storeSweepOutcome is one `--store` run's decision and its measured result.
type storeSweepOutcome struct {
	Dir    string
	Budget int64
	// BudgetSource is the surface that authored the ceiling the sweep measured against —
	// `user config` or `plugin default` — so the report can never present one as the other.
	BudgetSource string
	Surfaces     []storeStateSurface
	// Decisions is every enumerated entry, kept and evicted, in key order.
	Decisions []storeEntryDecision
	// Evicted is how many entries the sweep removed (or would remove).
	Evicted int
	// Held is the store's measured size BEFORE the sweep.
	Held int64
	// Retained is the store's measured size AFTER the sweep. It is zero under --dry-run, where
	// nothing was evicted: the store's public surface cannot size a hypothetical entry set
	// without deleting it, and a number that LOOKS like the post-sweep size while being the
	// pre-sweep one is the kind of figure this report exists to avoid.
	Retained int64
	// AlreadyGone counts planned evictions whose entry a CONCURRENT writer had already removed
	// between the plan and the removal. It is not an error (the outcome on disk is the one the
	// sweep wanted) and it is not a removal either, so it is reported instead of being folded into
	// the count.
	AlreadyGone int
	// Reclaim is the blob bytes the sweep freed (a real run) or would free from the store's
	// already-unreferenced blobs (a dry run — the evicted entries' own layers are freed with
	// them, and the real run is what measures those).
	Reclaim int64
	// DryRun records whether this outcome describes a dry run, so the report can choose its verbs
	// from the OUTCOME rather than inferring them from a count.
	DryRun bool
	// Skip is set when the live-build guard declined, never when there was nothing to do.
	Skip *retentionSkip
}

// kept returns the decisions that survived.
func (o *storeSweepOutcome) kept() []storeEntryDecision {
	var out []storeEntryDecision
	for _, d := range o.Decisions {
		if d.Keep {
			out = append(out, d)
		}
	}
	return out
}

// evicted returns the decisions the sweep removed (or would remove).
func (o *storeSweepOutcome) evicted() []storeEntryDecision {
	var out []storeEntryDecision
	for _, d := range o.Decisions {
		if !d.Keep {
			out = append(out, d)
		}
	}
	return out
}

// planStoreEviction is the PURE decision: which entries the state references, which it does not,
// and why. It touches nothing — the caller removes what this returned, so `--dry-run` and the
// real run make the same decision by construction.
func planStoreEviction(entries []storekit.Entry, res storeRefResolver, inv storeStateInventory) ([]storeEntryDecision, error) {
	// The keep set is resolved through the STORE, one Lookup per DISTINCT store reference the state
	// names, so the number of index scans is bounded by the state (a handful), not by the store's
	// entry count.
	keepByRef := map[string]string{} // normalised ref -> digest the store resolves it to
	for _, e := range entries {
		ref := e.Annotations[storekit.AnnotationImageName]
		if ref == "" {
			continue
		}
		norm, _ := normImageRef(ref)
		// Only an EXACT (tagged) match is resolved through the store, because only a name WITH a
		// tag denotes one artifact. A bare-repository reference is decided in the classification
		// loop below, where it keeps EVERY tag of the repository — resolving it here would ask the
		// store "which build does `vision-gate` mean", and the store would answer for one entry
		// while the state meant the repository.
		source, named := inv.refs[norm]
		if !named {
			continue
		}
		if _, done := keepByRef[norm]; done {
			continue
		}
		dgst, ok, lerr := res.Lookup(ref)
		if lerr != nil {
			return nil, fmt.Errorf("the state names %s (from %s) and the store cannot resolve it: %w", ref, source, lerr)
		}
		if !ok {
			return nil, fmt.Errorf("the state names %s (from %s) but the store's index cannot resolve it — the index and the entries disagree, so no entry may be evicted", ref, source)
		}
		keepByRef[norm] = dgst
	}

	out := make([]storeEntryDecision, 0, len(entries))
	for _, e := range entries {
		d := storeEntryDecision{
			Key:     e.Key,
			Ref:     e.Annotations[storekit.AnnotationImageName],
			Created: e.Annotations[storekit.AnnotationCreated],
		}
		source, byDigest := inv.digests[normImageID(e.Key)]
		norm, _ := normImageRef(d.Ref)
		resolved, byRef := keepByRef[norm]
		switch {
		case byDigest:
			// The state names this artifact's CONTENT ADDRESS — a container created from it, or a
			// project pin. Nothing about the reference matters: the bytes are what is live.
			d.Keep = true
			d.Reason = fmt.Sprintf("referenced by %s (its own content address %s)", source, shortDigest(e.Key))
		case d.Ref == "":
			// An entry with no reference annotation cannot be matched to state at all. Keeping it is
			// the only verdict that cannot be wrong.
			d.Keep = true
			d.Reason = fmt.Sprintf("no %s annotation: unclassifiable, so kept", storekit.AnnotationImageName)
		case inv.repos[repoOf(norm)] != "":
			// THE BARE-REPOSITORY CASE, and it is checked BEFORE the exact one on purpose: a state
			// record naming `vision-gate` (a quadlet, a digest-only reference folded to a bare
			// repository) names the REPOSITORY, not one build of it, so every tag under it is kept.
			// Evicting a build because the name that kept its sibling carried no tag is a false
			// evict, and a false evict costs a deployment — the one thing this command must never
			// do. The price is that a bare reference also protects superseded rebuilds; that is the
			// conservative direction and it is the direction the inventory's own `repos` doc
			// promises.
			d.Keep = true
			d.Reason = fmt.Sprintf("referenced by %s: the state names the repository %q, so every tag of it is kept", inv.repos[repoOf(norm)], repoOf(norm))
		case byRef && resolved == e.Key:
			d.Keep = true
			d.Reason = fmt.Sprintf("referenced by %s: %s resolves here", inv.sourceOf(norm), d.Ref)
		case byRef:
			d.Keep = false
			d.Reason = fmt.Sprintf("superseded: %s is referenced by %s and the store resolves it to %s", d.Ref, inv.sourceOf(norm), shortDigest(resolved))
		default:
			d.Keep = false
			d.Reason = fmt.Sprintf("unreferenced: no state surface names %s (or its content address)", refOrDigest(d.Ref, e.Key))
		}
		out = append(out, d)
	}
	return out, nil
}

// sourceOf names the surface that referenced a normalised reference.
func (in storeStateInventory) sourceOf(norm string) string {
	if s, ok := in.refs[norm]; ok {
		return s
	}
	if s, ok := in.repos[norm]; ok {
		return s
	}
	return "state"
}

// refOrDigest renders the identity a decision was made on.
func refOrDigest(ref, key string) string {
	if ref == "" {
		return shortDigest(key)
	}
	return ref
}

// shortDigest abbreviates a content address for a report line (`sha256:ab12cd34…`).
func shortDigest(dgst string) string {
	alg, hex, ok := strings.Cut(dgst, ":")
	if !ok {
		return dgst
	}
	if len(hex) > 12 {
		hex = hex[:12] + "…"
	}
	return alg + ":" + hex
}

// storeBlobBytes measures the store's blobs. AN UNMEASURABLE STORE IS AN ERROR, never a zero: a zero
// would let the budget verdict print "within budget" for a store nobody could size, which is the
// same lie as "nothing is referenced" one step further on. A blob that cannot be stat-ed after the
// listing is skipped (it vanished under us), because the LISTING — the thing that decides whether the
// store can be measured at all — has already succeeded by then.
func storeBlobBytes(s *storekit.Store) (int64, error) {
	blobs, err := s.Blobs()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, dgst := range blobs {
		p, perr := s.BlobPath(dgst)
		if perr != nil {
			continue
		}
		if fi, serr := os.Stat(p); serr == nil && !fi.IsDir() {
			total += fi.Size()
		}
	}
	return total, nil
}

// sortedKeys returns a set's keys in a deterministic order, so a surface's contribution and its
// report do not depend on map iteration order.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// runStoreSweep is the `store` category: read the state, decide, evict, reclaim, measure.
//
// ORDER IS THE CONTRACT. The live-build guard runs FIRST (a build owns the store it writes: an
// artifact's blobs are published before its entry, so a sweep between the two would free blobs an
// in-flight entry had not indexed yet). The state inventory is read SECOND and refuses the whole
// sweep on any doubt. Only then is anything deleted — entries first, then the blobs no surviving
// entry reaches, through the SAME reclaim the `cache` category uses.
func runStoreSweep(engineBin string, dryRun bool, budget storeBudget) (*storeSweepOutcome, error) {
	out := &storeSweepOutcome{Budget: budget.Bytes, BudgetSource: budget.Source, DryRun: dryRun}

	if _, _, live := liveBuildFloor(); live > 0 {
		out.Skip = &retentionSkip{
			live:   live,
			reason: "the image store is written by the builds that own it, and an artifact's blobs are published before its entry, so evicting now could free bytes an in-flight entry has not indexed yet",
		}
		return out, nil
	}

	dir, err := storekit.ImageStoreDir()
	if err != nil {
		return nil, fmt.Errorf("resolving the image store: %w", err)
	}
	out.Dir = dir
	store := storekit.OpenStoreDir(dir)
	resolver, err := storekit.OpenImageStore()
	if err != nil {
		return nil, fmt.Errorf("opening the image store %s: %w", dir, err)
	}

	inv, err := readStoreStateInventory(engineBin)
	if err != nil {
		return nil, fmt.Errorf("refusing to evict from the image store %s: %w", dir, err)
	}
	out.Surfaces = inv.Surfaces

	entries, err := store.Entries()
	if err != nil {
		return nil, fmt.Errorf("refusing to evict: the image store %s cannot be enumerated: %w", dir, err)
	}
	out.Decisions, err = planStoreEviction(entries, resolver, inv)
	if err != nil {
		return nil, fmt.Errorf("refusing to evict from the image store %s: %w", dir, err)
	}
	held, herr := storeBlobBytes(store)
	if herr != nil {
		return nil, fmt.Errorf("refusing to evict: the image store %s cannot be measured (%v), so neither its size nor its budget could be reported", dir, herr)
	}
	out.Held = held
	for _, d := range out.Decisions {
		if !d.Keep {
			out.Evicted++
		}
	}

	if dryRun {
		// The already-unreferenced blobs are the part of the reclaim a dry run CAN measure: the
		// store's own reader will name them on the real run too, whatever the entry policy does.
		// The evicted entries' own layers are freed with them, and only the real run sizes those.
		_, bytes, berr := sweepStoreBlobs(store, true)
		if berr != nil {
			return nil, fmt.Errorf("sizing the image store's unreferenced blobs: %w", berr)
		}
		out.Reclaim = bytes
		return out, nil
	}

	for _, d := range out.evicted() {
		removed, derr := store.DeleteEntry(d.Key)
		if derr != nil {
			return nil, fmt.Errorf("evicting the store entry %s (%s): %w", shortDigest(d.Key), d.Ref, derr)
		}
		if !removed {
			out.AlreadyGone++
		}
	}
	_, bytes, berr := sweepStoreBlobs(store, false)
	if berr != nil {
		return nil, fmt.Errorf("reclaiming the evicted entries' blobs: %w", berr)
	}
	out.Reclaim = bytes
	retained, merr := storeBlobBytes(store)
	if merr != nil {
		return nil, fmt.Errorf("the image store %s was swept but cannot be measured afterwards (%v); refusing to print a retained figure that would be a guess", dir, merr)
	}
	out.Retained = retained
	return out, nil
}
