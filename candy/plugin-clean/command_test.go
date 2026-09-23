package clean

import (
	"bytes"
	"strings"
	"testing"

	"github.com/opencharly/spec/spec"
)

// TestCleanCategories covers the --images/--check/--deep flag-resolution logic: the pre-existing
// "any one of --images/--check given alone suppresses the other default categories" behavior stays
// unchanged, and --deep NEVER fires implicitly on a plain `charly clean` (R5: no default-behavior
// change) but joins the same "an explicit category was given" gate as --images/--check.
func TestCleanCategories(t *testing.T) {
	cases := []struct {
		name                            string
		images, check, deep, cacheGC    bool
		wantImages, wantCheck, wantDeep bool
		wantCache                       bool
	}{
		{name: "no flags: full default sweep, deep excluded",
			images: false, check: false, deep: false,
			wantImages: true, wantCheck: true, wantDeep: false},
		{name: "--images alone: only images",
			images: true, check: false, deep: false,
			wantImages: true, wantCheck: false, wantDeep: false},
		{name: "--check alone: only check",
			images: false, check: true, deep: false,
			wantImages: false, wantCheck: true, wantDeep: false},
		{name: "--images + --check: both",
			images: true, check: true, deep: false,
			wantImages: true, wantCheck: true, wantDeep: false},
		{name: "--deep alone: only deep",
			images: false, check: false, deep: true,
			wantImages: false, wantCheck: false, wantDeep: true},
		{name: "--deep + --images: both, check excluded",
			images: true, check: false, deep: true,
			wantImages: true, wantCheck: false, wantDeep: true},
		{name: "--deep + --check: both, images excluded",
			images: false, check: true, deep: true,
			wantImages: false, wantCheck: true, wantDeep: true},
		{name: "all three flags: all categories",
			images: true, check: true, deep: true,
			wantImages: true, wantCheck: true, wantDeep: true},
		{name: "--cache alone: only cache (never images/check)",
			images: false, check: false, deep: false, cacheGC: true,
			wantImages: false, wantCheck: false, wantDeep: false, wantCache: true},
		{name: "--cache + --images: both, check excluded",
			images: true, check: false, deep: false, cacheGC: true,
			wantImages: true, wantCheck: false, wantDeep: false, wantCache: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotImages, gotCheck, gotDeep, gotCache := cleanCategories(c.images, c.check, c.deep, c.cacheGC)
			if gotImages != c.wantImages || gotCheck != c.wantCheck || gotDeep != c.wantDeep || gotCache != c.wantCache {
				t.Errorf("cleanCategories(%v,%v,%v,%v) = (%v,%v,%v,%v), want (%v,%v,%v,%v)",
					c.images, c.check, c.deep, c.cacheGC,
					gotImages, gotCheck, gotDeep, gotCache,
					c.wantImages, c.wantCheck, c.wantDeep, c.wantCache)
			}
		})
	}
}

