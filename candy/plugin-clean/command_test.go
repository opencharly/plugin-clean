package clean

import (
	"bytes"
	"flag"
	"strings"
	"testing"

	"github.com/opencharly/spec/spec"
)

// namedSet builds the map cleanCategories resolves from the six category flags, so a case reads
// as the flags an operator typed rather than as a positional tuple.
func namedSet(images, check, deep, cacheGC, scopes, buildCache bool) map[string]bool {
	out := map[string]bool{}
	for word, on := range map[string]bool{
		"images":      images,
		"check":       check,
		"deep":        deep,
		"cache":       cacheGC,
		"scopes":      scopes,
		"build-cache": buildCache,
	} {
		if on {
			out[word] = true
		}
	}
	return out
}

// TestCleanCategories covers the category discipline: the pre-existing "any one of
// --images/--check given alone suppresses the other default categories" behavior stays unchanged,
// --deep NEVER fires implicitly on a plain `charly clean` (R5: no default-behavior change) but
// joins the same "an explicit category was given" gate as --images/--check, --scopes is the one
// category that runs BOTH by default and with --deep (safe by construction, and a hung build scope
// starves a whole host rather than costing disk — opencharly/plugin-clean#11), and the new
// --build-cache is strictly opt-in.
//
// ⚑ The discipline is NOT hand-written in Go any more (categories.go derives it from
// schema/clean.cue's `bare:`/`implies:` rows), so this table is what proves the DERIVATION
// reproduces the rule the old hand-written `cleanCategories` implemented, case for case. Mutating
// the declaration — dropping `bare: true` from a default category, or `implies: [scopes]` from
// `deep` — turns these cases red, which is the point: the schema is the rule, and a silent edit to
// it cannot pass.
func TestCleanCategories(t *testing.T) {
	cases := []struct {
		name                         string
		images, check, deep, cacheGC bool
		scopes, buildCache           bool
		want                         cleanPlan
	}{
		{name: "no flags: full default sweep, scopes included, deep and build-cache excluded",
			want: cleanPlan{images: true, check: true, scopes: true}},
		{name: "--images alone: only images, no scopes",
			images: true,
			want:   cleanPlan{images: true}},
		{name: "--check alone: only check, no scopes",
			check: true,
			want:  cleanPlan{check: true}},
		{name: "--images + --check: both, no scopes",
			images: true, check: true,
			want: cleanPlan{images: true, check: true}},
		{name: "--deep alone: deep AND scopes",
			deep: true,
			want: cleanPlan{deep: true, scopes: true}},
		{name: "--deep + --images: both, check excluded, scopes from deep",
			images: true, deep: true,
			want: cleanPlan{images: true, deep: true, scopes: true}},
		{name: "--deep + --check: both, images excluded, scopes from deep",
			check: true, deep: true,
			want: cleanPlan{check: true, deep: true, scopes: true}},
		{name: "all three flags: all four categories",
			images: true, check: true, deep: true,
			want: cleanPlan{images: true, check: true, deep: true, scopes: true}},
		{name: "--cache alone: only cache (never images/check/scopes)",
			cacheGC: true,
			want:    cleanPlan{cache: true}},
		{name: "--cache + --images: both, check and scopes excluded",
			images: true, cacheGC: true,
			want: cleanPlan{images: true, cache: true}},
		{name: "--scopes alone: ONLY scopes (no images/check)",
			scopes: true,
			want:   cleanPlan{scopes: true}},
		{name: "--scopes + --images: both, check excluded",
			images: true, scopes: true,
			want: cleanPlan{images: true, scopes: true}},
		{name: "--build-cache alone: ONLY build-cache — naming it suppresses the default set",
			buildCache: true,
			want:       cleanPlan{buildCache: true}},
		{name: "--build-cache + --scopes: both, and no default category leaks in",
			scopes: true, buildCache: true,
			want: cleanPlan{scopes: true, buildCache: true}},
		{name: "--build-cache + --deep: deep, its scope reap, and the builder's cache",
			deep: true, buildCache: true,
			want: cleanPlan{deep: true, scopes: true, buildCache: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := cleanCategories(namedSet(c.images, c.check, c.deep, c.cacheGC, c.scopes, c.buildCache))
			if err != nil {
				t.Fatalf("cleanCategories: %v", err)
			}
			if got != c.want {
				t.Errorf("cleanCategories(images=%v,check=%v,deep=%v,cache=%v,scopes=%v,build-cache=%v) = %+v, want %+v",
					c.images, c.check, c.deep, c.cacheGC, c.scopes, c.buildCache, got, c.want)
			}
		})
	}
}

