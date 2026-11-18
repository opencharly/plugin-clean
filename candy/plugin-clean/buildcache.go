package clean

// buildcache.go — the `build-cache` category: the builder's PERSISTENT cache, which is the one
// large artefact a host accumulates during builds that no other category can see.
//
// # What this root is, and what it is NOT
//
// `buildkitdrive` owns the daemon's persistent `--root` at `CacheDir()/buildkitd`
// (`sdk/buildkitdrive` StateRoot). It is NOT one of charly's stores (D-STORE-3: a store tree IS an
// OCI Image Layout and nothing else) — it carries no `oci-layout` marker and it MUST carry a
// `buildkitd.lock`, which a store tree is forbidden to hold. So `storekit.NamedStores()` /
// `OpenStore` / `RemoveBlob` correctly do not see it, and this category must not pretend they do:
// it reclaims a FOREIGN daemon's cache tree and mints no store of its own. That is also why the
// two paths below come from `sdk/buildkitdrive` — the package that owns the daemon — and never
// from a hand-built `<cache>/buildkitd`.
//
// # Why the category exists
//
// Measured on this host (opencharly/plugin-clean#23): the live root held 50 GiB and a quarantined
// sibling held 105 GiB, and NEITHER could be reclaimed by any `charly clean` category — the
// quarantine was removed by hand with `sudo rm -rf` after 301 708 files refused to go. An
// improvised manual path with no owner is the R3 workaround this category deletes: the corpse now
// has a verb, the live root has a guarded one, and a tree this user genuinely cannot delete is
// named instead of half-deleted.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/opencharly/sdk/buildkitdrive"
	"github.com/opencharly/sdk/kit"
)

// quarantinedSuffix is the shape a QUARANTINED build cache takes: the daemon's state-root name
// with this suffix appended, e.g. `buildkitd.broken-1794801825`.
//
// ⚑ Nothing in this tree WRITES it, deliberately: quarantining is a RECOVERY decision (rename a
// corrupt root aside so the next build starts clean), an action for its owner — an operator today,
// a repair surface if one is ever designed. What this category owns is the other end of that
// decision, the corpse's removal, and that is what turns an improvised name into a recognized
// shape: the suffix is the contract between the two ends, and no sweep may invent a quarantine of
// its own.
const quarantinedSuffix = ".broken-"

// buildCacheLayout locates the builder's persistent cache: the daemon's state root, the
// one-instance lock it keeps INSIDE that root, and the socket it answers on.
type buildCacheLayout struct {
	// Root is the daemon's persistent `--root` (its state root).
	Root string
	// Lock is the daemon's own one-instance lock, inside Root. BuildKit enforces it: a second
	// daemon on the same root fails loudly, which is what makes it the HOLDER's liveness signal.
	Lock string
	// Socket is where a running daemon answers. It is NAMED in a refusal and never dialed.
	Socket string
}

// buildCacheLocate is a seam — the same package-var pattern liveBuildFloor / listBuildScopes use —
// so a test injects its OWN temporary root and no test reads or writes the host's real cache
// (D-STORE-8: tests use injected roots and write nothing outside their temporary directory).
var buildCacheLocate = defaultBuildCacheLocate

// defaultBuildCacheLocate resolves the layout through `sdk/buildkitdrive`, the package that OWNS
// the daemon's root: the root through the ONE roots resolver (D-STORE-1), the lock name from the
// daemon's own constant, and the socket through the same resolver. Nothing here spells a path.
func defaultBuildCacheLocate() (buildCacheLayout, error) {
	root, err := buildkitdrive.StateRoot()
	if err != nil {
		return buildCacheLayout{}, err
	}
	sock, serr := buildkitdrive.SocketPath()
	if serr != nil {
		// The socket only ever NAMES the holder in a refusal, so a host that cannot resolve it
		// must not lose the whole category to a diagnostic.
		sock = ""
	}
	return buildCacheLayout{
		Root:   root,
		Lock:   filepath.Join(root, buildkitdrive.LockName),
		Socket: sock,
	}, nil
}

// buildCacheRootOutcome is ONE root this category acted on: the live store, or one quarantined
// sibling. Each root reports INDEPENDENTLY, because these trees fail independently — a live store
// held by a running daemon says nothing about the dead corpse beside it, and a corpse this user
// cannot delete says nothing about the live store.
type buildCacheRootOutcome struct {
	// Dir is the root's absolute path — the name the report says it by.
	Dir string

	// Quarantined marks a `buildkitd.broken-*` sibling (dead by construction) rather than the
	// live state root.
	Quarantined bool

	// Present is false when the root does not exist at all.
	Present bool

	// Bytes is what the root holds: the reclaimed figure under a real run, the would-reclaim
	// figure under --dry-run — the SAME measurement either way, so the probe never lies.
	Bytes int64

	// Removed is true only when the tree was removed WHOLE, and never under --dry-run.
	Removed bool

	// Skip is set when a guard deliberately left the root alone, carrying the reason the operator
	// reads (a running daemon owns it).
	Skip *retentionSkip

	// Blocker is set when the tree cannot be removed whole by this user (container-mapped UIDs
	// from a rootless build, a read-only mount). Nothing was deleted: see removeBuildCacheRoot.
	Blocker string
}

