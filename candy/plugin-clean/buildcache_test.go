package clean

// buildcache_test.go — the `build-cache` category's proof, against REAL trees and a REAL flock.
//
// Nothing here is mocked. The guard is not a stub: the test takes the daemon's own one-instance
// lock with the production primitive (kit.AcquireFileLock), which is the same syscall a running
// buildkitd takes, so "a daemon owns this root" is proven the way the kernel proves it. The trees
// are real directories with real modes, so the ownership rules (WRITE on the directory being
// emptied, WRITE on the PARENT of the directory being removed, no chmod of a directory another uid
// owns) are exercised rather than asserted.
//
// Every test injects its own cache root through the buildCacheLocate seam — D-STORE-8: tests use
// injected roots and write nothing outside their temporary directory. No test may read the host's
// real buildkitd root, and none may depend on whether a daemon happens to be running there.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencharly/sdk/buildkitdrive"
	"github.com/opencharly/sdk/kit"
	"github.com/opencharly/spec/spec"
)

// withBuildCacheRoot points the category at a fresh temporary cache root for one test and returns
// its layout. The root exists and is empty; the lock file does NOT exist, which is the state of a
// root no daemon has ever opened.
func withBuildCacheRoot(t *testing.T) buildCacheLayout {
	t.Helper()
	orig := buildCacheLocate
	t.Cleanup(func() { buildCacheLocate = orig })
	root := filepath.Join(t.TempDir(), "cache", buildkitdrive.DirName)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("create the injected state root: %v", err)
	}
	l := buildCacheLayout{
		Root:   root,
		Lock:   filepath.Join(root, buildkitdrive.LockName),
		Socket: filepath.Join(t.TempDir(), "buildkitd.sock"),
	}
	buildCacheLocate = func() (buildCacheLayout, error) { return l, nil }
	return l
}

// writeFile creates a file of n bytes under dir (creating dir first) and returns its path.
func writeFile(t *testing.T, dir, name string, n int) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// liveOutcome returns the live-root entry of a sweep result.
func liveOutcome(t *testing.T, out []buildCacheRootOutcome) buildCacheRootOutcome {
	t.Helper()
	for _, r := range out {
		if !r.Quarantined {
			return r
		}
	}
	t.Fatalf("the sweep reported no live root: %+v", out)
	return buildCacheRootOutcome{}
}

