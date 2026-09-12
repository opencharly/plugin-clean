package clean

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/opencharly/sdk/kit"
)

// withBuildActivityDir points the build-activity locks at a temp dir via
// XDG_CACHE_HOME (kit.BuildActivityDir derives from os.UserCacheDir) — the SAME
// dir charly core's acquireBuildActivityLock writes to (see
// charly/filelock_test.go's TestAcquireBuildActivityLock_WritesContentAndReleases
// for the write-side coverage); this file covers the READ side (defaultLiveBuildFloor)
// directly via kit.AcquireFileLock, without needing the core writer.
func withBuildActivityDir(t *testing.T) string {
	t.Helper()
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	dir, err := kit.BuildActivityDir()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// acquireTestActivityLock mimics charly core's acquireBuildActivityLock (a flocked
// nonce file under the build-activity dir whose content is the build's generate
// CalVer) using only kit primitives, so this test needs no core dependency.
func acquireTestActivityLock(t *testing.T, dir, name, calver string) func() {
	t.Helper()
	path := filepath.Join(dir, name)
	rel, err := kit.AcquireFileLock(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(calver+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := rel(); err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(path)
	}
}

// TestLiveBuildFloor_Lifecycle proves the read side of the whole floor mechanism: a
// held activity lock is LIVE and floors retention at its recorded CalVer; release
// removes it; a stale (unheld) lock file is reaped by the next floor scan.
func TestLiveBuildFloor_Lifecycle(t *testing.T) {
	dir := withBuildActivityDir(t)

	// No locks → no live builds.
	if _, ok, live := liveBuildFloor(); ok || live != 0 {
		t.Fatalf("empty dir: want no floor/live, got ok=%v live=%d", ok, live)
	}

	rel1 := acquireTestActivityLock(t, dir, "build-1.lock", "2026.188.1900")
	floor, ok, live := liveBuildFloor()
	if !ok || live != 1 {
		t.Fatalf("held lock: want floorOK live=1, got ok=%v live=%d", ok, live)
	}
	if got := floor.String(); got != "2026.188.1900" {
		t.Fatalf("floor: want 2026.188.1900, got %s", got)
	}

	// A second, OLDER build lowers the floor.
	rel2 := acquireTestActivityLock(t, dir, "build-2.lock", "2026.188.1830")
	floor, _, live = liveBuildFloor()
	if live != 2 || floor.String() != "2026.188.1830" {
		t.Fatalf("two live: want floor 2026.188.1830 live=2, got %s live=%d", floor, live)
	}
	rel2()
	rel1()
	if _, ok, live := liveBuildFloor(); ok || live != 0 {
		t.Fatalf("after release: want none, got ok=%v live=%d", ok, live)
	}

	// A stale lock file (no holder) is reaped, never counted live.
	stale := filepath.Join(dir, "build-3.lock")
	if err := os.WriteFile(stale, []byte("2026.188.0001\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok, live := liveBuildFloor(); ok || live != 0 {
		t.Fatalf("stale lock: want reaped, got ok=%v live=%d", ok, live)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale lock file must be removed by the scan")
	}
}

// TestRetentionRemovable pins the pure retention decision, especially the
// build-activity protections that close the retention-untag race.
func TestRetentionRemovable(t *testing.T) {
	cv := func(s string) kit.CalVer {
		v, ok := kit.ParseCalVer(s)
		if !ok {
			t.Fatalf("bad calver %s", s)
		}
		return v
	}
	old := imageTagInfo{Ref: "r:old", ID: "id1", TagCalVer: cv("2026.188.1700"), OkTag: true}
	pinned := imageTagInfo{Ref: "r:pinned", ID: "id1", TagCalVer: cv("2026.188.1839"), OkTag: true}
	floor := cv("2026.188.1830")

	cases := []struct {
		name    string
		c       imageTagInfo
		idx     int
		live    int
		floorOK bool
		lastTag bool
		want    bool
	}{
		{"kept-newest", pinned, 2, 0, false, false, false},         // idx < keepN
		{"quiet-prune-removes-old", old, 5, 0, false, false, true}, // no live builds: standing rules only
		{"floor-protects-pin", pinned, 5, 1, true, false, false},   // >= floor while live
		{"floor-allows-older", old, 5, 1, true, false, true},       // < floor: untag ok while live
		{"unknown-floor-protects-all", old, 5, 1, false, false, false},
		{"last-tag-never-deleted-while-live", old, 5, 1, true, true, false},
		{"last-tag-ok-when-quiet", old, 5, 0, false, true, true},
		{"in-use-always-kept", imageTagInfo{Ref: "r", InUse: true, OkTag: true, TagCalVer: cv("2026.100.0001")}, 5, 0, false, false, false},
		{"undatable-always-kept", imageTagInfo{Ref: "r"}, 5, 0, false, false, false},
	}
	for _, tc := range cases {
		if got := retentionRemovable(tc.c, tc.idx, 3, floor, tc.floorOK, tc.live, tc.lastTag); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

// TestPruneDanglingImages_LiveBuildReportsSkip pins the OBSERVABLE half of the live-build guard:
// the sweep still removes NOTHING (the safety property, deliberately unchanged), but it now says
// it DECLINED — naming the in-flight build count — instead of returning the same empty result a
// genuinely empty store produces. listDanglingImages is stubbed to fail the test if it is ever
// called: the guard must still short-circuit BEFORE listing.
func TestPruneDanglingImages_LiveBuildReportsSkip(t *testing.T) {
	origList, origFloor := listDanglingImages, liveBuildFloor
	defer func() { listDanglingImages, liveBuildFloor = origList, origFloor }()
	liveBuildFloor = func() (kit.CalVer, bool, int) {
		return kit.CalVer{Year: 2026, Day: 188, HHMM: 1900}, true, 2
	}
	listDanglingImages = func(string) ([]kit.LocalImageInfo, error) {
		t.Fatal("listDanglingImages must not be called while a build is live")
		return nil, nil
	}

	const wantLine = "SKIPPED — 2 build(s) in flight; images are never removed during a build (re-run when builds are idle)"
	for _, onlyCharly := range []bool{true, false} {
		ids, totalBytes, skip, err := pruneDanglingImages("podman", onlyCharly, true)
		if err != nil {
			t.Fatalf("onlyCharly=%v: prune: %v", onlyCharly, err)
		}
		if ids != nil || totalBytes != 0 {
			t.Errorf("onlyCharly=%v: live-build guard must remove nothing, got (%v, %d)", onlyCharly, ids, totalBytes)
		}
		if skip == nil {
			t.Fatalf("onlyCharly=%v: want a skip signal — an empty result alone is indistinguishable from an empty store", onlyCharly)
		}
		if skip.live != 2 {
			t.Errorf("onlyCharly=%v: skip.live = %d, want 2", onlyCharly, skip.live)
		}
		if got := skip.String(); got != wantLine {
			t.Errorf("onlyCharly=%v: skip line\n got: %s\nwant: %s", onlyCharly, got, wantLine)
		}
	}
}

// TestPruneBuildahStaging_LiveBuildReportsSkip is the same contract for the THIRD guarded sweep
// (the /var/tmp buildah staging reaper): nothing is swept while a build is live, and the caller is
// told why instead of receiving the same empty list a clean host yields.
func TestPruneBuildahStaging_LiveBuildReportsSkip(t *testing.T) {
	origFloor := liveBuildFloor
	defer func() { liveBuildFloor = origFloor }()
	liveBuildFloor = func() (kit.CalVer, bool, int) { return kit.CalVer{}, false, 3 }

	dirs, skip := pruneBuildahStaging(true)
	if dirs != nil {
		t.Errorf("live-build guard must sweep nothing, got %v", dirs)
	}
	if skip == nil || skip.live != 3 {
		t.Fatalf("want a skip naming live=3, got %v", skip)
	}
	const wantLine = "SKIPPED — 3 build(s) in flight; buildah/podman staging of an in-flight build is never swept (re-run when builds are idle)"
	if got := skip.String(); got != wantLine {
		t.Errorf("skip line\n got: %s\nwant: %s", got, wantLine)
	}
}
