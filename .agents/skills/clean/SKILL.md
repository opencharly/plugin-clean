---
name: clean
description: |-
  Prune reusable build artifacts to defaults: retention (images, check runs) and sweep one-time build-candy leftovers — `charly clean`, keep_images/keep_check_runs, image-tag pruning, .check run cleanup.
---

# charly clean -- Build-artifact retention + cleanup

## Overview

`charly clean` reclaims disk by applying the project's configured retention to
**reusable** build artifacts and removing **one-time** transient leftovers. It is
the on-demand counterpart to the auto-pruning that runs after `charly box build`
and `charly check run`.

`charly clean` is a **compiled-in COMMAND-class plugin** (`candy/plugin-clean`, `command:clean`) that OWNS the command AND (K1-alpha core-minimization) the **shared retention ENGINE** itself — `pruneImagesByRetention`, `pruneCheckRuns`, `pruneBuildCandyDirs`, `invalidateImageTags`, `pruneDeepDanglingImages`, and the charly-labeled image-tag CalVer/label inventory (`candy/plugin-clean/retention.go`), relocated from the former `charly/retention.go` since it has zero core-only dependencies (`kit.CalVer`/`kit.ListLocalImages`/`kit.BuildActivityDir` are all sdk-portable). The plugin owns the flag grammar (`--dry-run` / `--images` / `--check` / `--deep` / `--keep` / `--invalidate`), the category orchestration, and the report output. `charly clean`'s own CLI calls the engine LOCALLY (no wire hop, same package). The other three callers — `charly box build`'s post-build prune, `charly box list tags`, and `charly check run`'s post-run prune (candy/plugin-check) — reach it via a new `verb:retention` capability (a `spec.RetentionRequest` → `spec.RetentionReply` Invoke), the SAME peer/core-adapter pattern verb:credential/verb:gpu/verb:tunnel already use: core's two callers (already running `LoadConfig` in-process) resolve `defaults.keep_images`/`keep_check_runs` themselves and pass the resolved ints in the request; plugin-check (a peer plugin, not core) reaches verb:retention via `InvokeProvider`. The SAME defaults, for its OWN CLI, resolve PLUGIN-SIDE via the shared `sdk/loaderkit.ResolveRetentionDefaultsViaExecutor` (K-wave 2 cone R6 — the former **"retention-defaults" `HostBuild` seam** `charly/host_build_retention_defaults.go` is DELETED: the loader is plugin-reachable over the reverse channel, so every `verb:retention` caller resolves the tunables itself). clean is **compiled-in** (`charly/charly.yml` `compiled_plugins:`) because its `Invoke(OpRun)` needs the in-proc reverse channel — threaded by `dispatchInProcCommand` ("Seam A") — to reach the host loader legs for that resolve; the out-of-process `CliMain` path has no reverse channel, so the categories needing a resolved keep-default (images/check/deep) error there — list/invalidate need no default and run standalone. This is the same "plugin owns the logic + generic seams for the core-coupled bits" doctrine the vm + pod deploy plugins established — no hidden core-command forward, and no plugin-specific command logic left in core.

Two artifact classes, two policies (operator principle):

- **Reusable → keep-last-N, configurable in `defaults:`.** Container image tags
  and `.check` run output. Retention is set in `charly.yml` `defaults:` and
  applied automatically at creation; `charly clean` applies it on demand.

## Config (charly.yml `defaults:`)

```yaml
defaults:
  keep_images: 3      # per box: newest 3 distinct images, and at most 3 tags
                      # of each, after `charly box build`
  keep_check_runs: 3   # newest run dirs to keep per bed/score after `charly check run`
```

`0` (or absent → built-in fallback `0`) **disables** that retention. The repo
opts in; third-party configs get no surprise pruning until they set a value.

## Commands