// TestBuildCache_RefusesWhileADaemonOwnsTheRoot is the guard's proof: with the daemon's own lock
// HELD (by this test, through the production primitive — the same flock a running buildkitd takes),
// the sweep must remove NOTHING and must name the holder. Without the guard this sweep deletes a
// live store out from under a running build.
func TestBuildCache_RefusesWhileADaemonOwnsTheRoot(t *testing.T) {
	l := withBuildCacheRoot(t)
	writeFile(t, filepath.Join(l.Root, "export"), "data", 4096)

	// The test IS the daemon: it holds the one-instance lock for the root's lifetime.
	release, err := kit.AcquireFileLock(l.Lock, false)
	if err != nil {
		t.Fatalf("holding the daemon's lock: %v", err)
	}
	out, serr := sweepBuildCache(false)
	if serr != nil {
		t.Fatalf("sweepBuildCache: %v", serr)
	}
	live := liveOutcome(t, out)
	if live.Skip == nil {
		t.Fatalf("the sweep did not decline while a holder owned the lock: %+v", live)
	}
	reason := live.Skip.String()
	for _, want := range []string{"SKIPPED", l.Lock, "buildkitd"} {
		if !strings.Contains(reason, want) {
			t.Errorf("the refusal does not name the holder (%q missing): %s", want, reason)
		}
	}
	if live.Removed {
		t.Error("the sweep claimed a removal it must not have performed")
	}
	if _, statErr := os.Stat(filepath.Join(l.Root, "export", "data")); statErr != nil {
		t.Fatalf("a live store was touched despite the guard: %v", statErr)
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	// The same sweep, once the holder is gone: a lock dies with its holder (D-STORE-7), so the root
	// is now reclaimable without asking anyone.
	out, serr = sweepBuildCache(false)
	if serr != nil {
		t.Fatalf("sweepBuildCache after release: %v", serr)
	}
	live = liveOutcome(t, out)
	if live.Skip != nil || !live.Removed || live.Bytes != 4096 {
		t.Fatalf("a released store was not reclaimed: %+v (skip %v)", live, live.Skip)
	}
	if _, statErr := os.Stat(l.Root); !os.IsNotExist(statErr) {
		t.Fatalf("the state root still exists after a real removal: %v", statErr)
	}
}

// TestBuildCache_TakesTheLockBeforeRemoving pins the race the guard closes: a root whose lock file
// is ABSENT (no daemon has ever opened it) is still seized before removal, so a daemon cannot start
// halfway through — and the seized lock is released, not left held.
func TestBuildCache_TakesTheLockBeforeRemoving(t *testing.T) {
	l := withBuildCacheRoot(t)
	writeFile(t, filepath.Join(l.Root, "cache.db"), "db", 128)
	if _, err := os.Stat(l.Lock); !os.IsNotExist(err) {
		t.Fatalf("the injected root must start with no lock file, got %v", err)
	}

	out, err := sweepBuildCache(false)
	if err != nil {
		t.Fatalf("sweepBuildCache: %v", err)
	}
	if live := liveOutcome(t, out); !live.Removed {
		t.Fatalf("the lockless store was not reclaimed: %+v", live)
	}
	if _, statErr := os.Stat(l.Root); !os.IsNotExist(statErr) {
		t.Fatalf("the state root survived the removal: %v", statErr)
	}
}

// TestBuildCache_DryRunReportsAndTouchesNothing pins the probe: the dry run reports the SAME bytes
// a real run would reclaim, removes nothing, and — the property the acquire path must not break —
// does not bring the daemon's lock file into existence to ask whether a daemon is there.
func TestBuildCache_DryRunReportsAndTouchesNothing(t *testing.T) {
	l := withBuildCacheRoot(t)
	writeFile(t, filepath.Join(l.Root, "export"), "data", 2048)
	dead := filepath.Join(filepath.Dir(l.Root), buildkitdrive.DirName+quarantinedSuffix+"1794801825")
	writeFile(t, dead, "big", 8192)

	out, err := sweepBuildCache(true)
	if err != nil {
		t.Fatalf("sweepBuildCache(dry): %v", err)
	}
	live := liveOutcome(t, out)
	if live.Removed || live.Bytes != 2048 || !live.Present {
		t.Fatalf("dry-run live outcome = %+v, want present/not-removed/2048 bytes", live)
	}
	if _, statErr := os.Stat(filepath.Join(l.Root, "export", "data")); statErr != nil {
		t.Fatalf("the dry run touched the live root: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dead, "big")); statErr != nil {
		t.Fatalf("the dry run touched a quarantined root: %v", statErr)
	}
	if _, statErr := os.Stat(l.Lock); !os.IsNotExist(statErr) {
		t.Fatalf("the dry run CREATED the daemon's lock file — it must touch nothing: %v", statErr)
	}
	var quarantined buildCacheRootOutcome
	for _, r := range out {
		if r.Quarantined {
			quarantined = r
		}
	}
	if !quarantined.Present || quarantined.Bytes != 8192 || quarantined.Removed {
		t.Fatalf("dry-run quarantined outcome = %+v, want present/not-removed/8192 bytes", quarantined)
	}
}

// TestBuildCache_SweepsQuarantinedSiblingsOnly pins the discovery: every `buildkitd.broken-*`
// directory beside the live root is reclaimed unconditionally (a quarantined store is dead by
// construction), and NOTHING else under the cache root is touched — not the daemon's config, log or
// live lock, and not a directory whose name merely resembles one.
func TestBuildCache_SweepsQuarantinedSiblingsOnly(t *testing.T) {
	l := withBuildCacheRoot(t)
	parent := filepath.Dir(l.Root)
	deadOne := filepath.Join(parent, buildkitdrive.DirName+quarantinedSuffix+"1794801825")
	writeFile(t, deadOne, "blob", 1024)
	deadTwo := filepath.Join(parent, buildkitdrive.DirName+quarantinedSuffix+"2")
	writeFile(t, deadTwo, "blob", 512)

	// Everything below must SURVIVE: the daemon's own files, and near-miss names that the prefix
	// test must not swallow (a different tool's directory, and the suffix without its separator).
	survivors := []string{
		filepath.Join(parent, buildkitdrive.DirName+".toml"),
		filepath.Join(parent, buildkitdrive.DirName+".log"),
	}
	if err := os.WriteFile(survivors[0], []byte("worker"), 0o644); err != nil {
		t.Fatalf("write %s: %v", survivors[0], err)
	}
	if err := os.WriteFile(survivors[1], []byte("log"), 0o644); err != nil {
		t.Fatalf("write %s: %v", survivors[1], err)
	}
	nearMiss := filepath.Join(parent, "not-"+buildkitdrive.DirName+quarantinedSuffix+"1")
	writeFile(t, nearMiss, "x", 8)
	survivors = append(survivors, nearMiss)

	out, err := sweepBuildCache(false)
	if err != nil {
		t.Fatalf("sweepBuildCache: %v", err)
	}
	removed := map[string]bool{}
	for _, r := range out {
		if r.Quarantined && r.Removed {
			removed[r.Dir] = true
		}
	}
	for _, dead := range []string{deadOne, deadTwo} {
		if !removed[dead] {
			t.Errorf("quarantined root %s was not reclaimed: %+v", dead, out)
		}
		if _, statErr := os.Stat(dead); !os.IsNotExist(statErr) {
			t.Errorf("quarantined root %s survived: %v", dead, statErr)
		}
	}
	for _, keep := range survivors {
		if _, statErr := os.Stat(keep); statErr != nil {
			t.Errorf("the sweep touched %s, which is not a quarantined root: %v", keep, statErr)
		}
	}
}

// TestBuildCache_RelaxesAReadOnlyDirectoryItOwns pins the difference between a mode this user may
// change and an owner it may not: a NON-EMPTY directory the user owns but cannot write is relaxed
// and removed (a hand `rm -rf` needs the same chmod), where refusing it would leave a reclaimable
// store behind over a permission bit the owner can fix.
func TestBuildCache_RelaxesAReadOnlyDirectoryItOwns(t *testing.T) {
	l := withBuildCacheRoot(t)
	inner := filepath.Join(l.Root, "runc-overlayfs", "snapshots")
	writeFile(t, inner, "blob", 300)
	if err := os.Chmod(inner, 0o500); err != nil {
		t.Fatalf("chmod %s: %v", inner, err)
	}
	t.Cleanup(func() { _ = os.Chmod(inner, 0o755) })

	out, err := sweepBuildCache(false)
	if err != nil {
		t.Fatalf("sweepBuildCache: %v", err)
	}
	live := liveOutcome(t, out)
	if live.Blocker != "" {
		t.Fatalf("a directory this user OWNS was treated as a blocker: %s", live.Blocker)
	}
	if !live.Removed || live.Bytes != 300 {
		t.Fatalf("the read-only-but-owned tree was not reclaimed: %+v", live)
	}
}

// TestBuildCache_AnEmptyReadOnlyDirectoryIsNotABlocker pins the kernel's own rule: rmdir needs
// WRITE on the PARENT, not on the directory itself, so an EMPTY read-only directory never blocks a
// removal. Treating it as a blocker would refuse a tree that is entirely reclaimable — the
// over-refusal defect this rule exists to prevent.
func TestBuildCache_AnEmptyReadOnlyDirectoryIsNotABlocker(t *testing.T) {
	l := withBuildCacheRoot(t)
	empty := filepath.Join(l.Root, "empty-and-read-only")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", empty, err)
	}
	writeFile(t, filepath.Join(l.Root, "history.db"), "h", 64)
	if err := os.Chmod(empty, 0o500); err != nil {
		t.Fatalf("chmod %s: %v", empty, err)
	}
	t.Cleanup(func() { _ = os.Chmod(empty, 0o755) })

	out, err := sweepBuildCache(false)
	if err != nil {
		t.Fatalf("sweepBuildCache: %v", err)
	}
	live := liveOutcome(t, out)
	if live.Blocker != "" || !live.Removed {
		t.Fatalf("an empty read-only directory blocked a reclaimable tree: %+v", live)
	}
}

// TestBuildCache_NamesAnUndeletableTreeAndLeavesItWhole is the honesty half of the category: when a
// tree cannot be removed WHOLE, the sweep must say so by NAME and delete NOTHING — a half-deleted
// store is a corpse nobody can diagnose, and "removed" over 40 GiB of leftovers is a lie about a
// disk. A directory this user cannot enumerate blocks the whole tree.
//
// Skipped under root, where the kernel grants every permission and the situation cannot be built:
// the measured real case is an unprivileged user against a rootless build's store.
func TestBuildCache_NamesAnUndeletableTreeAndLeavesItWhole(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: every directory is enumerable, so an unremovable tree cannot be built")
	}
	l := withBuildCacheRoot(t)
	blocked := filepath.Join(l.Root, "runc-overlayfs", "snapshots", "6920")
	writeFile(t, blocked, "fs", 700)
	writeFile(t, filepath.Join(l.Root, "cache.db"), "db", 100)
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatalf("chmod %s: %v", blocked, err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })

	out, err := sweepBuildCache(false)
	if err != nil {
		t.Fatalf("sweepBuildCache: %v", err)
	}
	live := liveOutcome(t, out)
	if live.Blocker == "" {
		t.Fatalf("an unenumerable directory did not block the removal: %+v", live)
	}
	if !strings.Contains(live.Blocker, blocked) {
		t.Errorf("the blocker does not name the path %s: %s", blocked, live.Blocker)
	}
	if live.Removed {
		t.Error("the sweep claimed a removal it could not have completed")
	}
	// THE point: nothing was half-deleted. The blocking directory is made listable again before
	// the check — a mode-000 directory cannot be inspected even when its contents are untouched, so
	// a failed stat would otherwise read as a deletion.
	if err := os.Chmod(blocked, 0o755); err != nil {
		t.Fatalf("restore %s: %v", blocked, err)
	}
	for _, p := range []string{
		filepath.Join(blocked, "fs"),
		filepath.Join(l.Root, "cache.db"),
	} {
		if _, statErr := os.Stat(p); statErr != nil {
			t.Errorf("a blocked removal deleted %s — the tree must be left whole: %v", p, statErr)
		}
	}
}

