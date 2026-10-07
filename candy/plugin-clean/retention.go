package clean

// retention.go — the SHARED retention ENGINE (image-tag / build-candy / check-run pruning +
// the --deep store-wide dangling-image purge + the charly-labeled image-tag inventory).
// Relocated from charly/retention.go (K1-alpha core-minimization): this candy is the ONE
// owner now. `charly clean`'s own CLI (command.go) calls runRetention directly — no wire
// hop, same package. The remaining callers — candy/plugin-box's post-build prune
// (pruneAfterBuild) and `box list tags` (listImageTags), and the check harness's post-run
// prune — are all PEER PLUGINS reaching it via verb:retention over InvokeProvider, the same
// peer-dispatch pattern verb:credential/verb:gpu/verb:tunnel use; NO core adapter remains
// (charly/retention_plugin.go, the former core-side caller, is DELETED — #118).
//
// runRetention — the verb:retention entry the peers call — runs every category EXCEPT `scopes`;
// a peer's post-build/post-run prune has no business stopping a host systemd scope. `scopes` is a
// CLI-only category, so it travels as an explicit argument rather than on the shared wire request
// (see retentionOutcome.Scopes).
//
// Retention fallback: when defaults.keep_images / keep_check_runs are absent from config,
// the caller resolves 0 ("disabled") so third-party configs get no surprise pruning. The
// repo's charly.yml opts in (keep_images: 3, keep_check_runs: 3). See /charly-core:clean.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/opencharly/sdk/kit"
	"github.com/opencharly/spec/cache"
	"github.com/opencharly/spec/spec"
)

// runRetention dispatches one verb:retention request to the requested category(ies) and
// returns the reply. Mirrors the former charly/host_build_retention.go dispatch, plus the
// new `list` (charly box list tags) and `build_prune` (the narrow post-build-only scope)
// actions this relocation adds. req.KeepImages/req.KeepCheckRuns arrive PRE-RESOLVED (the
// caller's defaults.keep_images/keep_check_runs, 0 = disabled) — this engine never reads
// charly.yml itself, it only ever runs the podman/filesystem side of retention (R3: config
// resolution stays with whoever can load the project — core call sites resolve in-process,
// the plugin's own CLI + plugin-check's post-run hook + plugin-box's post-build prune resolve
// it PLUGIN-SIDE via the shared sdk/loaderkit.ResolveRetentionDefaultsViaExecutor, K-wave 2
// cone R6, the former "retention-defaults" HostBuild seam DELETED). It returns ONLY the wire
// reply — a live-build skip signal is dropped here, because the shared wire type cannot carry it;
// in-process callers that can REPORT a skip use runRetentionOutcome.
func runRetention(req spec.RetentionRequest) spec.RetentionReply {
	return runRetentionOutcome(req, false).Reply
}

// retentionOutcome is what ONE engine run decided: the WIRE reply PLUS the live-build skip
// signals, one per guarded sweep. spec.RetentionReply is the CUE-generated SHARED wire type and
// has no field for "a guarded sweep declined", so the signals travel BESIDE it here: the
// plugin's own CLI (command.go) prints them, and runRetention — the verb:retention entry point
// peer plugins call — drops them. Carrying them across the wire to those peer callers (plugin-box's
// post-build prune, plugin-check's post-run prune) needs a new field on the shared
// #RetentionReply schema in opencharly/spec plus a pin bump: a cross-repo change, deliberately
// not guessed at here.
type retentionOutcome struct {
	Reply spec.RetentionReply

	// DanglingSkip is set when the charly-labeled dangling sweep (the `images` category)
	// declined on the live-build guard, never when it merely found nothing.
	DanglingSkip *retentionSkip

	// StagingSkip is set when the buildah/podman staging sweep (the `images` category)
	// declined on the same guard.
	StagingSkip *retentionSkip

	// DeepSkip is set when the store-wide dangling sweep (the `deep` category) declined.
	DeepSkip *retentionSkip

	// Scopes is the unit list of the transient build-scope reaper (the `scopes` category): the
	// scopes it stopped, or would stop under a dry run. It rides HERE, not on spec.RetentionReply,
	// for the same reason the skip signals do: the reaper is a HOST-HYGIENE surface reached only by
	// `charly clean`'s own CLI — no peer asks for it over verb:retention (a peer's post-build or
	// post-run prune must never stop a scope) — and it needs neither the resolved keep-defaults nor
	// the container engine (see runRetentionOutcome's needsEngine: only the store-touching categories
	// resolve one). Adding a field to the shared wire type to carry a reply no peer reads would be a
	// cross-repo change for no consumer.
	Scopes []string

	// ScopeSkip is set when the scope reaper declined on a guard (a live build, or no user systemd
	// session), never when it merely found nothing.
	ScopeSkip *retentionSkip
}