// TestPrintRetentionResult_SkipIsExplicit is the OPERATOR-FACING half of the fix: when a
// live-build-guarded sweep declines, the CLI prints the engine's SKIP line — naming the cause and
// the in-flight build count — IN PLACE OF the removed count, so "0 removed" can never again be
// read as "nothing to remove". The fixture replies deliberately carry NON-EMPTY ids: a skip that
// still printed them would be exactly the ambiguity this closes.
func TestPrintRetentionResult_SkipIsExplicit(t *testing.T) {
	skipImages := &retentionSkip{live: 2, reason: skipReasonImages}
	skipStaging := &retentionSkip{live: 2, reason: skipReasonStaging}
	const (
		wantDeepSkip     = "deep: SKIPPED — 2 build(s) in flight; images are never removed during a build (re-run when builds are idle)\n"
		wantDanglingSkip = "dangling: SKIPPED — 2 build(s) in flight; images are never removed during a build (re-run when builds are idle)\n"
		wantStagingSkip  = "staging: SKIPPED — 2 build(s) in flight; buildah/podman staging of an in-flight build is never swept (re-run when builds are idle)\n"
	)

	t.Run("--deep", func(t *testing.T) {
		var buf bytes.Buffer
		out := retentionOutcome{
			Reply:    spec.RetentionReply{DeepIDs: []string{"sha256:aaaa"}, DeepBytes: 45 << 30},
			DeepSkip: skipImages,
		}
		if err := printRetentionResult(&buf, "removed", false, false, true, false, out); err != nil {
			t.Fatalf("print: %v", err)
		}
		if got := buf.String(); got != wantDeepSkip {
			t.Errorf("--deep skip output\n got: %q\nwant: %q", got, wantDeepSkip)
		}
	})

	t.Run("--images", func(t *testing.T) {
		var buf bytes.Buffer
		out := retentionOutcome{
			Reply: spec.RetentionReply{
				KeepImages:  3,
				DanglingIDs: []string{"sha256:bbbb"},
				StagingDirs: []string{"/var/tmp/buildah-x"},
			},
			DanglingSkip: skipImages,
			StagingSkip:  skipStaging,
		}
		if err := printRetentionResult(&buf, "removed", true, false, false, false, out); err != nil {
			t.Fatalf("print: %v", err)
		}
		want := "images: removed 0 tag(s) (keep_images=3)\n" + wantDanglingSkip + wantStagingSkip +
			"build: removed 0 staging dir(s) under .build/_candy (keep_images=3)\n"
		if got := buf.String(); got != want {
			t.Errorf("--images skip output\n got: %q\nwant: %q", got, want)
		}
	})

	t.Run("no live build: counts, never a skip line", func(t *testing.T) {
		var buf bytes.Buffer
		out := retentionOutcome{Reply: spec.RetentionReply{
			KeepImages:  3,
			DanglingIDs: []string{"sha256:bbbb"},
			StagingDirs: []string{"/var/tmp/buildah-x"},
			DeepIDs:     []string{"sha256:aaaa", "sha256:cccc"},
			DeepBytes:   45 << 30,
		}}
		if err := printRetentionResult(&buf, "removed", true, false, true, false, out); err != nil {
			t.Fatalf("print: %v", err)
		}
		got := buf.String()
		if strings.Contains(got, "SKIPPED") {
			t.Errorf("the engine reported no skip, yet the output claims one:\n%s", got)
		}
		for _, want := range []string{
			"images: removed 0 tag(s) (keep_images=3)\n",
			"dangling: removed 1 untagged charly image(s)\n  sha256:bbbb\n",
			"staging: removed 1 dead buildah staging dir(s)\n  /var/tmp/buildah-x\n",
			"deep: removed 2 untagged image(s) store-wide (up to ",
			"  sha256:aaaa\n  sha256:cccc\n",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("output missing %q:\n%s", want, got)
			}
		}
	})

	// The staging line used to be omitted whenever it found nothing, which hid BOTH "nothing to
	// sweep" and "the sweep declined". It is now printed unconditionally, like `dangling`.
	t.Run("empty staging is still reported", func(t *testing.T) {
		var buf bytes.Buffer
		out := retentionOutcome{Reply: spec.RetentionReply{KeepImages: 3}}
		if err := printRetentionResult(&buf, "removed", true, false, false, false, out); err != nil {
			t.Fatalf("print: %v", err)
		}
		want := "images: removed 0 tag(s) (keep_images=3)\n" +
			"dangling: removed 0 untagged charly image(s)\n" +
			"staging: removed 0 dead buildah staging dir(s)\n" +
			"build: removed 0 staging dir(s) under .build/_candy (keep_images=3)\n"
		if got := buf.String(); got != want {
			t.Errorf("output\n got: %q\nwant: %q", got, want)
		}
	})
}

// TestPrintRetentionResult_CacheCategory pins the `--cache` report shape: one
// line per named store carrying its live entry count and the reclaimed
// blob/byte figures, and an explicit "no named cache stores" line when the root
// holds none (never a silent blank).
func TestPrintRetentionResult_CacheCategory(t *testing.T) {
	t.Run("named stores", func(t *testing.T) {
		var buf bytes.Buffer
		out := retentionOutcome{Reply: spec.RetentionReply{CacheStores: []spec.CacheStoreInfo{
			{Name: "project", Entries: 7, RemovedBlobs: 3, RemovedBytes: 2 << 20},
			{Name: "materialized", Entries: 2, RemovedBlobs: 0, RemovedBytes: 0},
		}}}
		if err := printRetentionResult(&buf, "removed", false, false, false, true, out); err != nil {
			t.Fatalf("print: %v", err)
		}
		got := buf.String()
		for _, want := range []string{
			"cache project: 7 live entry(ies); removed 3 unreferenced blob(s)",
			"cache materialized: 2 live entry(ies); removed 0 unreferenced blob(s)",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("output missing %q:\n%s", want, got)
			}
		}
	})

	t.Run("no stores", func(t *testing.T) {
		var buf bytes.Buffer
		out := retentionOutcome{Reply: spec.RetentionReply{}}
		if err := printRetentionResult(&buf, "would remove", false, false, false, true, out); err != nil {
			t.Fatalf("print: %v", err)
		}
		if want := "cache: no named cache stores found\n"; buf.String() != want {
			t.Errorf("got %q, want %q", buf.String(), want)
		}
	})
}
