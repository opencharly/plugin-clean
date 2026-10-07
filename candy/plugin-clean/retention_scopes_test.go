package clean

import (
	"errors"
	"strings"
	"testing"

	"github.com/opencharly/sdk/kit"
	"github.com/opencharly/spec/spec"
)

// errTestNoEngine stands in for "this host has no working podman/docker" — the condition the
// scopes category must survive and the store-touching categories must surface.
var errTestNoEngine = errors.New("no container engine found (install podman or docker)")

// TestCharlyImageTags_DeadLabelCannotInvertRecency is the opencharly/plugin-clean#10 regression
// guard, and it is the one retention test in this package whose failure mode DESTROYS an artifact
// rather than mis-answering a query.
//
// The fixture is the shape the operator measured live on 2026-10-03: a box group spanning the
// schema-versioning-removal cutover, i.e. an OLDER image that still carries a parseable
// `ai.opencharly.version` label (202 of 238 local images did) next to a NEWER image built after
// the cutover that carries none. The pre-fix comparator's second rule was "a labelled row sorts
// before an unlabelled one, whatever the build times are", so it ranked the OLDER image first and
// the NEWEST image last — making the newest image the removal candidate under keep_images while
// the oldest survived. That is silent data loss: the newest build is the one a user is running.
//
// Two assertions, and the second is the operator-visible consequence, so neither can pass
// vacuously: the order, and what keep_images: 1 actually keeps.
func TestCharlyImageTags_DeadLabelCannotInvertRecency(t *testing.T) {
	origList, origCtr, origFloor := kit.ListLocalImages, listContainerImageRefs, liveBuildFloor
	defer func() { kit.ListLocalImages, listContainerImageRefs, liveBuildFloor = origList, origCtr, origFloor }()
	liveBuildFloor = func() (kit.CalVer, bool, int) { return kit.CalVer{}, false, 0 }
	listContainerImageRefs = func(string) (map[string]bool, map[string]bool, error) {
		return map[string]bool{}, map[string]bool{}, nil
	}

	const (
		oldRef = "ghcr.io/opencharly/x:2026.270.1000" // pre-cutover: label present
		newRef = "ghcr.io/opencharly/x:2026.279.1000" // post-cutover: label never emitted
	)
	kit.ListLocalImages = func(string) ([]kit.LocalImageInfo, error) {
		return []kit.LocalImageInfo{
			imgAt("old0000000000000000000000000000000000000000000000000000000000000001", oldRef, "x", "2026.200.0000", 1786000000),
			imgAt("new0000000000000000000000000000000000000000000000000000000000000002", newRef, "x", "", 1786200000),
		}, nil
	}

	groups, err := charlyImageTags("podman")
	if err != nil {
		t.Fatalf("charlyImageTags: %v", err)
	}
	got := groups["x"]
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(got), got)
	}
	if got[0].Ref != newRef {
		t.Fatalf("the group's FIRST row is %q, want the NEWEST build %q.\n"+
			"A labelled-but-older row must never outrank a newer unlabelled one: with the dead\n"+
			"ai.opencharly.version label as an ordering key the comparator inverts recency, so\n"+
			"keep_images would delete the newest image and keep the oldest (plugin-clean#10).",
			got[0].Ref, newRef)
	}

	// The consequence: keep_images: 1 keeps the newest build, and reclaims the old tag.
	removed, err := pruneImagesByRetention("podman", 1, true)
	if err != nil {
		t.Fatalf("pruneImagesByRetention: %v", err)
	}
	for _, r := range removed {
		if r == newRef {
			t.Fatalf("keep_images: 1 selected the NEWEST image %q for removal (removed: %v) — "+
				"the inversion is back", newRef, removed)
		}
	}
	if !containsStr(removed, oldRef) {
		t.Errorf("keep_images: 1 removed %v, want it to reclaim the surplus OLD tag %q", removed, oldRef)
	}
}

func containsStr(in []string, want string) bool {
	for _, s := range in {
		if s == want {
			return true
		}
	}
	return false
}