// runRetentionOutcome runs the requested category(ies) and returns the wire reply plus the
// live-build skip signal(s) — the form every caller that can REPORT a skip (the plugin's own CLI)
// uses, so the guard's decision is made once, in the engine, and never re-derived by a caller.
// scopes is the CLI-only transient build-scope category (see retentionOutcome.Scopes); every
// peer/wire caller passes false.
func runRetentionOutcome(req spec.RetentionRequest, scopes bool) retentionOutcome {
	// The engine is resolved ONLY for the categories that touch the container store. Four of them
	// never do: the scope reaper (systemd + /proc), the check-run sweep and the .build staging sweep
	// (filesystem), and the cache GC (the CAS blob store). Resolving it up front made every one of
	// them a hard ERROR on a host with no podman — including `charly clean --scopes`, i.e. the one
	// category that matters most on a host whose engine is broken, since a hung build scope is a
	// residue of a build that already failed. Both this file's and the candy description's "needs
	// neither the engine binary nor the resolved keep-defaults" are therefore true by construction,
	// not by assertion, and TestScopesOnlyRunNeedsNoEngine pins it.
	needsEngine := req.Invalidate != "" || req.List || req.BuildPrune || req.Images || req.Deep
	var engineBin string
	if needsEngine {
		var err error
		if engineBin, err = resolveEngine(); err != nil {
			return retentionOutcome{Reply: spec.RetentionReply{Error: err.Error()}}
		}
	}

	// --invalidate: targeted image-tag invalidation ONLY (matches the CLI's early return).
	if req.Invalidate != "" {
		refs, ierr := invalidateImageTags(engineBin, req.Invalidate, req.DryRun)
		if ierr != nil {
			return retentionOutcome{Reply: spec.RetentionReply{Error: fmt.Sprintf("invalidating image tags: %v", ierr)}}
		}
		return retentionOutcome{Reply: spec.RetentionReply{ImageRefs: refs}}
	}

	// list: the read-only tag inventory (`charly box list tags`) — nothing removed.
	if req.List {
		groups, lerr := charlyImageTags(engineBin)
		if lerr != nil {
			return retentionOutcome{Reply: spec.RetentionReply{Error: fmt.Sprintf("listing image tags: %v", lerr)}}
		}
		return retentionOutcome{Reply: spec.RetentionReply{TagGroups: flattenTagGroups(groups)}}
	}

	keepImages, keepCheck := req.KeepImages, req.KeepCheckRuns
	if req.Keep > 0 {
		keepImages, keepCheck = req.Keep, req.Keep
	}

	// build_prune: the narrow post-`charly box build` scope — tag retention + stale
	// .build/_candy staging dirs ONLY, matching the historic pruneAfterBuild behavior
	// exactly (never the fuller dangling-image/staging sweep the `images` category runs).
	if req.BuildPrune {
		reply := spec.RetentionReply{KeepImages: keepImages}
		refs, perr := pruneImagesByRetention(engineBin, keepImages, req.DryRun)
		if perr != nil {
			return retentionOutcome{Reply: spec.RetentionReply{Error: fmt.Sprintf("pruning images: %v", perr)}}
		}
		reply.ImageRefs = refs
		reply.BuildDirs = pruneBuildCandyDirs(filepath.Join(req.Dir, ".build"), keepImages, req.DryRun)
		return retentionOutcome{Reply: reply}
	}

	out := retentionOutcome{Reply: spec.RetentionReply{KeepImages: keepImages, KeepCheckRuns: keepCheck}}
	reply := &out.Reply

	if req.Images {
		refs, perr := pruneImagesByRetention(engineBin, keepImages, req.DryRun)
		if perr != nil {
			return retentionOutcome{Reply: spec.RetentionReply{Error: fmt.Sprintf("pruning images: %v", perr)}}
		}
		reply.ImageRefs = refs
		dangling, skip, derr := pruneDanglingCharlyImages(engineBin, req.DryRun)
		if derr != nil {
			return retentionOutcome{Reply: spec.RetentionReply{Error: fmt.Sprintf("pruning dangling images: %v", derr)}}
		}
		reply.DanglingIDs = dangling
		out.DanglingSkip = skip
		staging, sskip := pruneBuildahStaging(req.DryRun)
		reply.StagingDirs = staging
		out.StagingSkip = sskip
		reply.BuildDirs = pruneBuildCandyDirs(filepath.Join(req.Dir, ".build"), keepImages, req.DryRun)
	}
	if req.Check {
		paths, perr := pruneCheckRuns(filepath.Join(req.Dir, ".check"), keepCheck, req.DryRun)
		if perr != nil {
			return retentionOutcome{Reply: spec.RetentionReply{Error: fmt.Sprintf("pruning check runs: %v", perr)}}
		}
		reply.CheckPaths = paths
	}
	if req.Deep {
		ids, bytes, skip, derr := pruneDeepDanglingImages(engineBin, req.DryRun)
		if derr != nil {
			return retentionOutcome{Reply: spec.RetentionReply{Error: fmt.Sprintf("deep-purging dangling images: %v", derr)}}
		}
		reply.DeepIDs = ids
		reply.DeepBytes = bytes
		out.DeepSkip = skip
	}
	if req.Cache {
		stores, cerr := gcCacheStores(req.DryRun)
		if cerr != nil {
			return retentionOutcome{Reply: spec.RetentionReply{Error: fmt.Sprintf("GCing the cache stores: %v", cerr)}}
		}
		reply.CacheStores = stores
	}
	if scopes {
		reaped, skip, serr := reapBuildScopes(req.DryRun)
		if serr != nil {
			return retentionOutcome{Reply: spec.RetentionReply{Error: fmt.Sprintf("reaping transient build scopes: %v", serr)}}
		}
		out.Scopes = reaped
		out.ScopeSkip = skip
	}
	return out
}

// gcCacheStores reclaims unreferenced blobs from EVERY named `spec/cache`
// ArtifactStore under the cache root (the `cache` category, `charly clean
// --cache`). A replaced/deleted ArtifactStore entry leaves its superseded blobs
// behind (content addressing means a new key is a NEW manifest + blobs); each
// store's own GC reclaims them, bounded to its own entry cap. No live-build
// guard is needed here — this touches only the CAS blob store, never the podman
// image store the guard protects. An empty/absent cache root is an empty list,
// never an error. Read-only under dry_run (GCStats computes the same set without
// removing).
func gcCacheStores(dryRun bool) ([]spec.CacheStoreInfo, error) {
	names, err := cache.NamedStores()
	if err != nil {
		return nil, err
	}
	var out []spec.CacheStoreInfo
	for _, name := range names {
		l := cache.OpenNamedLayout(name)
		reclaim, gerr := l.GCStats(dryRun)
		if gerr != nil {
			return nil, fmt.Errorf("store %q: %w", name, gerr)
		}
		out = append(out, spec.CacheStoreInfo{
			Name:         name,
			Entries:      int64(l.Len()),
			RemovedBlobs: int64(reclaim.RemovedBlobs),
			RemovedBytes: reclaim.RemovedBytes,
		})
	}
	return out, nil
}

// resolveEngineBinary resolves the container engine binary via kit.ResolveRuntime — the
// same resolver every other engine-shelling site uses.
// resolveEngine is the engine-resolution seam (a package-level var for the same reason
// liveBuildFloor / listDanglingImages / listBuildScopes are): a test can prove which categories
// require it without an engine on PATH, which is exactly the property Block A1 asked this cutover
// to pin rather than assert.
var resolveEngine = resolveEngineBinary

func resolveEngineBinary() (string, error) {
	rt, err := kit.ResolveRuntime()
	if err != nil {
		return "", err
	}
	return kit.EngineBinary(rt.BuildEngine), nil
}

// --- image-tag inventory (charlyImageTags + support) -------------------------------------

// listContainerImageRefs returns the set of image IDs and image refs currently
// referenced by ANY container (running or stopped, incl. quadlet-managed
// deploys). Package-level var for testability (same pattern as kit.ListLocalImages).
var listContainerImageRefs = defaultContainerImageRefs

func defaultContainerImageRefs(engine string) (ids map[string]bool, refs map[string]bool, err error) {
	ids = map[string]bool{}
	refs = map[string]bool{}
	// Parse JSON, not a Go-template `--format`: podman's `{{.ImageID}}` template
	// panics (slice bounds [:12] length 0) when any container has an empty image
	// ID. The raw JSON field handles that gracefully.
	out, e := exec.Command(kit.EngineBinary(engine), "ps", "-a", "--format", "json").Output()
	if e != nil {
		return ids, refs, fmt.Errorf("listing containers via %s: %w", kit.EngineBinary(engine), e)
	}
	var rows []map[string]any
	if e := json.Unmarshal(out, &rows); e != nil {
		return ids, refs, fmt.Errorf("parsing %s ps output: %w", kit.EngineBinary(engine), e)
	}
	for _, r := range rows {
		if v, ok := r["ImageID"].(string); ok {
			if id := normImageID(v); id != "" {
				ids[id] = true
			}
		}
		if v, ok := r["Image"].(string); ok && v != "" {
			refs[v] = true
		}
	}
	return ids, refs, nil
}

// normImageID strips the "sha256:" prefix so short (12-char) and full (64-char)
// IDs compare by prefix.
func normImageID(s string) string { return strings.TrimPrefix(strings.TrimSpace(s), "sha256:") }

// imageInUse reports whether the candidate image is referenced by any container,
// by ID (prefix-tolerant: 12-char vs 64-char) or by any of its tags.
func imageInUse(im kit.LocalImageInfo, ids, refs map[string]bool) bool {
	cid := normImageID(im.ID)
	for id := range ids {
		if cid != "" && id != "" && (strings.HasPrefix(cid, id) || strings.HasPrefix(id, cid)) {
			return true
		}
	}
	for _, n := range im.Names {
		if refs[n] {
			return true
		}
	}
	return false
}