// TestBuildCache_UnreachableFromTheWireRequest states the boundary the category must respect: the
// verb:retention entry point every peer plugin (and every core adapter) reaches — runRetention, and
// runRetentionOutcome with the zero plan — must never reclaim a host's builder cache, however the
// wire request is filled. A peer's post-build prune deleting the build cache it is about to use
// would be a capability leak with a host-level blast radius.
func TestBuildCache_UnreachableFromTheWireRequest(t *testing.T) {
	l := withBuildCacheRoot(t)
	dead := filepath.Join(filepath.Dir(l.Root), buildkitdrive.DirName+quarantinedSuffix+"1")
	writeFile(t, dead, "blob", 64)
	writeFile(t, filepath.Join(l.Root, "cache.db"), "db", 64)

	// (1) The wire categories that need no container engine, through the verb:retention entry every
	// peer plugin calls.
	reply := runRetention(spec.RetentionRequest{Dir: t.TempDir(), DryRun: true, Check: true})
	if reply.Error != "" {
		t.Fatalf("the wire run failed: %s", reply.Error)
	}

	// (2) A FULLY populated wire request, with the engine deliberately unavailable so the run stops
	// before it can touch anything at all — and still never reaches the builder's cache.
	origEngine := resolveEngine
	defer func() { resolveEngine = origEngine }()
	resolveEngine = func() (string, error) { return "", errTestNoEngine }
	out := runRetentionOutcome(spec.RetentionRequest{
		Dir: t.TempDir(), DryRun: true, Images: true, Check: true, Deep: true, Cache: true,
	}, cleanPlan{})
	if len(out.BuildCache) != 0 {
		t.Fatalf("the wire-addressable form swept the build cache: %+v", out.BuildCache)
	}
	for _, p := range []string{dead, filepath.Join(l.Root, "cache.db")} {
		if _, statErr := os.Stat(p); statErr != nil {
			t.Fatalf("the wire-addressable form deleted %s: %v", p, statErr)
		}
	}
}