```bash
charly clean              # apply retention now: prune images + check runs + build-candy leftovers
charly clean --dry-run    # print everything that WOULD be removed; touch nothing
charly clean --images     # only image-tag retention
charly clean --check       # only check-run retention
charly clean --deep        # store-wide untagged/dangling-image purge (see below); runs ONLY
                           # this category unless combined with --images/--check
charly clean --deep --dry-run   # the safe default probe: report the would-remove count +
                                # an UPPER-BOUND reclaimable-bytes figure for --deep; touch nothing
charly clean --keep N     # override the retention count for this run (0 = use defaults:)
charly clean --invalidate '<glob>'   # remove charly-labeled image tags matching the glob
                                     # (full ref or last segment; in-use skipped; runs ONLY this)
charly clean --cache      # reclaim unreferenced blobs from every named charly cache (the
                          # content-addressed ArtifactStore's own GC); reports each store's live
                          # entries + the blobs/bytes it would reclaim; runs ONLY this category
charly clean --scopes     # reap HUNG transient build scopes — the systemd user scopes
                          # buildah/podman put a build container into (runc-buildah-*.scope).
                          # A scope is stopped only when it has hung; a live one is left alone
```

With **none** of `--images` / `--check` / `--deep` / `--cache` / `--scopes`, the **four** DEFAULT categories
run (image-tag + check-run + build-candy + scopes) — unchanged, and `--deep` and `--cache` NEVER fire
implicitly. `--scopes` IS in that list because it is itself a category flag: the test the code applies is
`anyCategory := images || check || deep || cacheGC || scopes`, so naming ANY one of the five suppresses the
default set and runs only what was named (plus, for `--deep`, the scope reap below).
**`--scopes` is a default and `--deep` implies it:** `cleanCategories` computes
`doScopes = scopes || deep || !anyCategory`, so a bare `charly clean` reaps hung build scopes
and `--deep` does too, while `--deep` on its own runs only the image half plus that scope reap — and
`--cache` on its own runs only the cache sweep, with `doScopes` false.
`--keep N` overrides both the image and check-run retention counts for the invocation (it does
not affect `--deep`, which has no keep-N — it purges every untagged image).
**Category names, from the engine's own header** (`retention.go`): *image-tag / build-candy / check-run
pruning*, plus the `--deep` store-wide purge and the `--cache` blob sweep. The middle one prunes
`.build/_candy/<candy>.<version>/` per candy (`pruneBuildCandyDirs`).