// imageContentIdentity is the ONE identity key retention uses: the image's content identity —
// the digest the engine reports as the image ID (see normImageID). It answers WHICH artifact a row
// names, never WHICH one is newer: two rows sharing it are ONE artifact wearing two tags, so
// `keep_images: N` budgets them once (retentionRanks).
//
// It replaces imageLabelCalVer, which read `ai.opencharly.version` as the retention ordering
// PRIMARY KEY. That label is no longer emitted — the schema-versioning-removal cutover deleted the
// author-declared aggregated image version it carried (deploykit.ComputeEffectiveVersions and
// ResolvedBox.Version are gone; sdk/deploykit/write_labels.go writes no ai.opencharly.version) —
// and the old key was worse than merely dead, because absence is a CUTOVER-BOUNDARY property, not
// a steady state: on any store spanning that boundary the sort's "labelled row sorts before
// unlabelled" tiebreak ranked PRE-cutover images above POST-cutover ones REGARDLESS of build time.
// That is an ordering INVERSION which can select the NEWEST image for removal and keep the oldest
// (opencharly/plugin-clean#10; the operator measured 202 of 238 local images still carrying a
// parseable label on 2026-10-03, so the inversion is reachable, not theoretical).
//
// Identity is content-addressed here for the same reason the org keys its caches on content
// (spec/cache's Entry.Components, loaderkit's components digest): it is derived from the artifact,
// so it cannot drift from what the artifact IS.
func imageContentIdentity(im kit.LocalImageInfo) string { return normImageID(im.ID) }

// imageLabelCalVer parses the image's `ai.opencharly.version` label. The label is no longer
// emitted (see imageContentIdentity) and is NOT an ordering key: this read survives ONLY for the
// `charly box list tags` payload, whose published wire field (spec.TagInfo.Version,
// spec/schema/clean.cue) is defined as exactly this label and documents "-" when it is absent.
// Deleting that field is the `spec` repo's half of this sweep (opencharly/plugin-clean#10 names
// spec/container/box_metadata_coneb.go and spec/schema/resolvedbox.cue); until it lands, do NOT
// re-wire this into the sort or into the exemption guard — that restores the inversion.
func imageLabelCalVer(im kit.LocalImageInfo) (kit.CalVer, bool) {
	return kit.ParseCalVer(im.Labels[spec.LabelVersion])
}

// imageTagInfo is one locally stored tag of a charly-labeled image — the shared
// inventory row behind retention pruning, `charly box list tags`, and
// `charly clean --invalidate`.
type imageTagInfo struct {
	Ref string
	// ID is the row's CONTENT IDENTITY (imageContentIdentity: the image-config digest, i.e. the
	// engine's image ID). It is the ONE identity key: retentionRanks groups rows by it, so every
	// tag of one artifact shares one distinct-image rank. It plays NO part in ordering.
	ID string
	// LabelCalVer/OkLabel are the `ai.opencharly.version` label — a NEVER-EMITTED key (see
	// imageContentIdentity) that retention no longer orders or exempts by. They survive only to
	// populate the published `charly box list tags` version column (spec.TagInfo.Version); do not
	// reintroduce them into the sort or the exemption guard.
	LabelCalVer kit.CalVer
	OkLabel     bool
	TagCalVer   kit.CalVer
	OkTag       bool
	InUse       bool
	// Created is the image's creation time (unix seconds) — the build-recency key, total over
	// every tag charly mints, and therefore the ordering's PRIMARY key. The CalVer keys are NOT:
	// `charly box build --tag` REPLACES the CalVer tag, so a bed build carries
	// `check-<bed>-<calver>`, which parses as no CalVer at all. A group of bed-tagged images
	// therefore had OkTag false for EVERY member, the sort's final comparator was false for every
	// pair, and the surviving order was whatever `podman images` happened to emit — so
	// keep_images: N kept an ARBITRARY N and could delete the newest build while keeping older
	// ones. Unlike the resolver's wrong ANSWER, this destroys an artifact.
	Created int64
}

// charlyImageTags inventories local storage: one row PER TAG (deduped by
// ref), grouped by the ai.opencharly.box label and sorted newest-first by
// CREATION TIME (build-tag CalVer tiebreak). Non-charly images (no label) never appear.
func charlyImageTags(engine string) (map[string][]imageTagInfo, error) {
	imgs, err := kit.ListLocalImages(engine)
	if err != nil {
		return nil, err
	}
	inUseIDs, inUseRefs, err := listContainerImageRefs(engine)
	if err != nil {
		return nil, err
	}
	groups := map[string][]imageTagInfo{}
	seenRef := map[string]bool{}
	for _, im := range imgs {
		short := im.Labels[spec.LabelBox]
		if short == "" {
			continue
		}
		lcv, okL := imageLabelCalVer(im)
		inUse := imageInUse(im, inUseIDs, inUseRefs)
		for _, ref := range im.Names {
			if seenRef[ref] {
				continue
			}
			seenRef[ref] = true
			tcv, okT := kit.ParseCalVer(kit.ExtractCalVerTag(ref))
			groups[short] = append(groups[short], imageTagInfo{
				Ref: ref, ID: imageContentIdentity(im), LabelCalVer: lcv, OkLabel: okL,
				TagCalVer: tcv, OkTag: okT, InUse: inUse, Created: im.Created,
			})
		}
	}
	for _, group := range groups {
		sort.SliceStable(group, func(i, j int) bool {
			// CREATION TIME is the ordering's primary — and only — recency key, because it is the
			// one key TOTAL over the tags charly mints (see imageTagInfo.Created). The former
			// primary key was the `ai.opencharly.version` label, whose absence on post-cutover
			// images made the "labelled sorts before unlabelled" tiebreak an ordering INVERSION
			// that could delete the NEWEST image: opencharly/plugin-clean#10. Content identity
			// (imageContentIdentity) deliberately does NOT order — it decides which rows are one
			// artifact, which is retentionRanks' job.
			if group[i].Created != group[j].Created && group[i].Created != 0 && group[j].Created != 0 {
				return group[i].Created > group[j].Created // newer build first
			}
			if group[i].OkTag && group[j].OkTag && group[i].TagCalVer != group[j].TagCalVer {
				return group[j].TagCalVer.Less(group[i].TagCalVer) // newer build first
			}
			return group[i].OkTag && !group[j].OkTag // dateable sorts before undateable
		})
	}
	return groups, nil
}

// flattenTagGroups presents charlyImageTags' grouped inventory as the verb:retention
// `list` reply payload: boxes sorted alphabetically, tags within each box in the
// group's existing newest-first order. Version is the `ai.opencharly.version` label — a key
// nothing emits any more, so it reads "-" on every current row; it survives because
// spec.TagInfo.Version's published contract is defined as exactly that label and its removal is
// the `spec` half of the opencharly/plugin-clean#10 sweep (see imageLabelCalVer).
func flattenTagGroups(groups map[string][]imageTagInfo) []spec.TagInfo {
	boxes := make([]string, 0, len(groups))
	for b := range groups {
		boxes = append(boxes, b)
	}
	sort.Strings(boxes)
	var out []spec.TagInfo
	for _, b := range boxes {
		for _, t := range groups[b] {
			version := "-"
			if t.OkLabel {
				version = t.LabelCalVer.String()
			}
			out = append(out, spec.TagInfo{Box: b, Ref: t.Ref, Version: version, InUse: t.InUse})
		}
	}
	return out
}