// TestBuildScopeNameRe pins the family the `scopes` category may touch, against the unit name a
// real build produced (RDD spike, 2026-10-07: a rootless `podman build --cgroup-manager=systemd`
// of a two-line Containerfile emitted `runc-buildah-buildah2754860408.scope`, description
// "libcontainer container buildah-buildah2754860408"), and against every scope that ISN'T
// charly's — including the harness's own and podman's pause scope. Over-matching here would let
// `charly clean` stop a scope whose owner still depends on it.
func TestBuildScopeNameRe(t *testing.T) {
	in := []string{
		"runc-buildah-buildah2754860408.scope", // MEASURED: a real build, this host, 2026-10-07
		"runc-buildah-buildah986455857.scope",  // the SEVEN-DAY hung scope named in plugin-clean#11
		"crun-buildah-buildah1234.scope",       // the other runtime token
		"crun-buildah-buildahSPIKE111.scope",   // the spike's own orphan
	}
	out := []string{
		"init.scope",
		"runc-charly-checkbox-2615952-1.scope",     // a bed container's scope: a live container owns it
		"podman-pause-8c1e66a7.scope",              // podman's pause scope: a live container depends on it
		"dsh-subprocess-836585-d3b07601ba92.scope", // the agent harness's own scope
		"app-Hyprland-omarchy-hyprland.scope",
		"systemd-run-x.scope",
	}
	for _, u := range in {
		if !buildScopeNameRe.MatchString(strings.TrimSpace(u)) {
			t.Errorf("buildScopeNameRe does not match %q — a real build scope would never be reaped", u)
		}
	}
	for _, u := range out {
		if buildScopeNameRe.MatchString(strings.TrimSpace(u)) {
			t.Errorf("buildScopeNameRe matches %q — another owner's scope must never be reaped", u)
		}
	}
}

// TestSelectReapableBuildScopes pins the reaping DECISION, which is the whole safety surface of
// the `scopes` category: a scope is reapable only when it is in the family AND has been active
// long enough AND no builder is alive. Perturbing any one of the three (dropping the age bound,
// dropping the family match, or ignoring a live builder) turns exactly one of these cases red.
func TestSelectReapableBuildScopes(t *testing.T) {
	const maxAge = 3600 // one hour, the production default
	in := []buildScope{
		{Unit: "runc-buildah-buildah1.scope", AgeSeconds: 7 * 24 * 3600}, // hung for a week — the #11 case
		{Unit: "runc-buildah-buildah2.scope", AgeSeconds: 3599},          // one second too young
		{Unit: "runc-buildah-buildah3.scope", AgeSeconds: 0},             // unreadable stamp — refuse, fail closed
		{Unit: "dsh-subprocess-836585-x.scope", AgeSeconds: 30 * 24 * 3600},
		{Unit: "podman-pause-8c1e66a7.scope", AgeSeconds: 30 * 24 * 3600},
	}

	got := selectReapableBuildScopes(in, maxAge, false)
	if len(got) != 1 || got[0].Unit != "runc-buildah-buildah1.scope" {
		t.Fatalf("selectReapableBuildScopes = %+v, want exactly the week-old build scope", got)
	}

	if live := selectReapableBuildScopes(in, maxAge, true); len(live) != 0 {
		t.Fatalf("a live builder parent must block EVERY scope, got %+v", live)
	}

	// The age bound is what separates "hung" from "a build that is simply running".
	if young := selectReapableBuildScopes([]buildScope{{Unit: "runc-buildah-buildah9.scope", AgeSeconds: 60}}, maxAge, false); len(young) != 0 {
		t.Fatalf("a 60-second-old build scope must not be reaped, got %+v", young)
	}
}