// sweepBuildCache reclaims the builder's persistent cache: the LIVE state root when no daemon owns
// it, and every quarantined sibling unconditionally.
//
// # Why the live root is guarded and the corpses are not
//
// A running daemon holds an exclusive flock on `<root>/buildkitd.lock` for its whole life — that
// lock IS how BuildKit enforces one daemon per root — so it is the holder's own liveness signal,
// and a lock dies with its holder (D-STORE-7). The sweep therefore SEIZES the lock before it
// touches the live root: while it holds it, no daemon can start on that root, so the removal races
// nothing; and when a daemon already holds it, the acquire answers ErrLockBusy and that IS the
// refusal, naming the daemon.
//
// A quarantined sibling is dead BY CONSTRUCTION — a store is only quarantined because it was
// abandoned — so there is nothing left to race and it needs no lock at all.
func sweepBuildCache(dryRun bool) ([]buildCacheRootOutcome, error) {
	layout, err := buildCacheLocate()
	if err != nil {
		return nil, err
	}
	out := []buildCacheRootOutcome{buildCacheLive(layout, dryRun)}
	siblings, serr := quarantinedBuildCaches(layout.Root)
	if serr != nil {
		return nil, serr
	}
	for _, dir := range siblings {
		out = append(out, removeBuildCacheRoot(dir, true, dryRun))
	}
	return out, nil
}

// buildCacheLive is the guarded instrument: the live state root. It removes the root only when no
// daemon owns it, and it names the owner when one does.
func buildCacheLive(l buildCacheLayout, dryRun bool) buildCacheRootOutcome {
	out := buildCacheRootOutcome{Dir: l.Root}
	info, err := os.Stat(l.Root)
	if err != nil || !info.IsDir() {
		return out // absent: nothing owns it and there is nothing to remove
	}
	out.Present = true

	// A daemon that has ever run on this root created the lock at startup, so an ABSENT lock is
	// proof that no daemon owns it. Under --dry-run the probe must create nothing, so it only
	// seizes an existing lock; on a real run it seizes in either case, because creating the lock
	// is what blocks a daemon from starting halfway through the removal.
	lockPresent := false
	if _, lerr := os.Stat(l.Lock); lerr == nil {
		lockPresent = true
	}
	if !dryRun || lockPresent {
		release, aerr := kit.AcquireFileLock(l.Lock, false)
		if errors.Is(aerr, kit.ErrLockBusy) {
			out.Skip = &retentionSkip{reason: liveBuildCacheReason(l)}
			return out
		}
		if aerr != nil {
			out.Blocker = fmt.Sprintf("taking the daemon's one-instance lock %s: %v", l.Lock, aerr)
			return out
		}
		// Held across the whole removal, and released only after it: that is the point of taking
		// it. A release error changes nothing the caller can act on (the lock dies with the
		// process either way), so it is not promoted into a false failure.
		defer func() { _ = release() }()
	}

	out.Bytes, out.Blocker, out.Removed = reclaimBuildCacheTree(l.Root, dryRun)
	return out
}

// removeBuildCacheRoot is the unguarded instrument: one quarantined sibling, dead by construction.
func removeBuildCacheRoot(dir string, quarantined, dryRun bool) buildCacheRootOutcome {
	out := buildCacheRootOutcome{Dir: dir, Quarantined: quarantined}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return out
	}
	out.Present = true
	out.Bytes, out.Blocker, out.Removed = reclaimBuildCacheTree(dir, dryRun)
	return out
}

// reclaimBuildCacheTree measures one root, REFUSES when this user cannot delete it whole, and —
// on a real run only — removes it. It returns the bytes it held, the blocker (empty when there is
// none) and whether it was actually removed.
//
// ⚑ It never unlinks anything before the whole tree is known removable. A half-deleted store is a
// corpse nobody can diagnose, and an operator who is told "removed" while 40 GiB of foreign-owned
// directories remain has been lied to about a disk. So the walk happens FIRST, the refusal is
// reported BY NAME with the path and the uid that owns it, and the tree is left exactly as it was.
func reclaimBuildCacheTree(dir string, dryRun bool) (bytes int64, blocker string, removed bool) {
	bytes, blocker, relax := inspectBuildCacheTree(dir)
	if blocker != "" || dryRun {
		return bytes, blocker, false
	}
	// Directories this user OWNS but cannot write are not a blocker: their owner may relax the
	// mode, which is exactly what a hand removal would have had to do. A directory owned by
	// ANOTHER uid is the genuine blocker — see inspectBuildCacheTree.
	for _, d := range relax {
		if fi, serr := os.Stat(d); serr == nil {
			_ = os.Chmod(d, fi.Mode().Perm()|0o700)
		}
	}
	if rerr := os.RemoveAll(dir); rerr != nil {
		// The preflight said removable and the kernel disagreed (an immutable file, a read-only
		// mount): report it BY NAME and never claim a removal that did not happen.
		return bytes, fmt.Sprintf("removing it: %v", rerr), false
	}
	return bytes, "", true
}