// --- live-build-floor + image-tag retention ------------------------------------------------

// liveBuildFloor is a package-level var for testability — the same seam
// kit.ListLocalImages / listContainerImageRefs use. It reads the HOST-GLOBAL
// build-activity lock dir (~/.cache/charly/locks/builds, shared across every
// worktree on the host, written by charly core's acquireBuildActivityLock), so a
// retention test that does NOT stub it is non-deterministic: a concurrent build in
// ANOTHER process holds a lock, the live-build protection engages, and the test's
// expected pruning silently does not happen. A test stubs this to a fixed
// (no-live-build) result so the retention decision under test is deterministic
// regardless of host build activity.
var liveBuildFloor = defaultLiveBuildFloor

// The live-build guard's two reasons, one per guarded sweep family — kept as constants so the
// CLI, the tests, and the engine all name the SAME cause text.
const (
	// skipReasonImages: image-removing sweeps (dangling-image reaper, charly-labeled and
	// store-wide).
	skipReasonImages = "images are never removed during a build"
	// skipReasonStaging: the /var/tmp buildah-podman staging reaper (an in-flight build's
	// staging dir is LIVE data, not dead weight).
	skipReasonStaging = "buildah/podman staging of an in-flight build is never swept"
	// skipReasonScopes: the transient build-scope reaper (a live build's own scope is not hung).
	skipReasonScopes = "transient build scopes of an in-flight build are never reaped"
	// skipReasonScopesNoSystemd: the scope reaper found no user systemd session to enumerate
	// transient scopes in (a headless/container host) — nothing to enumerate, nothing to reap.
	skipReasonScopesNoSystemd = "no user systemd session to enumerate transient build scopes"
)

// retentionSkip is a live-build-guarded sweep's DECISION TO DECLINE, carried OUT of the engine so
// a caller — and the operator reading its output — can tell "the guard stopped me" apart from
// "there was nothing to remove". Both cases return the same empty id list; only this signal
// distinguishes them, and conflating them is exactly how `deep: removed 0 untagged image(s)
// store-wide` was read as "the tool is blind" while 63 removable untagged images (tens of GB)
// sat in the store — an operator then reached for raw `podman rmi -f`, which deletes the very
// build-layer cache that makes the next build ~8x faster.
//
// The guard's WHEN is unchanged and deliberately so (an untagged intermediate may be the parent
// of an in-flight build); this type only makes its decision observable.
type retentionSkip struct {
	live   int
	reason string
}

// String is the user-visible form: the exact line the CLI prints IN PLACE OF the removed count
// for the skipped category. The live-build clause is omitted when there is no live build to name
// (the scope reaper's no-systemd-session case), so the line never claims a build count it did not
// measure.
func (s retentionSkip) String() string {
	if s.live <= 0 {
		return fmt.Sprintf("SKIPPED — %s", s.reason)
	}
	return fmt.Sprintf("SKIPPED — %d build(s) in flight; %s (re-run when builds are idle)", s.live, s.reason)
}

// defaultLiveBuildFloor scans the build-activity locks (kit.BuildActivityDir): a
// lock file whose flock is ACQUIRABLE is stale (its build died) and is reaped;
// a HELD one is a LIVE build whose recorded generate CalVer floors every FROM
// pin it may still resolve. Returns the minimum live CalVer, whether that floor
// is usable, and the live-build count — a live lock with an unreadable CalVer
// forces floorOK=false, so the caller protects everything.
func defaultLiveBuildFloor() (floor kit.CalVer, floorOK bool, live int) {
	dir, err := kit.BuildActivityDir()
	if err != nil {
		return kit.CalVer{}, false, 0
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return kit.CalVer{}, false, 0
	}
	haveFloor := false
	floorOK = true
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if rel, lerr := kit.AcquireFileLock(p, false); lerr == nil {
			_ = rel()
			_ = os.Remove(p) // stale — its build died without releasing
			continue
		}
		live++
		var cv kit.CalVer
		ok := false
		if b, rerr := os.ReadFile(p); rerr == nil {
			cv, ok = kit.ParseCalVer(strings.TrimSpace(string(b)))
		}
		if !ok {
			floorOK = false
			continue
		}
		if !haveFloor || cv.Less(floor) {
			floor, haveFloor = cv, true
		}
	}
	if live == 0 {
		return kit.CalVer{}, false, 0
	}
	if !haveFloor {
		floorOK = false
	}
	return floor, floorOK, live
}

// retentionRemovable is the pure retention decision for one inventoried tag:
// the standing rules (keep every tag of the newest keepN DISTINCT images, never
// remove an undatable tag, never an in-use one) plus the build-activity
// protections. `rank` is the tag's DISTINCT-IMAGE rank within its box group —
// not its index among tags: keep_images budgets IMAGES, and a tag is kept when
// the image it names is inside the budget, however many tags that image wears
// (see imageRanksByDistinctID) — while ANY build
// is live, (a) a tag at or above the oldest live build's generate CalVer may
// still be FROM-resolved and is kept (an unknown floor keeps everything), and
// (b) an image's LAST local tag is never removed (an outright mid-build image
// deletion corrupts buildah's layer store — the layer-not-known/SIGSEGV
// variant the fan-out surfaced).
func retentionRemovable(c imageTagInfo, rank, keepN int, floor kit.CalVer, floorOK bool, live int, lastTag bool) bool {
	if rank < keepN {
		return false // keep every tag of the newest keepN DISTINCT images
	}
	// ORDER IS LOAD-BEARING — do not reorder these guards. The undatable check sits AFTER the
	// rank check, so a tag that falls outside either retention ordinal still reaches this guard
	// and is protected when it qualifies. Hoisting the rank check below this one would change
	// which rows are even considered.
	//
	// There is exactly ONE datable key left, and it is the tag: the `ai.opencharly.version` label
	// that this guard used to read as a second one is never emitted any more (see
	// imageContentIdentity), so an AND over two keys would today be an AND with a constant — and,
	// on a store spanning the cutover, would resurrect exactly the key whose ordering inversion is
	// opencharly/plugin-clean#10. A row is therefore exempt ONLY when its tag carries no
	// `:YYYY.DDD.HHMM`, which is the fail-safe direction: `latest`, `dev`, and a bare ref are never
	// reclaimed, while a `check-<bed>-<calver>` tag (datable by its LABEL position, not by
	// ExtractCalVerTag) is likewise exempt. Surplus CalVer tag rows — the case keep_images'
	// tag-ordinal half exists for — remain reclaimable, which is what keeps a content-stable
	// image's tag rows bounded (TestPruneImagesByRetention_SharedID).
	if !c.OkTag {
		return false // never remove a tag we cannot date by the ONE remaining datable key
	}
	if c.InUse {
		return false // image referenced by a container/deploy
	}
	if live > 0 {
		if !floorOK {
			return false
		}
		if c.OkTag && !c.TagCalVer.Less(floor) {
			return false
		}
		if lastTag {
			return false
		}
	}
	return true
}

