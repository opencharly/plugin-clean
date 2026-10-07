package clean

import (
	"testing"

	"github.com/opencharly/sdk/kit"
)

// TestPruneImagesByRetention_MultiTagGroupSurvivesIntact is the discriminating guard for the
// two-ordinal `keep_images` ranking: a group holding ONE image that wears several tags plus
// DISTINCT siblings behind it is the only shape on which the two-ordinal ranking and the
// pre-fix single tag-row ordinal choose differently. Every other shape — N distinct images
// each wearing one tag — ranks identically under both, so no assertion over it can fail on
// the pre-fix code.
//
// PROVENANCE. The table below is not invented: it is the `fedora-nonfree` group as OBSERVED on
// a live host on 2026-08-16 at 04:4xZ, post-prune (that store had been pruned at 06:18:45 local,
// so it is a contaminated baseline and is recorded as such). The shape is ordinary rather than
// exotic — eight groups on that host carried an id wearing 2-3 tags, because a content-stable
// box rebuilt with a fresh `--tag` shares its image id and ACCUMULATES the tag (confirmed at
// run time: a `--tag rt-x2` rebuild of unchanged content re-reported the pre-existing CalVer tag
// alongside the new one). The literal is frozen deliberately: reading the live store here would
// trade determinism back for host state, and the group WILL drift.
//
// Ordering note: all rows carry an identical ai.opencharly.version label, which is NO LONGER an
// ordering key at all (opencharly/plugin-clean#10 — the label is a cutover-boundary artifact and
// ordering by it inverted recency). CREATION TIME is the primary key, so the Created values, not
// the tag strings, establish the ranks. The tags are bed-shaped (`check-<bed>-<calver>`), which
// ExtractCalVerTag reports as EMPTY, so tag CalVer orders nothing here either.
//
// The exemption note is load-bearing for the canary below: with the label gone, a row whose ONLY
// tag is bed-shaped is UNDATABLE and therefore exempt from removal (retentionRemovable guards on
// `!OkTag` alone). That is why this fixture now carries one extra, plain-CalVer row as the
// vacuity canary, and why the observed rank-tail sibling is asserted to be EXEMPT rather than
// removed. Widening the datability of a bed tag belongs to the `spec` owner of
// container.ExtractCalVerTag, not here.
//
// Pairs with TestPruneImagesByRetention_SharedID, which proves the shared-id ranking; this one
// proves a multi-tag image survives INTACT while the distinct sibling past the budget does not.
// Neither subsumes the other.
func TestPruneImagesByRetention_MultiTagGroupSurvivesIntact(t *testing.T) {
	origList, origCtr, origFloor := kit.ListLocalImages, listContainerImageRefs, liveBuildFloor
	defer func() { kit.ListLocalImages, listContainerImageRefs, liveBuildFloor = origList, origCtr, origFloor }()

	// No live build: the host-global build-activity lock is the seam this stub exists for. With a
	// live build the `lastTag` guard protects every single-tagged image and the expected pruning
	// silently does not happen — which is exactly why a LIVE bed cannot make this assertion.
	liveBuildFloor = func() (kit.CalVer, bool, int) { return kit.CalVer{}, false, 0 }
	listContainerImageRefs = func(string) (map[string]bool, map[string]bool, error) {
		return map[string]bool{}, map[string]bool{}, nil
	}

	const (
		multiTagA = "ghcr.io/opencharly/fedora-nonfree:check-docs-2026.227.2301"
		multiTagB = "ghcr.io/opencharly/fedora-nonfree:check-marketplace-2026.228.0010"
		multiTagC = "ghcr.io/opencharly/fedora-nonfree:check-sidecar-pod-2026.228.0221"
		rankTail  = "ghcr.io/opencharly/fedora-nonfree:check-docs-2026.227.1835"
		// plainCanary carries a PLAIN CalVer tag — the shape retention can still date, and the
		// only row in this fixture whose selection proves the sweep is running at all.
		plainCanary = "ghcr.io/opencharly/fedora-nonfree:2026.227.0900"
	)
	lbl := func() map[string]string {
		return map[string]string{
			"ai.opencharly.box":     "fedora-nonfree",
			"ai.opencharly.version": "2026.227.0830", // identical across all four, as observed
		}
	}
	group := []kit.LocalImageInfo{
		{ID: "57a3efe70a68", Created: 1786850392, Labels: lbl(), Names: []string{
			"ghcr.io/opencharly/fedora-nonfree:check-pod-2026.228.0319"}},
		{ID: "d39f559add13", Created: 1786850024, Labels: lbl(), Names: []string{
			"ghcr.io/opencharly/fedora-nonfree:check-pod-overlay-2026.228.0312"}},
		// The multi-tag image: rank 2 of 4, inside a keep_images: 3 budget, wearing three tags
		// whose own ordinals (0,1,2) are all inside the budget too. BOTH ordinals inside => all
		// three survive. The pre-fix ranking counted TAG ROWS, so these sat at rows 2,3,4 and the
		// last two fell outside the budget.
		{ID: "e91486ab32f7", Created: 1786825413, Labels: lbl(), Names: []string{
			multiTagA, multiTagB, multiTagC}},
		// The distinct sibling past the budget: rank 3. Its only tag is bed-shaped, which
		// ExtractCalVerTag reads as NO CalVer, so with the dead label gone it is EXEMPT — asserted
		// below, never silently dropped from the fixture.
		{ID: "8241fa641a54", Created: 1786818957, Labels: lbl(), Names: []string{rankTail}},
		// The vacuity canary: the OLDEST distinct image, wearing a PLAIN CalVer tag. Rank 4 (past
		// keep_images=3) AND datable AND unreferenced, so it must be selected — the one row here
		// that fails if the fixture ever stops pruning at all. Added by this cutover because the
		// observed rank-tail row above is now exempt; the four rows above stay frozen as observed.
		{ID: "5c0a1b2d3e4f", Created: 1786818000, Labels: lbl(), Names: []string{
			"ghcr.io/opencharly/fedora-nonfree:2026.227.0900"}},
	}
	kit.ListLocalImages = func(string) ([]kit.LocalImageInfo, error) { return group, nil }

	removed, err := pruneImagesByRetention("podman", 3, true /* dryRun: selection only, no rmi */)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}

	got := map[string]bool{}
	for _, r := range removed {
		got[r] = true
	}

	// The DISCRIMINATING half: the multi-tag image keeps every tag. Pre-fix this fails — the
	// tag-row ranking selects multiTagB and multiTagC.
	for _, keep := range []string{multiTagA, multiTagB, multiTagC} {
		if got[keep] {
			t.Errorf("multi-tag image lost a tag it must keep: %s\n"+
				"  all three tags of e91486ab32f7 are inside BOTH ordinals at keep_images=3;\n"+
				"  selecting one is the pre-fix tag-row ranking", keep)
		}
	}

	// The bed-tagged sibling is EXEMPT — the post-cutover truth, pinned rather than left implicit.
	// Its only tag carries no CalVer a parser can read, and the key that used to date it
	// (ai.opencharly.version) is never emitted, so retention refuses to remove it: the fail-safe
	// direction, since removing a row we cannot date is how an artifact disappears unremarked.
	if got[rankTail] {
		t.Errorf("the bed-tagged sibling past the budget was selected: %s\n"+
			"  its only tag is bed-shaped, so with the dead label gone it is undatable and must be "+
			"exempt (retentionRemovable guards on !OkTag alone)", rankTail)
	}

	// The NON-discriminating half, kept as the vacuity check: it fails if the fixture ever stops
	// pruning at all (a guard engaging, keepN mis-resolved, the group mis-keyed), which is what
	// would otherwise let the assertions above pass for the wrong reason.
	if !got[plainCanary] {
		t.Errorf("the distinct sibling past the budget was NOT selected: %s\n"+
			"  it is rank 4 at keep_images=3 (the oldest of the five rows), datable by its plain\n"+
			"  CalVer tag, and unreferenced; if it stops being selected — while the exempt\n"+
			"  bed-tagged sibling above stays unselected — the assertions above pass vacuously",
			plainCanary)
	}

	if len(removed) != 1 {
		t.Errorf("expected exactly 1 selected ref, got %d: %v", len(removed), removed)
	}
}

