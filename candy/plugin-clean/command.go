package clean

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/opencharly/sdk"
	"github.com/opencharly/sdk/kit"
	"github.com/opencharly/sdk/loaderkit"
	"github.com/opencharly/spec/spec"
)

// command.go — the externalized `charly clean` command. The plugin OWNS the flag grammar, the
// category orchestration, the output, AND (K1-alpha core-minimization) the SHARED retention
// ENGINE itself (retention.go — image-tag / build-candy / check-run pruning + the --deep
// store-wide dangling-image purge, also called by `charly box build` / `charly check run` /
// `charly box list tags` via verb:retention, the peer-adapter pattern verb:credential/verb:gpu/
// verb:tunnel already use). The engine runs LOCALLY here — no wire hop for the plugin's own CLI.
// The project's defaults.keep_images / keep_check_runs resolve PLUGIN-SIDE via the shared
// sdk/loaderkit.ResolveRetentionDefaultsViaExecutor (K-wave 2 cone R6 — the "retention-defaults"
// HostBuild seam is DELETED: the loader is plugin-reachable). No hidden core-command forward.
//
// clean is COMPILED-IN (charly.yml compiled_plugins): its Invoke(OpRun) runs in charly's process
// and gets the in-proc reverse channel (provider_command_external.go dispatchInProcCommand
// threads it), so the loader resolve reaches the host loader legs. The out-of-process cliMain path
// has NO reverse channel, so --invalidate/images/check/deep (which need the resolved keep-defaults)
// error there; runCleanCLI's own engine calls otherwise work standalone (list/invalidate need no
// defaults).
//
// THE CATEGORY SET IS DECLARED IN THE SCHEMA, NOT HERE (categories.go + schema/clean.cue): this file
// registers one flag per declared row and prints what the plan resolved to. There is no
// `anyCategory := …` chain and no `doScopes = …` rule left in Go — the default set and the
// `--deep` → `scopes` implication are the declaration's own `bare:`/`implies:` fields.

// cleanFlags is the parsed `charly clean` grammar: the MODIFIERS — `--dry-run`, `--keep`,
// `--invalidate` — which tune or target a category rather than naming one, plus one flag per
// DECLARED category (schema/clean.cue).
type cleanFlags struct {
	dryRun     bool
	keep       int
	invalidate string
	// vars holds the pointer each declared category's flag writes to, so the named set can be
	// read back after Parse without a per-word variable — the declaration decides how many there
	// are, and a category added to the schema is a flag the moment the row exists.
	vars map[string]*bool
}

// named returns the categories this invocation named, in the form cleanCategories resolves.
func (f *cleanFlags) named() map[string]bool {
	out := make(map[string]bool, len(f.vars))
	for word, p := range f.vars {
		out[word] = *p
	}
	return out
}

// newCleanFlagSet builds the grammar: the modifiers, then ONE flag per category declared in
// schema/clean.cue — its word as the flag name, its `contract:` line as the help text. It returns
// an error rather than panicking when the declaration cannot be read or decoded, so a broken
// embedded schema is a reported defect instead of a crash in a compiled-in command.
func newCleanFlagSet(f *cleanFlags) (*flag.FlagSet, error) {
	cats, err := declaredCategories()
	if err != nil {
		return nil, err
	}
	fs := flag.NewFlagSet("clean", flag.ContinueOnError)
	fs.BoolVar(&f.dryRun, "dry-run", false, "Print everything that would be removed; touch nothing")
	fs.IntVar(&f.keep, "keep", 0, "Override the retention count for this run (0 = use defaults:)")
	fs.StringVar(&f.invalidate, "invalidate", "", "Remove every charly-labeled image tag matching this glob (full ref or last path segment); runs ONLY the invalidation")
	f.vars = make(map[string]*bool, len(cats))
	for _, c := range cats {
		f.vars[c.Flag] = fs.Bool(c.Flag, false, c.Contract)
	}
	return fs, nil
}