// retentionRanks maps each tag in a newest-first box group onto the TWO ordinals `keep_images: N`
// budgets, because one number alone cannot express what retention has to protect:
//
//   - imageRank — the rank of the DISTINCT IMAGE the tag names, where a distinct image is a
//     distinct CONTENT IDENTITY (imageContentIdentity: the image-config digest). Every tag of the
//     newest image ranks 0, every tag of the next distinct image ranks 1, and so on.
//   - tagOrd — the tag's ordinal WITHIN its own image, newest first.
//
// A tag survives when BOTH are inside the budget, i.e. `keep_images: N` keeps the newest N
// distinct images AND at most N tags of each. Both halves are load-bearing and each one alone
// regresses the other:
//
//   - Ranking by TAG INDEX alone (the form this replaces) let ONE image wearing three tags consume
//     the whole `keep_images: 3` budget, so the 2nd and 3rd DISTINCT images fell outside it and
//     were pruned — measured live on `fedora-nonfree` (id e2efeb1c). Direction matters: that made
//     retention keep FEWER distinct images, so it OVER-pruned within a managed family. It could
//     never cause under-reclaim, and it is not an explanation for any observed disk shortfall.
//   - Ranking by IMAGE alone reclaims no tags at all: a content-stable image rebuilt many times
//     wears every CalVer tag it ever had at imageRank 0, so its tag rows would grow without bound.
//     TestPruneImagesByRetention_SharedID is that invariant, and it caught this exact regression
//     when the first cut of this fix budgeted images only.
//
// An entry with no resolvable ID gets an imageRank of its own rather than joining a neighbour's:
// two unidentifiable rows are not evidence of one image, and merging them would silently widen
// what the budget protects. Such rows are usually also undatable, which retentionRemovable refuses
// to remove on a separate axis.
func retentionRanks(group []imageTagInfo) (imageRank, tagOrd []int) {
	imageRank = make([]int, len(group))
	tagOrd = make([]int, len(group))
	byID := map[string]int{}
	seenTags := map[string]int{}
	next := 0
	for i, c := range group {
		if c.ID == "" {
			imageRank[i], tagOrd[i] = next, 0
			next++
			continue
		}
		r, seen := byID[c.ID]
		if !seen {
			r = next
			byID[c.ID] = r
			next++
		}
		imageRank[i] = r
		tagOrd[i] = seenTags[c.ID]
		seenTags[c.ID]++
	}
	return imageRank, tagOrd
}

func pruneImagesByRetention(engine string, keepN int, dryRun bool) ([]string, error) {
	if keepN <= 0 {
		return nil, nil
	}
	groups, err := charlyImageTags(engine)
	if err != nil {
		return nil, err
	}
	floor, floorOK, live := liveBuildFloor()
	tagCount := map[string]int{}
	for _, group := range groups {
		for _, c := range group {
			if c.ID != "" {
				tagCount[c.ID]++
			}
		}
	}
	var removed []string
	for _, group := range groups {
		imageRank, tagOrd := retentionRanks(group)
		for idx, c := range group {
			lastTag := c.ID != "" && tagCount[c.ID] <= 1
			// BOTH ordinals must be inside the budget for a tag to survive — this is an AND, not
			// an OR. A tag whose IMAGE is inside the budget but which is itself a surplus tag of
			// that image (tagOrd >= keepN) is re-ranked to its tag ordinal and becomes removable;
			// that is the half that keeps a content-stable image's tag rows bounded. Reading this
			// as "either axis keeps it" invites deleting the reassignment below, which would
			// restore the unbounded tag growth TestPruneImagesByRetention_SharedID guards.
			// Verified by probe, not by reading: 1 image / 5 tags / keepN=3 removes 2.
			rank := imageRank[idx]
			if rank < keepN && tagOrd[idx] >= keepN {
				rank = tagOrd[idx] // surplus tag of a kept image — budget it as a tag
			}
			if !retentionRemovable(c, rank, keepN, floor, floorOK, live, lastTag) {
				continue
			}
			if dryRun {
				if c.ID != "" {
					tagCount[c.ID]--
				}
				removed = append(removed, c.Ref)
				continue
			}
			// rmi WITHOUT -f untags this ref while other tags of a shared id
			// survive; it also refuses an image still held by a build /
			// "external" container our InUse pre-check can't see — the
			// safety backstop. Silent skip — in-use retention is expected.
			if err := exec.Command(kit.EngineBinary(engine), "rmi", c.Ref).Run(); err != nil {
				continue
			}
			if c.ID != "" {
				tagCount[c.ID]--
			}
			removed = append(removed, c.Ref)
		}
	}
	return removed, nil
}

// --- dangling-image sweeps (default charly-labeled + --deep store-wide) --------------------

// listDanglingImages lists every UNTAGGED (dangling) image in local storage — charly-built or
// not. Package-level var for testability (same pattern as kit.ListLocalImages /
// listContainerImageRefs): a test stubs this so the pure selection + dry-run logic in
// pruneDanglingImages is deterministic without touching the real engine.
var listDanglingImages = defaultListDanglingImages

func defaultListDanglingImages(engine string) ([]kit.LocalImageInfo, error) {
	out, err := exec.Command(kit.EngineBinary(engine), "images", "--all", "--filter", "dangling=true", "--format", "json").Output()
	if err != nil {
		return nil, fmt.Errorf("listing dangling images: %w", err)
	}
	return kit.ParseLocalImagesJSON(out)
}

// selectDanglingImages is the PURE selection predicate behind both dangling-image sweeps: given
// every listed dangling image, decide which are candidates for removal. onlyCharly=true is the
// default `charly clean` sweep (only images carrying the ai.opencharly.box label — never a
// foreign image); onlyCharly=false is the `charly clean --deep` store-wide sweep (every untagged
// image, INCLUDING unlabeled multi-stage build intermediates — WriteLabels stamps
// ai.opencharly.* only on the FINAL build stage, so an intermediate stage image is never
// charly-labeled and is invisible to the default sweep; this is the R4-closing gap --deep
// exists to close). No InUse check here: a dangling (untagged) image is essentially never a
// running container's OWN image reference, and the real backstop is the unforced `rmi` at
// removal time (below), which refuses anything still referenced by a container or a kept tag.
func selectDanglingImages(imgs []kit.LocalImageInfo, onlyCharly bool) []kit.LocalImageInfo {
	var out []kit.LocalImageInfo
	for _, im := range imgs {
		if onlyCharly && im.Labels[spec.LabelBox] == "" {
			continue // not charly-built
		}
		out = append(out, im)
	}
	return out
}