// TestRetentionUndatableGuardUsesTheOneLiveKey pins the exemption's exact width after the
// opencharly/plugin-clean#10 cutover. The guard used to be `!OkLabel && !OkTag` — an AND over the
// ai.opencharly.version label and the build tag — and this test used to pin the consequence that
// a datable LABEL defeated the exemption. The label is never emitted any more, so that AND had a
// constant for a term, and keeping it would have kept alive the very key whose ordering inversion
// is that issue. The guard is now `!OkTag`: the build tag is the ONE datable key left, so a row
// whose tag carries no `:YYYY.DDD.HHMM` is protected however many tags its image wears.
//
// The label is therefore IRRELEVANT here: the two cases below differ only in it, and BOTH must
// protect their surplus undatable tags. Re-wiring the guard to read the label again makes the
// labelled case reclaim rows whose tags cannot date them — the fail-safe direction inverted, and
// on a store spanning the cutover, the inversion itself.
func TestRetentionUndatableGuardUsesTheOneLiveKey(t *testing.T) {
	origList, origCtr, origFloor := kit.ListLocalImages, listContainerImageRefs, liveBuildFloor
	defer func() { kit.ListLocalImages, listContainerImageRefs, liveBuildFloor = origList, origCtr, origFloor }()
	liveBuildFloor = func() (kit.CalVer, bool, int) { return kit.CalVer{}, false, 0 }
	listContainerImageRefs = func(string) (map[string]bool, map[string]bool, error) {
		return map[string]bool{}, map[string]bool{}, nil
	}

	// One image, five undatable `:latest`-style tags, so tag ordinals 3 and 4 fall outside
	// keep_images=3. The ONLY difference between the two cases is the version label.
	names := []string{"ghcr/x:latest", "ghcr/x:dev", "ghcr/x:stable", "ghcr/x:edge", "ghcr/x:main"}
	run := func(labels map[string]string) []string {
		rows := make([]kit.LocalImageInfo, len(names))
		for i := range names {
			rows[i] = kit.LocalImageInfo{ID: "aaa", Created: 100, Labels: labels, Names: names}
		}
		kit.ListLocalImages = func(string) ([]kit.LocalImageInfo, error) { return rows, nil }
		removed, err := pruneImagesByRetention("podman", 3, true)
		if err != nil {
			t.Fatalf("prune: %v", err)
		}
		return removed
	}

	// Datable LABEL present -> IRRELEVANT (never emitted) -> the tag is undatable -> exempt.
	labelled := run(map[string]string{"ai.opencharly.box": "x", "ai.opencharly.version": "2026.100.0000"})
	if len(labelled) != 0 {
		t.Errorf("a dead ai.opencharly.version label must NOT date a row: the exemption reads the "+
			"ONE remaining datable key (the build tag), so nothing here is removable — got %d "+
			"removals: %v", len(labelled), labelled)
	}

	// No label -> identical outcome, since the label plays no part.
	unlabelled := run(map[string]string{"ai.opencharly.box": "x"})
	if len(unlabelled) != 0 {
		t.Errorf("with an undatable tag the exemption must protect every row however many tags "+
			"the image wears — got %d removals: %v", len(unlabelled), unlabelled)
	}

	// The other half of the guard: a DATABLE tag past the budget IS reclaimable, so the
	// exemption above cannot pass by disabling the tag ordinal wholesale.
	datable := []kit.LocalImageInfo{{ID: "bbb", Created: 200, Labels: map[string]string{"ai.opencharly.box": "y"},
		Names: []string{"ghcr/y:2026.001.0300", "ghcr/y:2026.001.0200", "ghcr/y:2026.001.0100"}}}
	kit.ListLocalImages = func(string) ([]kit.LocalImageInfo, error) { return datable, nil }
	got, err := pruneImagesByRetention("podman", 1, true)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(got) == 0 {
		t.Errorf("datable surplus tags past keep_images=1 must still be reclaimable — got none")
	}
}
