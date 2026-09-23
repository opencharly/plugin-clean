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

// runCleanCLI parses the clean flags and drives the categories: --invalidate (targeted image-tag
// invalidation), images (+ build-candy staging), check runs, and the store-wide --deep purge.
func runCleanCLI(ctx context.Context, exec *sdk.Executor, args []string) error {
	fs := flag.NewFlagSet("clean", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "Print everything that would be removed; touch nothing")
	images := fs.Bool("images", false, "Only image-tag retention")
	check := fs.Bool("check", false, "Only check-run retention")
	deep := fs.Bool("deep", false, "Purge every untagged/dangling image in local storage (not just charly-labeled) plus any layer blobs they alone held — reports UP TO the summed image size, since layers SHARED with kept images reduce actual reclaim; pair with --invalidate for the fullest reclaim. Runs ONLY this category unless combined with --images/--check")
	cacheGC := fs.Bool("cache", false, "Reclaim unreferenced blobs from every named charly cache (the content-addressed ArtifactStore's own GC); reports each store's live entries + reclaimed blobs/bytes. Runs ONLY this category unless combined with --images/--check/--deep")
	keep := fs.Int("keep", 0, "Override the retention count for this run (0 = use defaults:)")
	invalidate := fs.String("invalidate", "", "Remove every charly-labeled image tag matching this glob (full ref or last path segment); runs ONLY the invalidation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	tag := "removed"
	if *dryRun {
		tag = "would remove"
	}

	// --invalidate: targeted image-tag invalidation ONLY. Needs no keep-defaults.
	if *invalidate != "" {
		reply := runRetention(spec.RetentionRequest{Dir: dir, DryRun: *dryRun, Invalidate: *invalidate})
		if reply.Error != "" {
			return fmt.Errorf("%s", reply.Error)
		}
		fmt.Printf("invalidate: %s %d tag(s) matching %q\n", tag, len(reply.ImageRefs), *invalidate)
		for _, r := range reply.ImageRefs {
			fmt.Printf("  %s\n", r)
		}
		return nil
	}

	doImages, doCheck, doDeep, doCache := cleanCategories(*images, *check, *deep, *cacheGC)

	// The cache category needs NO resolved keep-defaults and NO engine binary (it
	// GCs the content-addressed ArtifactStore's own blobs, bounded by each store's
	// own cap), so it runs even out-of-process. Only images/check/deep need the
	// loader-resolved defaults.
	var keepImages, keepCheck int
	if doImages || doCheck || doDeep {
		var derr error
		keepImages, keepCheck, derr = resolveRetentionDefaults(ctx, exec, dir)
		if derr != nil {
			return derr
		}
	}
	if doImages || doCheck || doDeep || doCache {
		out := runRetentionOutcome(spec.RetentionRequest{
			Dir: dir, DryRun: *dryRun, Images: doImages, Check: doCheck, Deep: doDeep, Cache: doCache,
			Keep: *keep, KeepImages: keepImages, KeepCheckRuns: keepCheck,
		})
		if out.Reply.Error != "" {
			return fmt.Errorf("%s", out.Reply.Error)
		}
		if perr := printRetentionResult(os.Stdout, tag, doImages, doCheck, doDeep, doCache, out); perr != nil {
			return perr
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
func printRetentionResult(w io.Writer, tag string, doImages, doCheck, doDeep, doCache bool, out retentionOutcome) error {
	reply := out.Reply
	var lines []string
	if doImages {
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
	if doCheck {
		lines = append(lines, fmt.Sprintf("check: %s %d run artifact(s) (keep_check_runs=%d, NOTES.md preserved)\n", tag, len(reply.CheckPaths), reply.KeepCheckRuns))
		for _, p := range reply.CheckPaths {
			lines = append(lines, fmt.Sprintf("  %s\n", p))
		}
	}
	if doDeep {
		count := fmt.Sprintf("%s %d untagged image(s) store-wide (up to %s reclaimable — shared layers may reduce actual reclaim; pair with --invalidate for the fullest reclaim)", tag, len(reply.DeepIDs), kit.HumanBytes(reply.DeepBytes))
		lines = append(lines, sweepLines("deep", out.DeepSkip, count, reply.DeepIDs)...)
	}
	if doCache {
		if len(reply.CacheStores) == 0 {
			lines = append(lines, "cache: no named cache stores found\n")
		}
		for _, s := range reply.CacheStores {
			lines = append(lines, fmt.Sprintf("cache %s: %d live entry(ies); %s %d unreferenced blob(s) (%s)\n",
				s.Name, s.Entries, tag, s.RemovedBlobs, kit.HumanBytes(s.RemovedBytes)))
		}
	}
	if _, err := fmt.Fprint(w, strings.Join(lines, "")); err != nil {
		return fmt.Errorf("printing the clean report: %w", err)
	}
	return nil
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

// cleanCategories resolves the --images/--check/--deep flags into which categories run this
// invocation. --images and --check keep their pre-existing "only this" semantics (any one given
// alone suppresses the other default categories). --deep joins that same "explicit category" gate
// but NEVER fires implicitly: on a plain `charly clean` (no flags at all) doDeep is always false —
// the store-wide untagged-image sweep is a strictly broader operation than the default
// per-charly-labeled retention (it can remove far more, and scans the whole local store), so it
// stays opt-in-only and the default `charly clean` behavior is unchanged. Passing --deep alone runs
// ONLY the deep category (mirroring --invalidate's "runs ONLY this"); combine it with
// --images/--check to run more than one category in one invocation.
func cleanCategories(images, check, deep, cacheGC bool) (doImages, doCheck, doDeep, doCache bool) {
	anyCategory := images || check || deep || cacheGC
	doImages = images || !anyCategory
	doCheck = check || !anyCategory
	doDeep = deep
	doCache = cacheGC
	return doImages, doCheck, doDeep, doCache
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