// TestNewCleanFlagSetAcceptsEveryDeclaredCategory pins the DERIVATION the other way round: every
// category the schema declares is a flag the CLI accepts, with the row's own contract as its help
// text, and no category word is registered twice (a duplicate would panic inside flag, so the
// declaration's uniqueness is asserted where it is decoded AND here).
func TestNewCleanFlagSetAcceptsEveryDeclaredCategory(t *testing.T) {
	cats, err := declaredCategories()
	if err != nil {
		t.Fatalf("declaredCategories: %v", err)
	}
	var f cleanFlags
	fs, err := newCleanFlagSet(&f)
	if err != nil {
		t.Fatalf("newCleanFlagSet: %v", err)
	}
	registered := map[string]*flag.Flag{}
	fs.VisitAll(func(fl *flag.Flag) { registered[fl.Name] = fl })
	for _, c := range cats {
		fl, ok := registered[c.Flag]
		if !ok {
			t.Errorf("declared category %q is not a flag the CLI accepts", c.Flag)
			continue
		}
		if fl.Usage != c.Contract {
			t.Errorf("category %q help text = %q, want the declared contract %q", c.Flag, fl.Usage, c.Contract)
		}
	}
	// The reverse direction: the plan's word→field table covers EXACTLY the declared set, so a
	// field without a row could never be named and a row without a field could never run.
	if len(categoryPlanFields) != len(cats) {
		t.Fatalf("categoryPlanFields binds %d word(s) but the schema declares %d", len(categoryPlanFields), len(cats))
	}
	for _, c := range cats {
		if categoryPlanFields[c.Flag] == nil {
			t.Errorf("declared category %q has no cleanPlan field bound to it", c.Flag)
		}
	}
	var probe cleanPlan
	for word, field := range categoryPlanFields {
		declared := false
		for _, c := range cats {
			if c.Flag == word {
				declared = true
				break
			}
		}
		if !declared {
			t.Errorf("categoryPlanFields binds %q, which the schema does not declare", word)
		}
		if field(&probe) == nil {
			t.Errorf("categoryPlanFields[%q] returns a nil field", word)
		}
	}
}

// TestCleanCategoriesParseTheFullGrammar drives the real flag parser, so the derived grammar is
// proven end to end: what `--build-cache` parses to is what cleanCategories resolves, and the
// modifiers it shares a FlagSet with do not disturb it.
func TestCleanCategoriesParseTheFullGrammar(t *testing.T) {
	cases := []struct {
		args []string
		want cleanPlan
	}{
		{args: nil, want: cleanPlan{images: true, check: true, scopes: true}},
		{args: []string{"--build-cache"}, want: cleanPlan{buildCache: true}},
		{args: []string{"--build-cache", "--dry-run"}, want: cleanPlan{buildCache: true}},
		{args: []string{"--deep", "--dry-run", "--keep", "1"}, want: cleanPlan{deep: true, scopes: true}},
	}
	for _, c := range cases {
		var f cleanFlags
		fs, err := newCleanFlagSet(&f)
		if err != nil {
			t.Fatalf("newCleanFlagSet: %v", err)
		}
		if err := fs.Parse(c.args); err != nil {
			t.Fatalf("parse %v: %v", c.args, err)
		}
		got, err := cleanCategories(f.named())
		if err != nil {
			t.Fatalf("cleanCategories: %v", err)
		}
		if got != c.want {
			t.Errorf("parse %v resolved %+v, want %+v", c.args, got, c.want)
		}
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
		if err := printRetentionResult(&buf, "removed", cleanPlan{deep: true}, out); err != nil {
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
		if err := printRetentionResult(&buf, "removed", cleanPlan{images: true}, out); err != nil {
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
		if err := printRetentionResult(&buf, "removed", cleanPlan{images: true, deep: true}, out); err != nil {
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
		if err := printRetentionResult(&buf, "removed", cleanPlan{images: true}, out); err != nil {
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
		if err := printRetentionResult(&buf, "removed", cleanPlan{cache: true}, out); err != nil {
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
		if err := printRetentionResult(&buf, "would remove", cleanPlan{cache: true}, out); err != nil {
			t.Fatalf("print: %v", err)
		}
		if want := "cache: no named cache stores found\n"; buf.String() != want {
			t.Errorf("got %q, want %q", buf.String(), want)
		}
	})
}