// pruneDanglingImages is the shared engine behind the charly-labeled dangling sweep (default
// `charly clean`) and the store-wide `--deep` purge (`charly clean --deep`): list, select
// (selectDanglingImages), then `rmi` each candidate — WITHOUT -f, so an image still referenced
// by a container or held by an in-flight build is refused and silently skipped (the same
// backstop tag retention relies on). Guarded like every other image-removing sweep in this file:
// never while ANY build is live (an untagged intermediate may be a parent of an in-flight
// build) — and when that guard fires it returns a retentionSkip naming the in-flight build
// count, so the caller can report the DECLINE instead of printing an empty result that reads
// like an empty store. Returns the removed (or would-remove, under dryRun) image IDs, the sum of
// their reported Size in bytes, and the skip signal (nil unless the live-build guard declined).
// This byte total is an UPPER BOUND on actual reclaimed disk, NOT a prediction: podman's
// per-image Size counts every layer the image references, and dangling images routinely SHARE
// layers with images that stay (retained tags, other dangling images) — removing one image frees
// only the layers it held UNIQUELY.
func pruneDanglingImages(engine string, onlyCharly, dryRun bool) ([]string, int64, *retentionSkip, error) {
	if _, _, live := liveBuildFloor(); live > 0 {
		return nil, 0, &retentionSkip{live: live, reason: skipReasonImages}, nil // never delete images while any build is in flight
	}
	imgs, err := listDanglingImages(engine)
	if err != nil {
		return nil, 0, nil, err
	}
	var removed []string
	var totalBytes int64
	for _, im := range selectDanglingImages(imgs, onlyCharly) {
		if dryRun {
			removed = append(removed, im.ID)
			totalBytes += im.Size
			continue
		}
		if err := exec.Command(kit.EngineBinary(engine), "rmi", im.ID).Run(); err != nil {
			continue // parent of a kept image / in use — expected, keep
		}
		removed = append(removed, im.ID)
		totalBytes += im.Size
	}
	return removed, totalBytes, nil, nil
}

// pruneDanglingCharlyImages removes UNTAGGED (dangling) charly-built images — the residue
// tag-retention leaves behind (an untagged id) plus dead build intermediates that happen to
// carry the ai.opencharly.box label. The default `charly clean` category; see
// pruneDanglingImages for the shared engine and the skip signal.
func pruneDanglingCharlyImages(engine string, dryRun bool) ([]string, *retentionSkip, error) {
	ids, _, skip, err := pruneDanglingImages(engine, true, dryRun)
	return ids, skip, err
}

// pruneDeepDanglingImages removes EVERY untagged (dangling) image in local storage — the
// store-wide `charly clean --deep` category. Unlike pruneDanglingCharlyImages, it is NOT
// restricted to the ai.opencharly.box label. Removing a dangling image via `rmi` also frees
// any layer blobs it alone referenced, so this is EFFECTIVELY a dangling-image-plus-unused-
// layer prune with a single engine call per image, not two.
func pruneDeepDanglingImages(engine string, dryRun bool) ([]string, int64, *retentionSkip, error) {
	return pruneDanglingImages(engine, false, dryRun)
}

// buildahStagingGlobs are the /var/tmp staging-dir patterns buildah/podman
// leave behind when a commit dies mid-write (ENOSPC, SIGKILL) — dead weight no
// engine command reclaims. Swept only when no build is live, and only dirs
// owned by the current user (rootless storage).
var buildahStagingGlobs = []string{
	"/var/tmp/container_images_storage*",
	"/var/tmp/buildah*",
}

// pruneBuildahStaging removes dead buildah/podman staging dirs (see
// buildahStagingGlobs). Live-build-guarded like the dangling reaper, and like it the guard's
// decision is returned as a retentionSkip (nil = the sweep ran).
func pruneBuildahStaging(dryRun bool) ([]string, *retentionSkip) {
	if _, _, live := liveBuildFloor(); live > 0 {
		return nil, &retentionSkip{live: live, reason: skipReasonStaging}
	}
	uid := os.Getuid()
	var removed []string
	for _, g := range buildahStagingGlobs {
		matches, _ := filepath.Glob(g)
		for _, m := range matches {
			st, err := os.Stat(m)
			if err != nil || !st.IsDir() {
				continue
			}
			if sys, ok := st.Sys().(*syscall.Stat_t); !ok || int(sys.Uid) != uid {
				continue // not ours (rootless scope only)
			}
			if dryRun {
				removed = append(removed, m)
				continue
			}
			if err := os.RemoveAll(m); err != nil {
				continue
			}
			removed = append(removed, m)
		}
	}
	return removed, nil
}

// --- transient build-scope reaping (the `scopes` category) ----------------------------------

// WHO creates these. `charly box build` (and every image build a bed runs) builds through
// buildah/podman, which puts each BUILD CONTAINER into its own transient systemd user scope named
// `<runtime>-<container-name>.scope`; podman names the build container `buildah-<id>`. MEASURED
// live (RDD spike, 2026-10-07) by building a two-line Containerfile on this host:
//
//	$ systemctl --user list-units --type=scope --all --no-legend | grep buildah
//	runc-buildah-buildah2754860408.scope  loaded active running  libcontainer container buildah-buildah2754860408
//
// That family is the COMPLETE list of transient scopes charly can create on a host. Image builds
// are the only charly operation that starts a transient scope: bed pods and every
// `charly config`/`start` service are quadlet systemd SERVICES (`charly-*.service`, not scopes),
// and a nested podman inside a bed pod creates its scopes in the POD's own cgroup namespace,
// invisible from here. Other software's scopes (`dsh-subprocess-*.scope` from the agent harness,
// `podman-pause-*.scope` for a paused container) are deliberately NOT in the family: reaping a
// scope another owner created, and whose lifetime that owner still depends on, is not charly's
// mandate.
//
// WHY reap. A build step that hangs leaves its scope behind FOREVER, because the only thing that
// would have stopped it — the build's own teardown — died with the builder:
// opencharly/plugin-clean#11 measured `runc-buildah-buildah986455857.scope` ACTIVE for SEVEN DAYS
// with 13 tasks inside, a `node ... | xargs npm install -g` step spinning at 309% CPU after its
// builder parent was gone, taking the host to 12.27 load on 16 cores. Nothing reaped it: the run's
// metadata was gone with its worktree, so `charly reap-orphans` (a probe over a project's recorded
// deploy state) could not see it, and `charly clean` had no category for it. Clearing it took a
// hand `systemctl --user stop` plus `kill -9` of reparented survivors.
//
// THE DECISION — pure and testable (selectReapableBuildScopes). A scope is reapable iff ALL of:
//
//	1. its unit name is in the build-scope family (buildScopeNameRe);
//	2. it has been ACTIVE for at least defaultBuildScopeMaxAge — a live build is far shorter, and
//	   a legitimately long one is additionally protected by rule 3;
//	3. NO live builder parent exists anywhere on the host (no `buildah`/`podman` process), so no
//	   build is in flight that could still own it. This is the issue's own rule and it is
//	   deliberately conservative: it refuses whenever a builder runs at all, including a
//	   buildah/podman invocation charly did not start.
//
// The sweep ADDITIONALLY declines wholesale while charly's own build-activity lock is held
// (liveBuildFloor), the same guard every other destructive category uses, so a `charly clean`
// racing another session's build can never touch that build's scope.
//
// REAPING is `systemctl --user stop <unit>`: the scope's default KillMode=control-group tears the
// whole cgroup down, so the hung step and its descendants die with it. MEASURED in the same spike
// against a real scope of this family whose launcher had exited:
//
//	$ systemctl --user stop runc-buildah-buildahSPIKE111.scope   # rc=0
//	$ systemctl --user show runc-buildah-buildahSPIKE111.scope -p LoadState
//	LoadState=not-found
//	$ ls /sys/fs/cgroup/.../runc-buildah-buildahSPIKE111.scope   # No such file or directory
//	$ ps -p <the surviving sleep pid>                            # gone
//
// The unit name is matched, never a PID: a scope's own name is the only handle that survives the
// builder, and nothing here needs to know which step inside it hung.

