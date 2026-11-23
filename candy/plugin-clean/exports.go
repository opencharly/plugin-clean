package clean

// exports.go — the `exports` category: the builder's INTERMEDIATE OCI export archives.
//
// Every image build stages its result as an OCI layout archive under the builder's state root
// (`buildkitdrive.StageDir`), bridges it into the run engine, and removes it. The archive has one
// consumer and no later build reuses its name, so anything left there is debris: a build that
// failed (buildkit writes the destination before the solve completes, so an ENOSPC leaves a
// partial file), a bridge that failed, a crash, or a kill. Measured on a host with a build
// history: 71 archives, 68 GiB, and an export that eventually failed on a full disk under a path
// nobody recognised as a leak (opencharly/plugin-build#64).
//
// `charly clean --build-cache` cannot cover this: it reclaims the builder's WHOLE state root and
// therefore refuses while the daemon holds its one-instance lock, which a long-lived daemon always
// does. This category touches only the export directory's own files.
//
// TWO guards, and both are load-bearing:
//
//  1. NO LIVE BUILD (the same `liveBuildFloor` every destructive category uses). A build's archive
//     exists exactly while the build runs, so the sweep declines wholesale when any is in flight.
//  2. NO OPEN HANDLE. The live-build lock alone is NOT sufficient — measured on a running
//     `charly box build`: `~/.cache/charly/locks/builds` was EMPTY while the build was
//     mid-resolve. A file a process holds open (`podman load -i <archive>`) is named and left
//     alone, whatever the lock says.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/opencharly/sdk/buildkitdrive"
)

// exportSweepOutcome is ONE `exports` run: where it looked, the guard's decision, and what it
// removed (or, under --dry-run, what it WOULD remove).
type exportSweepOutcome struct {
	// Dir is the resolved export directory (the report names it, so an operator can see which
	// root a relocated install swept).
	Dir string
	// Skip is set when the live-build guard declined the sweep — never when it merely found
	// nothing.
	Skip *retentionSkip
	// Removed is the archives removed, or would be under a dry run.
	Removed []string
	// Held is the archives left alone because a process holds them open.
	Held []string
	// Bytes is the summed size of Removed before removal.
	Bytes int64
}

// exportDir resolves the builder's export directory. A seam (the package-var pattern this
// engine's other categories use) so a test never touches the user's real cache root.
var exportDir = func() (string, error) {
	dir, err := buildkitdrive.StageDir()
	if err != nil {
		return "", fmt.Errorf("locating the builder's export directory: %w", err)
	}
	return dir, nil
}

// sweepExports removes the builder's leftover OCI export archives.
func sweepExports(dryRun bool) (exportSweepOutcome, error) {
	dir, err := exportDir()
	if err != nil {
		return exportSweepOutcome{}, err
	}
	out := exportSweepOutcome{Dir: dir}

	// Guard 1: a live build owns its archive until it returns.
	if _, _, live := liveBuildFloor(); live > 0 {
		out.Skip = &retentionSkip{live: live, reason: "the builder's export directory belongs to the builds in flight"}
		return out, nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return out, fmt.Errorf("reading the builder's export directory %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !isExportArtifact(e.Name()) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		// Guard 2: a process reading or writing the archive (a build's engine load, a
		// concurrent export) is named and left alone.
		if pathHeldOpen(p) {
			out.Held = append(out.Held, p)
			continue
		}
		if info, ierr := e.Info(); ierr == nil {
			out.Bytes += info.Size()
		}
		if !dryRun {
			if rerr := os.Remove(p); rerr != nil && !os.IsNotExist(rerr) {
				return out, fmt.Errorf("removing the export archive %s: %w", p, rerr)
			}
		}
		out.Removed = append(out.Removed, p)
	}
	return out, nil
}

// isExportArtifact reports whether a name is one of the builder's intermediate archives:
// `<ref>.oci.tar` (`buildkitdrive.ExportFile`), or the temp file the in-place index rewrite
// stages beside it (`.<archive>.oci.tar.<random>`, `os.CreateTemp`), which a crash leaves behind.
func isExportArtifact(name string) bool {
	if strings.HasSuffix(name, ".oci.tar") {
		return true
	}
	return strings.HasPrefix(name, ".") && strings.Contains(name, ".oci.tar.")
}

// pathHeldOpen reports whether any process keeps p open, by scanning /proc/<pid>/fd.
//
// It narrows the window the live-build lock cannot see: an archive is written, then opened by the
// bridge a moment later, and unlinking it in between makes the bridge fail. Once a bridge HAS the
// file open, an unlink is harmless on Unix (the holder keeps its descriptor), so an unreadable
// /proc entry is not a reason to refuse — it can only widen the window back to the lock's
// protection, never corrupt a running load. charly is a Linux tool; elsewhere the scan finds
// nothing and the live-build guard is what protects an archive.
func pathHeldOpen(p string) bool {
	want := filepath.Clean(p)
	pids, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, pid := range pids {
		if !pid.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(pid.Name()); err != nil {
			continue // not a process entry
		}
		fds, err := os.ReadDir(filepath.Join("/proc", pid.Name(), "fd"))
		if err != nil {
			continue // gone, or not ours to read
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join("/proc", pid.Name(), "fd", fd.Name()))
			if err != nil {
				continue
			}
			target = strings.TrimSuffix(target, " (deleted)") // a deleted-but-open file
			if filepath.Clean(target) == want {
				return true
			}
		}
	}
	return false
}