// runCleanCLI parses the clean flags and drives the categories the plan resolved to:
// --invalidate (targeted image-tag invalidation), images (+ build-candy staging), check runs, the
// store-wide --deep purge, the --cache blob/retired-root sweep, the --scopes reaper, and the
// --build-cache reclaim of the builder's persistent cache.
func runCleanCLI(ctx context.Context, exec *sdk.Executor, args []string) error {
	var f cleanFlags
	fs, err := newCleanFlagSet(&f)
	if err != nil {
		return err
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	tag := "removed"
	if f.dryRun {
		tag = "would remove"
	}

	// --invalidate: targeted image-tag invalidation ONLY. Needs no keep-defaults.
	if f.invalidate != "" {
		reply := runRetention(spec.RetentionRequest{Dir: dir, DryRun: f.dryRun, Invalidate: f.invalidate})
		if reply.Error != "" {
			return fmt.Errorf("%s", reply.Error)
		}
		fmt.Printf("invalidate: %s %d tag(s) matching %q\n", tag, len(reply.ImageRefs), f.invalidate)
		for _, r := range reply.ImageRefs {
			fmt.Printf("  %s\n", r)
		}
		return nil
	}

	plan, perr := cleanCategories(f.named())
	if perr != nil {
		return perr
	}

	// Two independent capabilities are needed per category, and only the categories that use them
	// pay for them:
	//
	//   * the loader-resolved keep-defaults — only `--images` and `--check` (and `--deep`, which
	//     resolves them for the report's counts). Resolved PLUGIN-SIDE through the in-proc reverse
	//     channel, so those three need compiled-in placement; `--cache`, `--scopes` and
	//     `--build-cache` do not.
	//   * the container engine — only the categories that touch the container store: `--images`,
	//     `--deep` and `--invalidate`/`list`. The engine is resolved INSIDE runRetentionOutcome for
	//     exactly those, so `--cache`, `--check`, `--scopes` and `--build-cache` run on a host whose
	//     podman is absent or broken, which is the state a host with a hung build scope is most
	//     likely to be in.
	var keepImages, keepCheck int
	if plan.images || plan.check || plan.deep {
		var derr error
		keepImages, keepCheck, derr = resolveRetentionDefaults(ctx, exec, dir)
		if derr != nil {
			return derr
		}
	}
	if plan.any() {
		out := runRetentionOutcome(spec.RetentionRequest{
			Dir: dir, DryRun: f.dryRun, Images: plan.images, Check: plan.check, Deep: plan.deep, Cache: plan.cache,
			Keep: f.keep, KeepImages: keepImages, KeepCheckRuns: keepCheck,
		}, plan)
		if out.Reply.Error != "" {
			return fmt.Errorf("%s", out.Reply.Error)
		}
		if rerr := printRetentionResult(os.Stdout, tag, plan, out); rerr != nil {
			return rerr
		}
	}
	return nil
}

// printRetentionResult writes the per-category result lines to w. It is a plain function (not an
// inline block of runCleanCLI) so the SKIP reporting below is unit-testable without a live engine,
// an executor, or a captured stdout.
//
// Why it exists at all: a live-build-guarded sweep DECLINES by returning an empty list — exactly
// what a sweep that found nothing returns. Printing only the count therefore made
// `deep: removed 0 untagged image(s) store-wide` indistinguishable from "the tool is blind", and
// an operator looking at 63 removable untagged images (~45 GB) concluded the latter and fell back
// to raw `podman rmi -f`, which deletes the build-layer cache that makes the next build ~8x faster.
// So each guarded category now prints the engine's SKIP line — naming the cause and the in-flight
// build count — IN PLACE OF its removed count whenever the engine reports a skip.
func printRetentionResult(w io.Writer, tag string, plan cleanPlan, out retentionOutcome) error {
	reply := out.Reply
	var lines []string
	if plan.images {
		lines = append(lines, fmt.Sprintf("images: %s %d tag(s) (keep_images=%d)\n", tag, len(reply.ImageRefs), reply.KeepImages))
		for _, r := range reply.ImageRefs {
			lines = append(lines, fmt.Sprintf("  %s\n", r))
		}
		lines = append(lines, sweepLines("dangling", out.DanglingSkip,
			fmt.Sprintf("%s %d untagged charly image(s)", tag, len(reply.DanglingIDs)), reply.DanglingIDs)...)
		// The staging line prints UNCONDITIONALLY (it used to be omitted when it found nothing) —
		// an omitted line hid BOTH "nothing to sweep" and "the sweep declined", which is the very
		// conflation this reporting exists to end. A skip still replaces the count, never joins it.
		lines = append(lines, sweepLines("staging", out.StagingSkip,
			fmt.Sprintf("%s %d dead buildah staging dir(s)", tag, len(reply.StagingDirs)), reply.StagingDirs)...)
		lines = append(lines, fmt.Sprintf("build: %s %d staging dir(s) under .build/_candy (keep_images=%d)\n", tag, len(reply.BuildDirs), reply.KeepImages))
		for _, p := range reply.BuildDirs {
			lines = append(lines, fmt.Sprintf("  %s\n", p))
		}
	}
	if plan.check {
		lines = append(lines, fmt.Sprintf("check: %s %d run artifact(s) (keep_check_runs=%d, NOTES.md preserved)\n", tag, len(reply.CheckPaths), reply.KeepCheckRuns))
		for _, p := range reply.CheckPaths {
			lines = append(lines, fmt.Sprintf("  %s\n", p))
		}
	}
	if plan.deep {
		count := fmt.Sprintf("%s %d untagged image(s) store-wide (up to %s reclaimable — shared layers may reduce actual reclaim; pair with --invalidate for the fullest reclaim)", tag, len(reply.DeepIDs), kit.HumanBytes(reply.DeepBytes))
		lines = append(lines, sweepLines("deep", out.DeepSkip, count, reply.DeepIDs)...)
	}
	if plan.scopes {
		// The scope reaper's count line names the guard's own age bound, so a reader can tell
		// "nothing is hung" from "nothing is old enough to judge" without reading the flag help.
		lines = append(lines, sweepLines("scopes", out.ScopeSkip,
			fmt.Sprintf("%s %d hung transient build scope(s) (active > %s, no live builder)",
				tag, len(out.Scopes), buildScopeMaxAge), out.Scopes)...)
	}
	if plan.store && out.Store != nil {
		lines = append(lines, storeLines(out.Store)...)
	}
	if plan.cache {
		if len(reply.CacheStores) == 0 {
			lines = append(lines, "cache: no named cache stores found\n")
		}
		for _, s := range reply.CacheStores {
			lines = append(lines, fmt.Sprintf("cache %s: %d live entry(ies); %s %d unreferenced blob(s) (%s)\n",
				s.Name, s.Entries, tag, s.RemovedBlobs, kit.HumanBytes(s.RemovedBytes)))
		}
		// The retired roots, reported even when there is nothing to remove: "already gone" and
		// "never existed" are the states a re-run reaches, and a silent omission would read as a
		// sweep that found nothing to do rather than one with nothing to do.
		for _, r := range out.RefusedStores {
			lines = append(lines, fmt.Sprintf("cache %s: REFUSED at %s — %s\n", r.Name, r.Dir, r.Reason))
		}
		for _, r := range out.RetiredRoots {
			switch {
			case !r.Present:
				lines = append(lines, fmt.Sprintf("cache retired %s: absent at %s (already swept)\n", r.Name, r.Dir))
			default:
				lines = append(lines, fmt.Sprintf("cache retired %s: %s %s (%s)\n", r.Name, tag, kit.HumanBytes(r.Bytes), r.Dir))
			}
		}
	}
	if plan.buildCache {
		lines = append(lines, buildCacheLines(tag, out.BuildCache)...)
	}
	if _, err := fmt.Fprint(w, strings.Join(lines, "")); err != nil {
		return fmt.Errorf("printing the clean report: %w", err)
	}
	return nil
}

// storeLines renders the `store` category (D-STORE-7): the surfaces it read, the decision it made
// entry by entry, and the measured result against the authored budget.
//
// EVERY surface line prints, including one that legitimately found nothing: "the quadlets named
// nothing" and "the quadlets were not read" are the two states an operator must be able to tell
// apart, because the second is the one that deletes a live image. The sweep refuses rather than
// printing the second as if it were the first — the SKIP line covers the live-build case, and a
// refusal is an error, not a line.
//
// The BUDGET line is measured against a store size that is really measured in both modes: the
// sweep's `Held` before it (a dry run) and its `Retained` after it (a real run). A dry run cannot
// size the post-sweep store — the store's public surface cannot evaluate a hypothetical entry set
// without deleting it — so it says which number it is rather than printing one that would read as
// the other.
func storeLines(o *storeSweepOutcome) []string {
	if o.Skip != nil {
		return []string{"store: " + o.Skip.String() + "\n"}
	}
	var lines []string
	for _, s := range o.Surfaces {
		if s.Absent {
			lines = append(lines, fmt.Sprintf("store state %s: absent at %s (nothing to reference)\n", s.Name, s.Where))
			continue
		}
		carried := fmt.Sprintf("%d ref(s)", s.Refs)
		if s.Digests > 0 {
			carried += fmt.Sprintf(", %d content address(es)", s.Digests)
		}
		lines = append(lines, fmt.Sprintf("store state %s: %s (%s)\n", s.Name, carried, s.Where))
	}
	if len(o.Decisions) == 0 {
		lines = append(lines, fmt.Sprintf("store %s: no entries\n", o.Dir))
		return lines
	}
	kept, evicted := o.kept(), o.evicted()
	verb := "evicted"
	if o.DryRun {
		verb = "would evict"
	}
	lines = append(lines, fmt.Sprintf("store %s: %d entry(ies); %d kept by state, %d %s\n",
		o.Dir, len(o.Decisions), len(kept), len(evicted), verb))
	for _, d := range kept {
		lines = append(lines, fmt.Sprintf("  kept    %s  %s  (%s)\n", refLabel(d.Ref), shortDigest(d.Key), d.Reason))
	}
	for _, d := range evicted {
		lines = append(lines, fmt.Sprintf("  evict   %s  %s  (%s)\n", refLabel(d.Ref), shortDigest(d.Key), d.Reason))
	}
	if o.AlreadyGone > 0 {
		lines = append(lines, fmt.Sprintf(
			"  (%d planned eviction(s) found the entry already gone — a concurrent writer got there first)\n",
			o.AlreadyGone))
	}
	if o.DryRun {
		lines = append(lines, fmt.Sprintf(
			"store budget: %s held of %s — the real run measures what the evicted entries leave behind\n",
			kit.HumanBytes(o.Held), kit.HumanBytes(o.Budget)))
		lines = append(lines, fmt.Sprintf(
			"store reclaim: would free %s of already-unreferenced blob(s), and each evicted entry's own layers with them\n",
			kit.HumanBytes(o.Reclaim)))
		return lines
	}
	lines = append(lines, fmt.Sprintf("store budget: retained %s of %s (%s)\n",
		kit.HumanBytes(o.Retained), kit.HumanBytes(o.Budget), budgetVerdict(o.Retained, o.Budget)))
	lines = append(lines, fmt.Sprintf("store reclaim: freed %s of blobs no surviving entry reaches\n", kit.HumanBytes(o.Reclaim)))
	return lines
}

// budgetVerdict is whether the store the sweep left behind is inside D-STORE-7's budget. An
// over-budget store is REPORTED, never "fixed": the entries that are left are the ones state
// references, and evicting one of them to meet a number is the failure this command exists to
// avoid.
func budgetVerdict(retained, budget int64) string {
	if retained <= budget {
		return "within budget"
	}
	return fmt.Sprintf("OVER budget by %s — the state references these entries, so nothing further may be evicted", kit.HumanBytes(retained-budget))
}

// refLabel renders an entry's reference, or a dash for an entry that carries none.
func refLabel(ref string) string {
	if ref == "" {
		return "-"
	}
	return ref
}

// buildCacheLines renders the `--build-cache` section: ONE line per root, each carrying its own
// status and its own byte figure.
//
// The live store and every quarantined corpse fail INDEPENDENTLY — a live root held by a running
// daemon says nothing about the corpse beside it, and a corpse this user cannot delete says
// nothing about the live store — so a single category count would have to lie about at least one
// of them. The live root therefore carries its own line, and the quarantined roots share a count
// line plus one status line each.
func buildCacheLines(tag string, roots []buildCacheRootOutcome) []string {
	// Never a silent blank: the engine returns one entry per root it examined, so an empty slice
	// means the category reported nothing, which is a defect worth seeing rather than a blank.
	if len(roots) == 0 {
		return []string{"build-cache: no root reported\n"}
	}
	var lines []string
	for _, r := range roots {
		if r.Quarantined {
			continue
		}
		text, _ := buildCacheRootStatus(tag, r)
		lines = append(lines, "build-cache live: "+text+"\n")
	}
	// The quarantined roots accumulate into ONE count line, and each status line follows it — the
	// summary always precedes the lines it summarizes, so a reader scanning the section in order is
	// never told the total before the verdict that produced it.
	var deadLines []string
	dead, deadRemoved := 0, 0
	var deadBytes int64
	for _, r := range roots {
		if !r.Quarantined {
			continue
		}
		text, reclaimed := buildCacheRootStatus(tag, r)
		deadLines = append(deadLines, "  "+text+"\n")
		dead++
		if reclaimed {
			deadRemoved++
			deadBytes += r.Bytes
		}
	}
	if dead == 0 {
		lines = append(lines, "build-cache dead: no quarantined root(s) found\n")
	} else {
		lines = append(lines, fmt.Sprintf("build-cache dead: %s %d quarantined root(s) (%s)\n",
			tag, deadRemoved, kit.HumanBytes(deadBytes)))
		lines = append(lines, deadLines...)
	}
	return lines
}

// buildCacheRootStatus is ONE root's status text and whether that status counts as reclaimed (a
// real removal, or a would-remove under --dry-run). The four states are exclusive and EVERY one is
// printed — absent, skipped by the lock guard, blocked by file ownership, reclaimed — because the
// value of this report is the root it tells the operator to leave alone as much as the bytes it
// reports.
func buildCacheRootStatus(tag string, r buildCacheRootOutcome) (string, bool) {
	switch {
	case !r.Present:
		return fmt.Sprintf("absent at %s (nothing to remove)", r.Dir), false
	case r.Skip != nil:
		return r.Skip.String(), false
	case r.Blocker != "":
		// Present tense on purpose: "cannot be removed" and "the tree is left whole" are true
		// under --dry-run AND after a real run, so the line never claims an action that did not
		// happen.
		return fmt.Sprintf("cannot be removed — %s holds %s and %s; the tree is left whole",
			r.Dir, kit.HumanBytes(r.Bytes), r.Blocker), false
	default:
		return fmt.Sprintf("%s %s (%s)", tag, r.Dir, kit.HumanBytes(r.Bytes)), true
	}
}

// sweepLines renders ONE live-build-guarded sweep's report lines: the explicit SKIP line when the
// engine reports the guard declined (IN PLACE OF the count — there is no count to print, the sweep
// never ran), otherwise the removed/would-remove count line followed by the item list.
func sweepLines(label string, skip *retentionSkip, count string, items []string) []string {
	if skip != nil {
		return []string{fmt.Sprintf("%s: %s\n", label, skip)}
	}
	lines := []string{fmt.Sprintf("%s: %s\n", label, count)}
	for _, item := range items {
		lines = append(lines, fmt.Sprintf("  %s\n", item))
	}
	return lines
}

// resolveRetentionDefaults resolves defaults.keep_images/keep_check_runs PLUGIN-SIDE via the
// shared sdk/loaderkit.ResolveRetentionDefaultsViaExecutor (K-wave 2 cone R6 — the former
// "retention-defaults" HostBuild seam is DELETED; the loader is plugin-reachable over the reverse
// channel, so the retention engine's keep-defaults input no longer needs a host round-trip). exec
// is nil on the out-of-process cliMain path (no reverse channel) → a clear error naming the
// compiled-in requirement for the categories that need a resolved default.
func resolveRetentionDefaults(ctx context.Context, exec *sdk.Executor, dir string) (keepImages, keepCheck int, err error) {
	if exec == nil {
		return 0, 0, fmt.Errorf("charly clean --images/--check/--deep requires compiled-in placement (the loader reverse channel is unavailable out-of-process)")
	}
	keepImages, keepCheck = loaderkit.ResolveRetentionDefaultsViaExecutor(ctx, exec, dir)
	return keepImages, keepCheck, nil
}