**`--deep` — the store-wide untagged/dangling-image purge.** The default image-tag sweep above
only ever touches images carrying the `ai.opencharly.box` label. A multi-stage build's
INTERMEDIATE stage images (`FROM ... AS stagename`) are never labeled — `WriteLabels` stamps the
`ai.opencharly.*` labels only on the FINAL stage — so they accumulate as unlabeled dangling
images completely invisible to the default sweep, wasting disk and confounding build-corruption
diagnosis on a host with many builds behind it. `charly clean --deep` closes this gap: it lists
EVERY untagged (dangling) image in local storage, charly-labeled or not, and removes each one
(`rmi` without `-f`, so any image still referenced by a container or a kept tag is safely
skipped — the same backstop the default sweep relies on). It never runs mid-build (the same
live-build guard the default dangling sweep uses) and never fires implicitly on a plain `charly
clean` — it is strictly opt-in. Removing a dangling image also frees any layer blobs it alone
held (the engine's overlay storage GCs an unreferenced layer once its last referencing image is
gone), so `--deep` is effectively a dangling-image-plus-unused-layer prune in one pass.
`--deep --dry-run` reports the would-remove image count plus an UPPER-BOUND reclaimable-bytes
figure (summed from each candidate's reported storage size) without touching anything — the safe
default probe before running it for real.

**The reclaimable-bytes figure is "up to", never a firm prediction.** Each image's reported
storage size counts EVERY layer it references, and dangling images routinely SHARE layers with
images that stay (retained tags, other dangling images) — removing an image frees only the
layers it held UNIQUELY, so actual disk freed is usually much less than the naive per-image sum.
RDD-verified live: a `--deep` purge removing 68 untagged images (3,552 → 3,484 images) reported
~92.6 GiB via this sum but freed only ~4.6 GiB of real disk (132.6 GB → 128 GB) — most of those
bytes stayed shared with the ~3,400 remaining (largely stale-tagged) images. Pair `--deep` with
`--invalidate` (which removes stale image TAGS, freeing whatever layers only they still held) to
get closer to the reported figure and reclaim more of the store.

## What gets pruned (and what never does)

**Image-tag retention** (`keep_images`): images are grouped by the
`ai.opencharly.box` label and ordered by the `ai.opencharly.version` CalVer label
**when both rows carry one**, then a row carrying that label before one that does
not, then **image creation time**, and only then the build tag.

**Which key decides depends on which ordinal you are asking about**, and this page owns
that fact. **Between distinct images**, the label decides whenever the two carry
DIFFERENT datable labels — it is the primary key. Creation time decides only when the
labels tie, which is the common case that motivates it — and the reason is that the
label is **DECLARED, never derived from content**. `ai.opencharly.version` is the
image's own `version:` if it has one, else the HIGHEST candy `version:` across its
entire candy set (own plus base chain), else the internal base's own effective version,
else a hard error — there is no content hash and no build-timestamp fallback
(`sdk/deploykit/effective_version.go`). Two images built from genuinely different trees
therefore carry the SAME label until somebody bumps one of those fields, so ties between
DISTINCT images are the norm rather than the exception and creation time is what
actually orders them. Observed: four distinct `fedora-nonfree` images, built hours
apart from different trees, all labelled `2026.227.0830`. **Within one image's tag
rows, the build tag decides** — every row of
one image shares its label, its labelled-ness and its creation time by construction
(`charlyImageTags` hoists them out of the per-ref loop), so the first three keys tie and
the `:YYYY.DDD.HHMM` tag is the only one left to break it. So *"creation time, not the
tag"* is true of the image budget and **false of the tag budget**.

Why the tag cannot serve as the *image* recency key: `charly box build --tag` REPLACES
the CalVer tag, so a bed build carries `check-<bed>-<calver>`, which parses as no CalVer
at all. Ordering distinct images on the tag made every member of such a group compare
equal, so `keep_images: N` kept an arbitrary N and could delete the newest build.
Creation time is the only recency key total over the tags charly mints — and it is
exactly the key that ties within one image, which is why the tag still ranks that image's
own rows.

`keep_images: N` budgets **two ordinals**: the newest N **distinct images** per group
survive — a *distinct image* being a distinct image ID, so every tag pointing at one ID
counts once — **and at most N tags of each, the same N** — so one image wearing many
CalVer tags no longer consumes the whole budget, starving the distinct images behind it,
while a content-stable image rebuilt many times still has its surplus tag rows reclaimed.

**Within an image, the newest tags survive and the oldest are dropped** — newest by
build-tag CalVer, since that is the only key that varies across one image's rows.
**Tags that carry no CalVer — `latest`, `dev`, a bed's `check-<bed>-<calver>` — all tie
on that key**, and a stable sort then keeps whatever order the engine listed them in. So
which of several undatable tags survives the tag budget is NOT specified, and if the
image itself carries a datable `ai.opencharly.version` label those rows are **not**
protected by the undatable-row exemption (that guard needs BOTH to be undatable). Do not
rely on `keep_images` to preserve a `latest` or a hand-applied tag on a
frequently-rebuilt image — pin it with a full ref, or keep it on an image that retention
does not group.

**Safety**: any image referenced by a container (`podman ps -a`, including
stopped/quadlet deploys) is skipped, and `rmi` runs WITHOUT `-f` so the engine refuses
any still-referenced image as a backstop. Non-charly images (no `ai.opencharly.box`
label) are never touched, and a tag row is exempted only when it has NEITHER a datable
`ai.opencharly.version` label NOR a datable `:YYYY.DDD.HHMM` tag — the guard is an AND,
so a row with either one is a normal candidate. A bed build (datable label, non-CalVer
tag) is therefore prunable, not exempt.

**Check-run retention** (`keep_check_runs`): each `.check/<bed|score>/` dir is
trimmed to the newest N run artifacts — CalVer-named run dirs (bed runs),
`runs/<id>/` dirs (score iterations), and `result-<calver>.yml` files.
**`NOTES.md` is ALWAYS preserved** (it's the durable Syncthing-replicated harness
memory), as is any other non-run file.

`--dry-run` is best-effort: it lists prune candidates but cannot see "external"
build containers (buildah intermediates `podman ps -a` doesn't list), so an
image held by one is listed yet safely skipped at removal time (the `rmi`
backstop refuses it). The real run silently retains such in-use images.

## Auto-prune at creation

The same retention runs automatically (no flag needed):

- After `charly box build` (push runs excluded) → `keep_images`.
- After `charly check run` (any path: bed / score) → `keep_check_runs`,
  after the new run's output is written so the newest run is kept.

**The post-build pass reclaims tags, never whole images.** It runs while the build
still holds its own build-activity lock, so it always observes a live build, and the
live-build path never removes an image's LAST tag — the guard that protects a
concurrent build's base. A build therefore trims surplus tag rows from images that
wear several and leaves every single-tagged image in place, so the **distinct-image**
half of `keep_images` is not enforced by building. Nothing reports this: the summary
counts what it removed, not what it selected and could not remove.

`charly clean` exists for on-demand sweeps and to clear a pre-existing backlog — and
it is the only verb that reclaims a distinct image, so a project that only ever builds
accumulates them past `keep_images` indefinitely.

## Out of scope

VM disk images (`output/`, `image/*/output/`) are single products per type
(overwritten on rebuild, not accumulated) — remove them on demand with
`charly vm destroy --disk`. The VM raw intermediate is already auto-cleaned during
the qcow2 build.

## Implementation

`candy/plugin-clean/` — the command plugin that OWNS `charly clean` AND the shared retention
ENGINE: `command.go` (flag grammar + category orchestration via `cleanCategories` +
`cleanMakepkgArtifacts` + `fetchRetentionDefaults`, which calls the one remaining host seam),
`provider.go` (`Invoke`, dispatching by word — `"clean"` for the CLI, `"retention"` for the
engine — the compiled-in dispatch surface for both), `plugin.go` (`NewProvider` / `NewMeta`
advertising `command:clean` + `verb:retention` / `CliMain`), `retention.go` (the relocated
engine: `pruneImagesByRetention`, `pruneCheckRuns`, `pruneBuildCandyDirs`, `invalidateImageTags`,
`pruneDeepDanglingImages`, and the `charlyImageTags` inventory — all now in this package,
importing only `sdk/kit` + `spec/spec`). `--deep` shares its engine with the default
charly-labeled dangling sweep via `pruneDanglingImages`/`selectDanglingImages`
(`candy/plugin-clean/retention.go`) parameterized by an `onlyCharly` bool: `pruneDanglingCharlyImages`
(onlyCharly=true, the default sweep) and `pruneDeepDanglingImages` (onlyCharly=false, `--deep`)
are both thin wrappers over the ONE shared selection + removal engine (R2 — no duplicated
listing/removal logic between the two categories). The engine is reached two ways: `charly
clean`'s own CLI calls `runRetention` in-package (no wire hop); the three OTHER callers reach
`verb:retention` — the build command's post-build prune (`candy/plugin-box/box.go`'s
`pruneAfterBuild`, resolving `defaults.keep_images` itself and passing it pre-resolved) and
`charly box list tags` (`candy/plugin-box`'s `listImageTags` via `verb:retention`) resolve+Invoke
the compiled-in provider directly (`providerRegistry.resolve
(ClassVerb, "retention")`); `charly check run`'s post-run prune (the `command:check` plugin's
harness, `candy/plugin-check`) reaches it over the PLUGIN↔PLUGIN `InvokeProvider` peer-dispatch
leg (F10) instead, since it is itself a plugin and cannot resolve the core registry directly.
Both non-core-adapter callers resolve `defaults.keep_images`/`keep_check_runs`
PLUGIN-SIDE via the shared `sdk/loaderkit.ResolveRetentionDefaultsViaExecutor`
(K-wave 2 cone R6 — the former "retention-defaults" `HostBuild` seam
`charly/host_build_retention_defaults.go` is DELETED), reached over the compiled-in in-proc
reverse channel threaded by `dispatchInProcCommand` (`charly/provider_command_external.go`).
Retention keys live on `spec.BoxConfig` (CUE-sourced), merged via `mergeBoxConfig`
(`spec/spec/merge_unified.go`), validated in `candy/plugin-box/validate_config_rules.go`
(the `>= 0` keep rule — the former core `validateBuildTunables`/`validate.go` are DELETED,
K-wave 2 cone R1).

## Cross-References

- `/charly-build:build` — `charly box build` + the `keep_images` auto-prune.
- `/charly-check:check` — `charly check run` + the `keep_check_runs` auto-prune.
- `/charly-vm:vm` — `charly vm destroy --disk` for VM disk removal.
- `/charly-image:image` — the `defaults:` block where retention keys live.