// defaultBuildScopeMaxAge is how long a build-family scope may stay ACTIVE with no live builder
// before it is treated as hung. One hour is an order of magnitude above the longest healthy
// image build in flight on this host, and the live-builder guard protects the exceptions.
const defaultBuildScopeMaxAge = time.Hour

// buildScopeNameRe matches the transient scope podman/buildah gives a BUILD container:
// `<runtime>-buildah-<id>.scope` (measured: runc-buildah-buildah2754860408.scope). The runtime
// token varies with the configured OCI runtime (runc/crun), so it is matched generically.
var buildScopeNameRe = regexp.MustCompile(`^[a-z][a-z0-9]*-buildah-.*\.scope$`)

// buildScope is one ACTIVE transient build scope, as enumerated from the user systemd session.
type buildScope struct {
	// Unit is the systemd unit name (e.g. runc-buildah-buildah2754860408.scope).
	Unit string
	// AgeSeconds is how long the unit has been active, derived from its
	// ActiveEnterTimestampMonotonic against the kernel's monotonic clock (/proc/uptime) — the same
	// clock, so no wall-clock or timezone assumption enters the decision.
	AgeSeconds int64
}

// Seams — package-level vars for testability, the same pattern liveBuildFloor / listDanglingImages
// use. A test stubs all three so the reap decision is deterministic without a systemd session, a
// live engine, or a real scope.
var (
	listBuildScopes     = defaultListBuildScopes
	liveBuilderParents  = defaultLiveBuilderParents
	stopBuildScope      = defaultStopBuildScope
	buildScopeMaxAge    = defaultBuildScopeMaxAge
	userSystemdBusReady = defaultUserSystemdBusReady
)

// defaultUserSystemdBusReady reports whether a user systemd session exists to enumerate scopes in.
// Probed by the session's private socket rather than by parsing a systemctl error string.
func defaultUserSystemdBusReady() bool {
	run := os.Getenv("XDG_RUNTIME_DIR")
	if run == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(run, "systemd", "private"))
	return err == nil
}

// defaultStopBuildScope is the reap primitive: stop one transient scope. The scope's default
// KillMode=control-group takes its whole process tree down with it (measured, see above).
func defaultStopBuildScope(unit string) error {
	return exec.Command("systemctl", "--user", "stop", unit).Run()
}

// uptimeSeconds reads the kernel's monotonic clock (/proc/uptime, first field, seconds). The scope
// ages below are measured against it, so they cannot be skewed by a wall-clock step.
func uptimeSeconds() (int64, error) {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, fmt.Errorf("parsing /proc/uptime: no fields")
	}
	secs, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, fmt.Errorf("parsing /proc/uptime: %w", err)
	}
	return int64(secs), nil
}

// defaultListBuildScopes enumerates the ACTIVE transient build scopes of the user systemd session.
// Only active units are returned (an inactive one is not hung, it is already gone), and a unit
// whose properties cannot be read is reported with AgeSeconds 0 so the age rule REFUSES it —
// failing closed, never open, on an unreadable scope.
func defaultListBuildScopes() ([]buildScope, error) {
	out, err := exec.Command("systemctl", "--user", "list-units", "--type=scope",
		"--all", "--plain", "--no-legend", "--no-pager").Output()
	if err != nil {
		return nil, fmt.Errorf("listing user scopes: %w", err)
	}
	uptime, err := uptimeSeconds()
	if err != nil {
		return nil, err
	}
	var scopes []buildScope
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !buildScopeNameRe.MatchString(fields[0]) {
			continue
		}
		props, perr := exec.Command("systemctl", "--user", "show", fields[0],
			"-p", "ActiveState", "-p", "ActiveEnterTimestampMonotonic").Output()
		if perr != nil {
			scopes = append(scopes, buildScope{Unit: fields[0]})
			continue
		}
		var state string
		var entered int64
		for _, pl := range strings.Split(string(props), "\n") {
			k, v, ok := strings.Cut(strings.TrimSpace(pl), "=")
			if !ok {
				continue
			}
			switch k {
			case "ActiveState":
				state = v
			case "ActiveEnterTimestampMonotonic":
				entered, _ = strconv.ParseInt(v, 10, 64)
			}
		}
		if state != "active" {
			continue
		}
		age := uptime - entered/1_000_000
		if entered == 0 {
			age = 0 // unreadable activation stamp — refuse rather than assume old
		}
		scopes = append(scopes, buildScope{Unit: fields[0], AgeSeconds: age})
	}
	return scopes, nil
}

// defaultLiveBuilderParents reports whether ANY process that can own a build container is alive —
// a `buildah` or a `podman` CLI process. Read straight from /proc (no pgrep dependency, no shell),
// so it is cheap and cannot itself hang.
func defaultLiveBuilderParents() (bool, error) {
	names := map[string]bool{"buildah": true, "podman": true}
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return false, fmt.Errorf("scanning /proc for a live builder: %w", err)
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if _, aerr := strconv.Atoi(e.Name()); aerr != nil {
			continue
		}
		comm, rerr := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if rerr != nil {
			continue
		}
		if names[strings.TrimSpace(string(comm))] {
			return true, nil
		}
	}
	return false, nil
}

// selectReapableBuildScopes is the PURE reaping decision: from every enumerated scope, the ones a
// sweep may stop. It is a function of its arguments alone, so the guard can be pinned by a test
// with no systemd, no /proc scan and no real scope (see TestSelectReapableBuildScopes).
func selectReapableBuildScopes(scopes []buildScope, maxAgeSeconds int64, liveBuilder bool) []buildScope {
	if liveBuilder {
		return nil // a builder is alive — no scope can be proven orphaned
	}
	var out []buildScope
	for _, s := range scopes {
		if !buildScopeNameRe.MatchString(s.Unit) {
			continue // not a scope charly's builds create
		}
		if s.AgeSeconds < maxAgeSeconds {
			continue // still young — a healthy build's scope, or too soon to judge
		}
		out = append(out, s)
	}
	return out
}

// reapBuildScopes reaps (or, under dryRun, reports) every hung transient build scope. It returns
// the reaped/would-reap unit names and — when a guard, never "nothing found", is the reason for an
// empty result — the skip signal the CLI prints in place of a count.
func reapBuildScopes(dryRun bool) ([]string, *retentionSkip, error) {
	if _, _, live := liveBuildFloor(); live > 0 {
		return nil, &retentionSkip{live: live, reason: skipReasonScopes}, nil
	}
	if !userSystemdBusReady() {
		return nil, &retentionSkip{reason: skipReasonScopesNoSystemd}, nil
	}
	scopes, err := listBuildScopes()
	if err != nil {
		return nil, nil, err
	}
	liveBuilder, err := liveBuilderParents()
	if err != nil {
		return nil, nil, err
	}
	var reaped []string
	for _, s := range selectReapableBuildScopes(scopes, int64(buildScopeMaxAge/time.Second), liveBuilder) {
		if dryRun {
			reaped = append(reaped, s.Unit)
			continue
		}
		if err := stopBuildScope(s.Unit); err != nil {
			continue // already gone, or systemd refused — never fatal, never a false claim
		}
		reaped = append(reaped, s.Unit)
	}
	return reaped, nil, nil
}

