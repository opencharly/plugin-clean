package clean

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/opencharly/sdk/kit"
)

// stubExports points the sweep at a temp directory and makes the live-build guard report no live
// build — the same package-var seam the other categories use (liveBuildFloor).
func stubExports(t *testing.T, dir string) {
	t.Helper()
	prevDir, prevFloor := exportDir, liveBuildFloor
	exportDir = func() (string, error) { return dir, nil }
	liveBuildFloor = func() (kit.CalVer, bool, int) { return kit.CalVer{}, false, 0 }
	t.Cleanup(func() { exportDir, liveBuildFloor = prevDir, prevFloor })
}

// TestSweepExports_RemovesArchivesAndTemps covers the two shapes the builder leaves behind: the
// archive itself (buildkitdrive.ExportFile) and the temp file the in-place index rewrite stages
// beside it (os.CreateTemp), which a crash leaves too.
func TestSweepExports_RemovesArchivesAndTemps(t *testing.T) {
	dir := t.TempDir()
	stubExports(t, dir)
	archive := writeFile(t, dir, "ghcr.io_opencharly_arch_probe.oci.tar", 128)
	staged := writeFile(t, dir, ".ghcr.io_opencharly_arch_probe.oci.tar.987654", 16)
	other := writeFile(t, dir, "unrelated.txt", 8)
	if err := os.Mkdir(filepath.Join(dir, "a-directory.oci.tar"), 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := sweepExports(false)
	if err != nil {
		t.Fatalf("sweepExports: %v", err)
	}
	if out.Skip != nil {
		t.Fatalf("unexpected skip: %v", out.Skip)
	}
	if len(out.Removed) != 2 {
		t.Fatalf("removed %d file(s), want 2: %v", len(out.Removed), out.Removed)
	}
	if out.Bytes != 144 {
		t.Errorf("reported %d bytes, want 144", out.Bytes)
	}
	for _, gone := range []string{archive, staged} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s survived: %v", gone, err)
		}
	}
	for _, kept := range []string{other, filepath.Join(dir, "a-directory.oci.tar")} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s must be untouched: %v", kept, err)
		}
	}
}

// TestSweepExports_DryRunTouchesNothing: the plan is reported, the files stay.
func TestSweepExports_DryRunTouchesNothing(t *testing.T) {
	dir := t.TempDir()
	stubExports(t, dir)
	archive := writeFile(t, dir, "img.oci.tar", 64)

	out, err := sweepExports(true)
	if err != nil {
		t.Fatalf("sweepExports(dryRun): %v", err)
	}
	if len(out.Removed) != 1 {
		t.Fatalf("dry run reported %d file(s), want 1", len(out.Removed))
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("a dry run removed the archive: %v", err)
	}
}

// TestSweepExports_DeclinesWhileABuildIsLive: a build owns its archive until it returns, so the
// sweep reports the guard's decision and removes nothing — the SKIP must be distinguishable from
// "found nothing", which is why the outcome carries the skip rather than an empty list alone.
func TestSweepExports_DeclinesWhileABuildIsLive(t *testing.T) {
	dir := t.TempDir()
	stubExports(t, dir)
	liveBuildFloor = func() (kit.CalVer, bool, int) { return kit.CalVer{}, false, 1 }
	archive := writeFile(t, dir, "img.oci.tar", 64)

	out, err := sweepExports(false)
	if err != nil {
		t.Fatalf("sweepExports: %v", err)
	}
	if out.Skip == nil {
		t.Fatal("a live build must produce a skip, not an empty result")
	}
	if len(out.Removed) != 0 {
		t.Fatalf("removed %d file(s) while a build was live", len(out.Removed))
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("the archive was removed while a build was live: %v", err)
	}
}

// TestSweepExports_LeavesAnOpenArchive: an archive a process holds open is named and kept. The
// test holds the descriptor itself, so the /proc scan is exercised for real.
func TestSweepExports_LeavesAnOpenArchive(t *testing.T) {
	dir := t.TempDir()
	stubExports(t, dir)
	archive := writeFile(t, dir, "img.oci.tar", 64)

	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	out, err := sweepExports(false)
	if err != nil {
		t.Fatalf("sweepExports: %v", err)
	}
	if len(out.Held) != 1 {
		t.Fatalf("held %d archive(s), want 1: %v", len(out.Held), out.Held)
	}
	if len(out.Removed) != 0 {
		t.Fatalf("removed %d archive(s) that were open", len(out.Removed))
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("the open archive was removed: %v", err)
	}
}