// inspectBuildCacheTree answers three questions in ONE walk, which is why it is one function:
// how many bytes the root holds (the reported figure), the FIRST path that blocks a whole-tree
// removal (reported by name), and the directories this user owns but cannot write (a mode to
// relax before removal, not a blocker).
//
// The removal rules it applies are the kernel's own:
//   - emptying a directory needs WRITE on that directory;
//   - removing a directory (rmdir) needs WRITE on its PARENT, so an EMPTY directory this user
//     cannot write is still removable, and an empty sibling of a blocker is not one;
//   - a directory that cannot be enumerated cannot be emptied either, so a walk error IS a
//     blocker, reported rather than skipped.
func inspectBuildCacheTree(dir string) (bytes int64, blocker string, relax []string) {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if blocker == "" {
				blocker = fmt.Sprintf("%s: cannot be read (%v)", p, err)
			}
			return nil
		}
		if !d.IsDir() {
			if fi, ierr := d.Info(); ierr == nil {
				bytes += fi.Size()
			}
			return nil
		}
		if writableByUser(p) {
			return nil
		}
		entries, rerr := os.ReadDir(p)
		if rerr == nil && len(entries) == 0 {
			return nil // empty: removed through the PARENT's permission, never through its own
		}
		if rerr != nil || !ownedByUser(p) {
			if blocker == "" {
				blocker = describeBlockedDir(p, rerr)
			}
			return nil
		}
		relax = append(relax, p)
		return nil
	})
	if blocker == "" && !writableByUser(filepath.Dir(dir)) {
		blocker = describeBlockedDir(filepath.Dir(dir), nil)
	}
	return bytes, blocker, relax
}

// The access(2) mode bits a removal needs, from POSIX: X_OK is 1 and W_OK is 2. Go's `syscall`
// package does not export them on linux (they live in golang.org/x/sys/unix, which this plugin has
// no other reason to import), and they are the kernel ABI rather than a tunable of this sweep.
const (
	accessXOK = 1
	accessWOK = 2
)

// writableByUser reports whether this process may create and unlink entries in dir — the right a
// whole-tree removal needs on every directory it empties.
func writableByUser(dir string) bool {
	return syscall.Access(dir, accessWOK|accessXOK) == nil
}

// ownedByUser reports whether dir's owner is this process's user, i.e. whether its mode can be
// relaxed before removal. A directory owned by ANOTHER uid — a rootless build leaves its store
// owned by the container's mapped UIDs, the measured case in opencharly/plugin-clean#23 — cannot
// be emptied by this user at all, and no chmod can change that.
func ownedByUser(dir string) bool {
	fi, err := os.Stat(dir)
	if err != nil {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}

// describeBlockedDir names WHY a path blocks a whole-tree removal: the path, the uid that owns it,
// and what that means — because "permission denied" alone leaves a reader unable to tell a
// container-mapped UID from a shared host's other tenant, and the two have different remedies.
func describeBlockedDir(dir string, readErr error) string {
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Sprintf("%s: cannot be inspected (%v)", dir, err)
	}
	uid := -1
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		uid = int(st.Uid)
	}
	if readErr != nil {
		return fmt.Sprintf("%s: owned by uid %d and cannot be enumerated (%v)", dir, uid, readErr)
	}
	return fmt.Sprintf("%s: owned by uid %d and not writable by this user — a rootless build "+
		"leaves its store owned by the container's mapped UIDs, which only that mapping can empty", dir, uid)
}

// quarantinedBuildCaches lists the quarantined siblings of the live state root: the directories
// beside it whose name begins with `<the daemon's state-root name>.broken-`.
//
// The discovery is a PREFIX test over ONE ReadDir, never a glob: the cache root is an operator-
// configurable path (`CHARLY_CACHE_DIR`, D-STORE-1), so a glob built from it would read a `[` or a
// `*` in that path as pattern syntax — matching something else, or nothing at all, silently.
func quarantinedBuildCaches(stateRoot string) ([]string, error) {
	parent := filepath.Dir(stateRoot)
	prefix := filepath.Base(stateRoot) + quarantinedSuffix
	ents, err := os.ReadDir(parent)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing the cache root %s: %w", parent, err)
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		out = append(out, filepath.Join(parent, e.Name()))
	}
	sort.Strings(out)
	return out, nil
}

// liveBuildCacheReason is the operator-facing reason a live build cache was left alone. It names
// the HOLDER — the daemon, by the lock it keeps and the socket it answers on — rather than saying
// "in use", because the only question a reader has next is "in use by WHAT, and how do I stop it".
func liveBuildCacheReason(l buildCacheLayout) string {
	where := ""
	if l.Socket != "" {
		where = fmt.Sprintf(" (it answers on %s)", l.Socket)
	}
	return fmt.Sprintf("a running buildkitd holds %s%s, so a build may be writing this store; stop "+
		"the daemon or wait for the build to finish, then re-run", l.Lock, where)
}