// --- check-run + build-candy staging retention ----------------------------------------------

// pruneCheckRuns trims each bed/score subdir of checkDir to the newest keepN run
// artifacts: CalVer-named run dirs (bed runs), `runs/<id>` dirs (score
// iterations), and `result-<calver>.yml` files. NOTES.md and any other file are
// always preserved. keepN <= 0 disables. Returns the paths removed.
func pruneCheckRuns(checkDir string, keepN int, dryRun bool) ([]string, error) {
	if keepN <= 0 {
		return nil, nil
	}
	entries, err := os.ReadDir(checkDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var removed []string
	for _, e := range entries {
		if !e.IsDir() {
			continue // top-level files (ISSUE-*.md, etc.) are not run output
		}
		rm, err := pruneOneCheckDir(filepath.Join(checkDir, e.Name()), keepN, dryRun)
		if err != nil {
			return removed, err
		}
		removed = append(removed, rm...)
	}
	return removed, nil
}

func pruneOneCheckDir(bedDir string, keepN int, dryRun bool) ([]string, error) {
	children, err := os.ReadDir(bedDir)
	if err != nil {
		return nil, err
	}
	var calverDirs, resultFiles []string
	hasRuns := false
	for _, c := range children {
		name := c.Name()
		if name == "NOTES.md" {
			continue // durable memory — never prune
		}
		if c.IsDir() {
			if _, ok := kit.ParseCalVer(name); ok {
				calverDirs = append(calverDirs, name)
			} else if name == "runs" {
				hasRuns = true
			}
		} else if strings.HasPrefix(name, "result-") && strings.HasSuffix(name, ".yml") {
			resultFiles = append(resultFiles, name)
		}
	}

	var removed []string
	// CalVer-named run dirs: keep newest keepN by CalVer.
	removed = append(removed, removeOldestByCalVer(bedDir, calverDirs, keepN, "result-", ".yml", dryRun)...)
	// result-<calver>.yml: keep newest keepN by embedded CalVer.
	removed = append(removed, removeOldestByCalVer(bedDir, resultFiles, keepN, "result-", ".yml", dryRun)...)
	// runs/<id>: keep newest keepN by mtime (runIDs aren't CalVer).
	if hasRuns {
		removed = append(removed, removeOldestByMtime(filepath.Join(bedDir, "runs"), keepN, dryRun)...)
	}
	return removed, nil
}

// removeOldestByCalVer keeps the newest keepN entries (sorted by the CalVer
// embedded in the name, after trimming the given prefix/suffix) and removes the
// rest. Entries without a parseable CalVer are left untouched.
func removeOldestByCalVer(parent string, names []string, keepN int, prefix, suffix string, dryRun bool) []string {
	type dated struct {
		name string
		cv   kit.CalVer
	}
	var items []dated
	for _, n := range names {
		core := strings.TrimSuffix(strings.TrimPrefix(n, prefix), suffix)
		if cv, ok := kit.ParseCalVer(core); ok {
			items = append(items, dated{n, cv})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[j].cv.Less(items[i].cv) }) // newest first
	var removed []string
	for idx, it := range items {
		if idx < keepN {
			continue
		}
		p := filepath.Join(parent, it.name)
		if dryRun {
			removed = append(removed, p)
			continue
		}
		if err := os.RemoveAll(p); err == nil {
			removed = append(removed, p)
		}
	}
	return removed
}

// removeOldestByMtime keeps the newest keepN immediate subdirs of dir (by
// modification time) and removes the rest.
func removeOldestByMtime(dir string, keepN int, dryRun bool) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type timed struct {
		name string
		mod  int64
	}
	var items []timed
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, timed{e.Name(), info.ModTime().UnixNano()})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod > items[j].mod }) // newest first
	var removed []string
	for idx, it := range items {
		if idx < keepN {
			continue
		}
		p := filepath.Join(dir, it.name)
		if dryRun {
			removed = append(removed, p)
			continue
		}
		if err := os.RemoveAll(p); err == nil {
			removed = append(removed, p)
		}
	}
	return removed
}

// pruneBuildCandyDirs trims .build/_candy/<candy>.<version>/ to the newest keepN
// versions PER CANDY — the build-staging counterpart to image-tag retention, so
// outdated candy CalVer stagings don't accumulate (candy names are dot-free, so
// the version parses off the first dot). It also removes the LEGACY shared
// .build/_layers/ dir (fully superseded by the versioned _candy layout) — that
// cleanup is unconditional, like the makepkg sweep. keepN<=0 disables only the
// per-candy retention.
func pruneBuildCandyDirs(buildDir string, keepN int, dryRun bool) []string {
	var removed []string

	// Legacy: the pre-versioned shared staging dir is superseded; remove it.
	legacy := filepath.Join(buildDir, "_layers")
	if _, err := os.Stat(legacy); err == nil {
		if dryRun {
			removed = append(removed, legacy)
		} else if os.RemoveAll(legacy) == nil {
			removed = append(removed, legacy)
		}
	}

	if keepN <= 0 {
		return removed
	}
	candyRoot := filepath.Join(buildDir, "_candy")
	entries, err := os.ReadDir(candyRoot)
	if err != nil {
		return removed
	}
	byCandy := map[string][]string{}
	for _, e := range entries {
		// Skip transient .<name>.tmp.* staging dirs (in-flight installs).
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		name, _, ok := strings.Cut(e.Name(), ".")
		if !ok {
			continue
		}
		byCandy[name] = append(byCandy[name], e.Name())
	}
	for name, dirs := range byCandy {
		removed = append(removed, removeOldestByCalVer(candyRoot, dirs, keepN, name+".", "", dryRun)...)
	}
	return removed
}

// --- targeted image-tag invalidation (`charly clean --invalidate`) -------------------------

// matchImageGlob matches a glob against a full image ref OR its last path
// segment (repo:tag), so 'charly-fedora-2*' matches
// 'ghcr.io/opencharly/charly-fedora-2…:tag' without the registry prefix.
func matchImageGlob(glob, ref string) bool {
	last := ref
	if i := strings.LastIndex(last, "/"); i >= 0 {
		last = last[i+1:]
	}
	full, _ := path.Match(glob, ref)
	short, _ := path.Match(glob, last)
	return full || short
}

// invalidateImageTags removes every charly-labeled image tag matching the
// glob (full ref or its last path segment) — targeted cache invalidation
// for stale intermediates, replacing ad-hoc `podman rmi '<glob>'`. The
// retention safety rules apply unchanged: in-use images are skipped and
// `rmi` runs without -f as the backstop.
func invalidateImageTags(engine, glob string, dryRun bool) ([]string, error) {
	groups, err := charlyImageTags(engine)
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, tags := range groups {
		for _, t := range tags {
			if !matchImageGlob(glob, t.Ref) {
				continue
			}
			if t.InUse {
				continue
			}
			if dryRun {
				removed = append(removed, t.Ref)
				continue
			}
			if err := exec.Command(kit.EngineBinary(engine), "rmi", t.Ref).Run(); err != nil {
				continue // in-use backstop — engine refuses, same as retention
			}
			removed = append(removed, t.Ref)
		}
	}
	sort.Strings(removed)
	return removed, nil
}