// TestReapBuildScopes covers the sweep's three outcomes end to end with every seam stubbed, so it
// needs no systemd session, no /proc scan, and no real scope. Dry-run must REPORT without stopping;
// a live run must stop through the one reap primitive; and each guard's decline must surface as a
// retentionSkip so the CLI can print WHY the category is empty rather than "0 scopes".
func TestReapBuildScopes(t *testing.T) {
	origScopes, origLive, origStop, origReady := listBuildScopes, liveBuilderParents, stopBuildScope, userSystemdBusReady
	origFloor := liveBuildFloor
	defer func() {
		listBuildScopes, liveBuilderParents, stopBuildScope, userSystemdBusReady = origScopes, origLive, origStop, origReady
		liveBuildFloor = origFloor
	}()
	userSystemdBusReady = func() bool { return true }
	liveBuilderParents = func() (bool, error) { return false, nil }

	var stopped []string
	stopBuildScope = func(unit string) error { stopped = append(stopped, unit); return nil }
	listBuildScopes = func() ([]buildScope, error) {
		return []buildScope{
			{Unit: "runc-buildah-buildahHUNG.scope", AgeSeconds: 7 * 24 * 3600},
			{Unit: "runc-buildah-buildahFRESH.scope", AgeSeconds: 5},
		}, nil
	}

	t.Run("dry run reports without stopping", func(t *testing.T) {
		liveBuildFloor = func() (kit.CalVer, bool, int) { return kit.CalVer{}, false, 0 }
		stopped = nil
		got, skip, err := reapBuildScopes(true)
		if err != nil {
			t.Fatalf("reapBuildScopes: %v", err)
		}
		if skip != nil {
			t.Fatalf("a healthy host reported a skip: %v", skip)
		}
		if len(got) != 1 || got[0] != "runc-buildah-buildahHUNG.scope" {
			t.Fatalf("dry-run reaped %v, want only the hung scope", got)
		}
		if len(stopped) != 0 {
			t.Fatalf("dry run STOPPED %v — it must touch nothing", stopped)
		}
	})

	t.Run("live run stops the hung scope only", func(t *testing.T) {
		liveBuildFloor = func() (kit.CalVer, bool, int) { return kit.CalVer{}, false, 0 }
		stopped = nil
		got, _, err := reapBuildScopes(false)
		if err != nil {
			t.Fatalf("reapBuildScopes: %v", err)
		}
		if len(got) != 1 || len(stopped) != 1 || stopped[0] != "runc-buildah-buildahHUNG.scope" {
			t.Fatalf("reaped %v via %v, want exactly the hung scope", got, stopped)
		}
	})

	t.Run("a live charly build declines the whole category", func(t *testing.T) {
		liveBuildFloor = func() (kit.CalVer, bool, int) { return kit.CalVer{}, true, 2 }
		stopped = nil
		got, skip, err := reapBuildScopes(false)
		if err != nil {
			t.Fatalf("reapBuildScopes: %v", err)
		}
		if len(got) != 0 || len(stopped) != 0 {
			t.Fatalf("a build in flight was not protected: reaped %v via %v", got, stopped)
		}
		if skip == nil || !strings.Contains(skip.String(), "in flight") {
			t.Fatalf("the decline must be REPORTED, got %v", skip)
		}
	})

	t.Run("no user systemd session declines with its own reason", func(t *testing.T) {
		liveBuildFloor = func() (kit.CalVer, bool, int) { return kit.CalVer{}, false, 0 }
		userSystemdBusReady = func() bool { return false }
		got, skip, err := reapBuildScopes(false)
		if err != nil {
			t.Fatalf("reapBuildScopes: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("reaped %v without a systemd session", got)
		}
		if skip == nil || skip.String() != "SKIPPED — "+skipReasonScopesNoSystemd {
			t.Fatalf("no-systemd decline = %v, want the no-session reason with no build count", skip)
		}
	})
}

// TestPrintRetentionResult_ScopesCategory pins the operator-facing line for the new category: a
// count naming the guard's own age bound, the reaped unit names beneath it, and — when a guard
// declined — the SKIP line in place of the count, never both.
func TestPrintRetentionResult_ScopesCategory(t *testing.T) {
	t.Run("count and units", func(t *testing.T) {
		out := retentionOutcome{Scopes: []string{"runc-buildah-buildahHUNG.scope"}}
		var sb strings.Builder
		if err := printRetentionResult(&sb, "would remove", false, false, false, false, true, out); err != nil {
			t.Fatalf("print: %v", err)
		}
		want := "scopes: would remove 1 hung transient build scope(s) (active > 1h0m0s, no live builder)\n" +
			"  runc-buildah-buildahHUNG.scope\n"
		if got := sb.String(); got != want {
			t.Errorf("scopes output\n got: %q\nwant: %q", got, want)
		}
	})

	t.Run("a guard's decline replaces the count", func(t *testing.T) {
		out := retentionOutcome{Scopes: []string{"runc-buildah-buildahHUNG.scope"}, ScopeSkip: &retentionSkip{reason: skipReasonScopesNoSystemd}}
		var sb strings.Builder
		if err := printRetentionResult(&sb, "removed", false, false, false, false, true, out); err != nil {
			t.Fatalf("print: %v", err)
		}
		got := sb.String()
		if got != "scopes: SKIPPED — "+skipReasonScopesNoSystemd+"\n" {
			t.Errorf("skip output = %q", got)
		}
		if strings.Contains(got, "runc-buildah-buildahHUNG.scope") {
			t.Errorf("a declined sweep printed units it never touched:\n%s", got)
		}
	})
}

// TestReapBuildScopes_DecisionIsUnreachableFromTheWireRequest states the boundary the `scopes`
// category must respect: `runRetentionOutcome(req, false)` — the form every PEER caller and the
// verb:retention wire entry reach — must never stop a host scope, however the request is filled.
// A peer's post-build/post-run prune reaping the systemd scopes of the host it happens to run on
// would be a capability leak with a host-level blast radius.
func TestReapBuildScopes_DecisionIsUnreachableFromTheWireRequest(t *testing.T) {
	origFloor, origReady := liveBuildFloor, userSystemdBusReady
	origScopes, origLive, origStop := listBuildScopes, liveBuilderParents, stopBuildScope
	defer func() {
		liveBuildFloor, userSystemdBusReady = origFloor, origReady
		listBuildScopes, liveBuilderParents, stopBuildScope = origScopes, origLive, origStop
	}()
	liveBuildFloor = func() (kit.CalVer, bool, int) { return kit.CalVer{}, false, 0 }
	userSystemdBusReady = func() bool { return true }
	liveBuilderParents = func() (bool, error) { return false, nil }
	var stopped []string
	stopBuildScope = func(unit string) error { stopped = append(stopped, unit); return nil }
	listBuildScopes = func() ([]buildScope, error) {
		return []buildScope{{Unit: "runc-buildah-buildahHUNG.scope", AgeSeconds: 7 * 24 * 3600}}, nil
	}

	out := runRetentionOutcome(spec.RetentionRequest{Cache: true}, false)
	if len(out.Scopes) != 0 || len(stopped) != 0 {
		t.Fatalf("the wire form stopped a scope: scopes=%v stopped=%v", out.Scopes, stopped)
	}
}

// TestScopesOnlyRunNeedsNoEngine pins the claim the review asked this cutover to make true rather
// than assert: the scope reaper needs no container engine. `runRetentionOutcome` used to resolve one
// unconditionally, so `charly clean --scopes` — the category that matters MOST on a host whose
// engine is broken, since a hung build scope is the residue of a build that already failed — died
// with an engine-resolution error before it could reap anything. The negative control below (an
// images-only run must still fail) is what stops this passing by disabling the resolution entirely.
func TestScopesOnlyRunNeedsNoEngine(t *testing.T) {
	origEngine, origScopes, origLive, origStop, origReady := resolveEngine, listBuildScopes, liveBuilderParents, stopBuildScope, userSystemdBusReady
	origFloor := liveBuildFloor
	defer func() {
		resolveEngine, listBuildScopes, liveBuilderParents, stopBuildScope, userSystemdBusReady = origEngine, origScopes, origLive, origStop, origReady
		liveBuildFloor = origFloor
	}()

	resolved := 0
	resolveEngine = func() (string, error) {
		resolved++
		return "", errTestNoEngine
	}
	liveBuildFloor = func() (kit.CalVer, bool, int) { return kit.CalVer{}, false, 0 }
	userSystemdBusReady = func() bool { return true }
	liveBuilderParents = func() (bool, error) { return false, nil }
	var stopped []string
	stopBuildScope = func(unit string) error { stopped = append(stopped, unit); return nil }
	listBuildScopes = func() ([]buildScope, error) {
		return []buildScope{{Unit: "runc-buildah-buildahHUNG.scope", AgeSeconds: 7 * 24 * 3600}}, nil
	}

	// The scopes-only run must reap WITHOUT ever resolving an engine.
	out := runRetentionOutcome(spec.RetentionRequest{DryRun: true}, true)
	if out.Reply.Error != "" {
		t.Fatalf("a scopes-only run failed on engine resolution: %s", out.Reply.Error)
	}
	if resolved != 0 {
		t.Fatalf("the scopes-only run resolved the engine %d time(s); it must not need one", resolved)
	}
	if len(out.Scopes) != 1 {
		t.Fatalf("the scopes-only run reaped %v, want the hung scope", out.Scopes)
	}

	// The negative control: an images-only run DOES need it, and a host without it must fail loudly
	// rather than silently prune nothing.
	img := runRetentionOutcome(spec.RetentionRequest{Images: true, DryRun: true}, false)
	if img.Reply.Error == "" || resolved != 1 {
		t.Fatalf("an images-only run must resolve the engine and fail on its absence: err=%q resolved=%d",
			img.Reply.Error, resolved)
	}
}