// TestBuildCache_NeedsNoContainerEngine pins the claim the category makes rather than asserts: it
// reclaims a filesystem tree, so a host whose podman is absent or broken — the state a host with a
// large stale store is often in — can still reclaim it. The negative control (an images-only run
// must still fail) is what stops this passing by disabling engine resolution entirely.
func TestBuildCache_NeedsNoContainerEngine(t *testing.T) {
	origEngine := resolveEngine
	defer func() { resolveEngine = origEngine }()
	resolved := 0
	resolveEngine = func() (string, error) {
		resolved++
		return "", errTestNoEngine
	}
	l := withBuildCacheRoot(t)
	writeFile(t, filepath.Join(l.Root, "cache.db"), "db", 32)

	out := runRetentionOutcome(spec.RetentionRequest{DryRun: true}, cleanPlan{buildCache: true})
	if out.Reply.Error != "" {
		t.Fatalf("a build-cache-only run failed on engine resolution: %s", out.Reply.Error)
	}
	if resolved != 0 {
		t.Fatalf("the build-cache-only run resolved the engine %d time(s); it must not need one", resolved)
	}
	if len(out.BuildCache) == 0 {
		t.Fatal("the build-cache-only run reported no root")
	}

	img := runRetentionOutcome(spec.RetentionRequest{Images: true, DryRun: true}, cleanPlan{})
	if img.Reply.Error == "" || resolved != 1 {
		t.Fatalf("an images-only run must resolve the engine and fail on its absence: err=%q resolved=%d",
			img.Reply.Error, resolved)
	}
}

// TestDefaultBuildCacheLocateUsesTheOwnerPackage pins the ONE-implementation rule: the root, the
// lock name and the socket all come from `sdk/buildkitdrive` — the package that owns the daemon's
// `--root` — behind the ONE roots resolver (D-STORE-1), never from a hand-built
// `<cache>/buildkitd`. The comparison is against the owner's own exported API, so this test cannot
// be satisfied by transcribing the same literal twice.
func TestDefaultBuildCacheLocateUsesTheOwnerPackage(t *testing.T) {
	// An injected cache root (D-STORE-8): CHARLY_CACHE_DIR relocates every root through the ONE
	// resolver, so this reads a temporary path and never the host's real cache.
	t.Setenv("CHARLY_CACHE_DIR", filepath.Join(t.TempDir(), "charly-cache"))

	wantRoot, err := buildkitdrive.StateRoot()
	if err != nil {
		t.Fatalf("buildkitdrive.StateRoot: %v", err)
	}
	wantSocket, serr := buildkitdrive.SocketPath()
	if serr != nil {
		t.Fatalf("buildkitdrive.SocketPath: %v", serr)
	}
	got, err := defaultBuildCacheLocate()
	if err != nil {
		t.Fatalf("defaultBuildCacheLocate: %v", err)
	}
	if got.Root != wantRoot {
		t.Errorf("Root = %q, want the daemon owner's own %q", got.Root, wantRoot)
	}
	if got.Lock != filepath.Join(wantRoot, buildkitdrive.LockName) {
		t.Errorf("Lock = %q, want %q", got.Lock, filepath.Join(wantRoot, buildkitdrive.LockName))
	}
	if got.Socket != wantSocket {
		t.Errorf("Socket = %q, want %q", got.Socket, wantSocket)
	}
	// The root must be the cache root's child, i.e. resolved through the contract's resolver and
	// not from an environment variable read here.
	if parent := filepath.Dir(got.Root); parent != filepath.Dir(wantRoot) {
		t.Errorf("Root %q is not beside the cache root %q", got.Root, parent)
	}
}

// TestBuildCacheReport_EveryRootCarriesItsOwnStatus pins the operator surface: one line per root,
// each with its own bytes and its own status, because these trees fail independently. Every state
// is distinct in the output — absent, skipped by the lock guard, blocked by ownership, reclaimed —
// so a reader can never mistake "the guard stopped me" or "I could not delete it" for "nothing was
// there".
func TestBuildCacheReport_EveryRootCarriesItsOwnStatus(t *testing.T) {
	roots := []buildCacheRootOutcome{
		{Dir: "/cache/buildkitd", Present: true, Bytes: 48 << 30, Removed: true},
		{Dir: "/cache/buildkitd.broken-1", Quarantined: true, Present: true, Bytes: 105 << 30, Removed: true},
		{Dir: "/cache/buildkitd.broken-2", Quarantined: true, Present: true, Bytes: 3 << 30,
			Blocker: "export: owned by uid 100975 and not writable by this user"},
		{Dir: "/cache/buildkitd.broken-3", Quarantined: true},
	}
	// The ORDER is part of the contract: the summary precedes the lines it summarizes, so the
	// section reads top-down without a total appearing above the verdict that produced it.
	want := []string{
		"build-cache live: removed /cache/buildkitd (48.0 GiB)\n",
		"build-cache dead: removed 1 quarantined root(s) (105.0 GiB)\n",
		"  removed /cache/buildkitd.broken-1 (105.0 GiB)\n",
		"  cannot be removed — /cache/buildkitd.broken-2 holds 3.0 GiB and export: owned by uid 100975",
		"  absent at /cache/buildkitd.broken-3 (nothing to remove)\n",
	}
	got := buildCacheLines("removed", roots)
	if len(got) != len(want) {
		t.Fatalf("report has %d line(s), want %d:\n%s", len(got), len(want), strings.Join(got, ""))
	}
	for i, w := range want {
		if i == 3 {
			// a long line: assert the parts, since the blocker text is a full sentence
			if !strings.Contains(got[i], w) || !strings.Contains(got[i], "the tree is left whole") {
				t.Errorf("line %d = %q, want it to carry %q", i, got[i], w)
			}
			continue
		}
		if got[i] != w {
			t.Errorf("line %d = %q, want %q", i, got[i], w)
		}
	}

	for _, wantSub := range []string{
		"build-cache live: removed /cache/buildkitd (48.0 GiB)\n",
		"build-cache dead: removed 1 quarantined root(s) (105.0 GiB)\n",
		"  removed /cache/buildkitd.broken-1 (105.0 GiB)\n",
		"  cannot be removed — /cache/buildkitd.broken-2 holds 3.0 GiB and export: owned by uid 100975",
		"the tree is left whole\n",
		"  absent at /cache/buildkitd.broken-3 (nothing to remove)\n",
	} {
		if !strings.Contains(strings.Join(got, ""), wantSub) {
			t.Errorf("report is missing %q:\n%s", wantSub, strings.Join(got, ""))
		}
	}

	// The guarded live root: the SKIP line stands in place of the removal line, and the dead group
	// still reports independently.
	held := []buildCacheRootOutcome{
		{Dir: "/cache/buildkitd", Present: true, Bytes: 48 << 30,
			Skip: &retentionSkip{reason: liveBuildCacheReason(buildCacheLayout{
				Root: "/cache/buildkitd", Lock: "/cache/buildkitd/buildkitd.lock", Socket: "/run/charly/buildkitd.sock",
			})}},
	}
	heldOut := strings.Join(buildCacheLines("removed", held), "")
	if !strings.Contains(heldOut, "build-cache live: SKIPPED — a running buildkitd holds /cache/buildkitd/buildkitd.lock") {
		t.Errorf("the held live root is not reported as skipped, naming the holder:\n%s", heldOut)
	}
	if !strings.Contains(heldOut, "it answers on /run/charly/buildkitd.sock") {
		t.Errorf("the refusal does not name the daemon's socket:\n%s", heldOut)
	}
	if !strings.Contains(heldOut, "build-cache dead: no quarantined root(s) found") {
		t.Errorf("an empty quarantine group is not stated explicitly:\n%s", heldOut)
	}
	if strings.Contains(heldOut, "removed 48.0 GiB") {
		t.Errorf("a skipped live root was reported as reclaimed:\n%s", heldOut)
	}
}
