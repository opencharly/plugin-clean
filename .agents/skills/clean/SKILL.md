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

`charly clean` is a **compiled-in COMMAND-class plugin** (`candy/plugin-clean`, `command:clean`) that OWNS the command AND (K1-alpha core-minimization) the **shared retention ENGINE** itself — `pruneImagesByRetention`, `pruneCheckRuns`, `pruneBuildCandyDirs`, `invalidateImageTags`, `pruneDeepDanglingImages`, and the charly-labeled image-tag CalVer/label inventory (`candy/plugin-clean/retention.go`), relocated from the former `charly/retention.go` since it has zero core-only dependencies (`kit.CalVer`/`kit.ListLocalImages`/`kit.BuildActivityDir` are all sdk-portable). The plugin owns the flag grammar (`--dry-run` / `--images` / `--check` / `--deep` / `--cache` / `--scopes` / `--build-cache` / `--store` / `--keep` / `--invalidate`) — DERIVED from the declared category set in `schema/clean.cue` — the category orchestration, and the report output. `charly clean`'s own CLI calls the engine LOCALLY (no wire hop, same package). The other three callers — `charly box build`'s post-build prune, `charly box list tags`, and `charly check run`'s post-run prune (candy/plugin-check) — reach it via a new `verb:retention` capability (a `spec.RetentionRequest` → `spec.RetentionReply` Invoke), the SAME peer/core-adapter pattern verb:credential/verb:gpu/verb:tunnel already use: core's two callers (already running `LoadConfig` in-process) resolve `defaults.keep_images`/`keep_check_runs` themselves and pass the resolved ints in the request; plugin-check (a peer plugin, not core) reaches verb:retention via `InvokeProvider`. The SAME defaults, for its OWN CLI, resolve PLUGIN-SIDE via the shared `sdk/loaderkit.ResolveRetentionDefaultsViaExecutor` (K-wave 2 cone R6 — the former **"retention-defaults" `HostBuild` seam** `charly/host_build_retention_defaults.go` is DELETED: the loader is plugin-reachable over the reverse channel, so every `verb:retention` caller resolves the tunables itself). clean is **compiled-in** (`charly/charly.yml` `compiled_plugins:`) because its `Invoke(OpRun)` needs the in-proc reverse channel — threaded by `dispatchInProcCommand` ("Seam A") — to reach the host loader legs for that resolve; the out-of-process `CliMain` path has no reverse channel, so the categories needing a resolved keep-default (images/check/deep) error there — list/invalidate need no default and run standalone; `--cache`, `--scopes` and `--build-cache` need neither the defaults nor the container engine, so they run there too (and on a host whose podman is broken); `--store` needs BOTH the container engine (one of its state surfaces) and the user configuration, which it reads over the reverse channel — so it requires compiled-in placement too, and the out-of-process CLI REFUSES rather than assume the plugin's 32 GiB fallback. This is the same "plugin owns the logic + generic seams for the core-coupled bits" doctrine the vm + pod deploy plugins established — no hidden core-command forward, and no plugin-specific command logic left in core.

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
charly clean --build-cache        # reclaim the builder's persistent cache — the daemon's state
                                  # root (<cache>/buildkitd) when no daemon owns it, plus every
                                  # quarantined sibling (buildkitd.broken-*); bytes per root
charly clean --build-cache --dry-run  # the same report; removes nothing, and does not even
                                      # create the daemon's lock file to ask who owns the root
charly clean --store      # D-STORE-7: evict the charly image store entries the HOST's charly
                          # state no longer references (engine containers + quadlet units), free
                          # the blobs no surviving entry reaches, and report the retained size
                          # against the AUTHORED budget; REFUSES when a state surface cannot be read
charly clean --store --dry-run   # the same decision, entry by entry, with the reason for each;
                                 # touches nothing
```

With **none** of `--images` / `--check` / `--deep` / `--cache` / `--scopes` / `--build-cache` /
`--store`, the **four** DEFAULT categories run (image-tag + check-run + build-candy + scopes);
naming ANY category flag suppresses that default set and runs only what was named (plus, for
`--deep`, the scope reap below). `--scopes` IS in the default set because it is itself a category
flag, and `--deep` and `--cache` and `--build-cache` and `--store` NEVER fire implicitly — for
`--store` that is a safety property, not a preference: it is the only category that deletes
ARTIFACTS.
**The discipline is DECLARED, not coded.** The category set lives in
`candy/plugin-clean/schema/clean.cue` (`#CleanPlugin.categories`: one row per category, with its flag
word, its one-line contract — which is also the flag's help text — `bare:` = runs on an unqualified
`charly clean`, and `implies:` = the categories naming this one also runs). `cleanCategories`
(categories.go) derives the run set from those rows: named → runs; a running category also runs the
ones it `implies:`, transitively; `bare: true` runs when NO category was named. So `--deep` reaps
build scopes because `deep` declares `implies: ["scopes"]`, and a bare `charly clean` reaps them
because `scopes` declares `bare: true` — a new category is a row, not an edit to a hand-written
`anyCategory := …` chain.
`--keep N` overrides both the image and check-run retention counts for the invocation (it does
not affect `--deep`, which has no keep-N — it purges every untagged image).
**Category names, from the engine's own header** (`retention.go`): *image-tag / build-candy / check-run
pruning*, plus the `--deep` store-wide purge, the `--cache` blob sweep and the `--store` entry
eviction. The middle one prunes
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

**`--build-cache` — the builder's persistent cache (opencharly/plugin-clean#23).** `sdk/buildkitdrive`
owns the buildkitd daemon's persistent `--root` at `CacheDir()/buildkitd` (`StateRoot`). It is NOT one of
charly's stores: a store tree IS an OCI Image Layout and nothing else (D-STORE-3) — it carries no
`oci-layout` marker and it MUST carry a `buildkitd.lock` — so none of the other categories can see it
(`--images` prunes the ENGINE's storage, `--check` sweeps `.check/` run dirs, `--deep` purges dangling
engine images, `--cache` walks the OCI-layout stores under the cache root, `--scopes` reaps systemd build
scopes). Measured on a real host: the live root held 50 GiB and a quarantined sibling
(`buildkitd.broken-1794801825`) held 105 GiB that could be removed only by hand with `sudo rm -rf`
after 301 708 container-owned files refused to go — and nothing in the tree emitted that `.broken-*`
name. This category is that path, with an owner.

Two instruments, one category:

- **The live root** — removed only when no daemon owns it. A running `buildkitd` holds an exclusive
  flock on `<root>/buildkitd.lock` for its whole life (that lock is how BuildKit enforces one daemon per
  root), so the lock IS the holder's liveness signal. The sweep SEIZES the lock before it touches the
  root — so no daemon can start halfway through — and refuses, naming the daemon by its lock and its
  socket, when it cannot acquire it. A lock dies with its holder (D-STORE-7), so a store left by a
  crashed daemon is reclaimable without asking anyone.
- **Quarantined siblings** — every `<state-root>.broken-*` directory beside it is reclaimed
  UNCONDITIONALLY: a store is only quarantined because it was abandoned, so there is nothing to race.
  This plugin does not CREATE that name (quarantining is a recovery decision for the store's owner);
  it recognizes it, which is what turns an improvised directory into a shape with an owner.

Bytes are reported PER ROOT (the live root carries its own line; the quarantined roots share a count
line plus one status line each), `--dry-run` reports the SAME figures and touches nothing (it does not
even create the lock file to ask who owns the root), and the category never fires on a bare
`charly clean`. A tree this user cannot delete WHOLE — a rootless build leaves its store owned by
container-mapped UIDs, the measured case — is reported BY NAME with the blocking path and the uid that
owns it and left EXACTLY as it was: the walk that measures a root also decides whether a whole-tree
removal is possible, so the sweep never half-deletes a store and never claims a removal it did not
perform. A directory the user OWNS but cannot write is not a blocker (the owner may relax the mode);
an empty read-only directory is not a blocker either (rmdir needs write on the PARENT, not on the
directory itself). Both paths come from `sdk/buildkitdrive`; nothing here builds a `<cache>/buildkitd`
path by hand, and the sweep needs neither the container engine nor the resolved keep-defaults — so it
works on a host whose podman is missing or broken.

## The `--store` category — `D-STORE-7`, the image store's ENTRY policy

`DESIGN.md:2488` (`D-STORE-7`): *"`charly clean` keeps what state references and evicts the rest
within a size budget; locks die with their holder."* `--store` implements that row for the charly
image store — the `registry` NAMED store under the cache root, whose entries are the artifacts every
build APPENDS (a rebuild produces a different digest, so it is a different key beside the old one,
never a replacement). Nothing else ever removed an entry, so a store measured at 283 GiB / 172
entries on the host this was written on grew by one artifact per rebuild forever.

**It is a REFERENCE rule, never a recency one.** `keep_images`' "newest N" would delete a running
deployment's image if it happened to be the fourth; `--store` asks what charly's own state still
names, from two surfaces read through shared resolvers:

| Surface | What it is | Why it is in the inventory |
|---|---|---|
| the container engine (`podman ps -a`) | every container, running or stopped: its image ref and its image ID | the runtime truth a deploy actually runs — the surface `charly status --json` publishes as `image_ref` |
| the quadlet units (`$HOME/.config/containers/systemd/charly-*.container`, `Image=`) | the unit charly wrote for each pod deploy | the ONLY record of an ENABLED deploy whose container does not exist yet (`status --all`'s `enabled` rows carry no `image_ref`) |

Each state reference is resolved to an entry through **`ImageStore.Lookup`** — the store's own
"the newest entry whose annotation names this reference" — because state names a REFERENCE while
the store keys on the artifact's DIGEST. That resolution is also what makes a *superseded rebuild of
a referenced tag* reclaimable: when the engine still runs `vision-gate` and the store holds five
entries under that annotation, the name resolves to the newest and the four older ones are evicted
with the reason `superseded: … the store resolves it to <digest>`. An entry whose own content
address (digest) a container or a pin names is kept outright. An entry carrying no
`io.containerd.image.name` annotation is kept as `unclassifiable`, because the only verdict that
cannot be wrong is to leave it.

**It refuses rather than assume.** An unreadable state surface is NOT an empty one: "nothing is
referenced" is how a sweep deletes the image a running deployment was built from. So a failed
`podman ps`, a quadlet that cannot be read, a charly quadlet with no `Image=` line, an index that
disagrees with its own entries, or a store that cannot be enumerated **aborts the whole sweep** with
an error naming the surface and the path, and evicts nothing. `--dry-run` refuses on exactly the
same inputs — a dry run that predicted an eviction the real run then refused would be a lie about
the plan.

**The ceiling is the MACHINE's, and it never authorises evicting live state.** A machine authors it in
its USER CONFIGURATION — `~/.config/charly/charly.yml`, the per-host file `D-STORE-2` makes the home for
what the user sets — as a declared section beside `cache:`/`ledger:`/`deploy:`/`system:`:

```yaml
clean:
  store_budget:
    bytes: 274877906944      # 256 GiB
```

`bytes` is `int & >0`, projected as `int64` (256 GiB does not fit a 32-bit `int`; the wrap on 386/arm
would be silent). The plugin's own `schema/clean.cue` `#CleanPlugin.store_budget.bytes` (32 GiB) is the
FALLBACK for a machine that authors nothing, and the report names which applied: `ceiling: user config`
vs `ceiling: plugin default`. Three states, never a sentinel: absent (nil at every level) → the
fallback; authored positive → that number; unreadable or authored non-positive → REFUSE, naming the
file, because a parse failure must never evict a machine's builds under a ceiling it never wrote.

The sweep reports the store's RETAINED bytes against the ceiling and says `within budget` or `OVER
budget by <bytes>`; being over budget is REPORTED, never "fixed", because the entries left are the ones
state references.

**What a dry run can and cannot say.** The DECISION is identical in both modes (same entries, same
reasons, tested by mutation). The BYTE figure is not: a dry run measures the store as it is (`N
held of <budget>`) plus the blobs already unreferenced, while the retained size after eviction needs
the eviction — the store's public surface cannot size a hypothetical entry set without deleting it,
and a number that looked like the post-sweep size while being the pre-sweep one is exactly the kind
of figure this report exists to avoid.

A live build makes it **decline**: an artifact's blobs are published BEFORE its entry, so a sweep
between the two would free bytes an in-flight entry has not indexed yet.

**Out of scope, stated rather than guessed:** DECLARED configuration is not state. A `kind: vm`
whose disk was built from a container image is the one place a store artifact is needed without any
host record naming it (opencharly/plugin-vm#98 carries that gap).

## The retired roots — and the pre-relocation image store

`--cache` also disposes of the store roots the migration RECORD retires. One of them is not a cache
entry at all: when `opencharly/sdk#556` moved the image store out of W3.6's data root
(`$XDG_DATA_HOME/charly/images`) into the cache root's `registry` store, it moved the reader and the
writer and left the CONTENT behind — hundreds of GiB on a host with build history, at a path nothing
resolved any more (opencharly/plugin-clean#30).

That root is **ingested, never removed**: every entry is re-published into `registry` (its blobs
hardlinked where the trees share a filesystem, copied otherwise — reported either way; its descriptor
re-keyed into the current entry shape), every entry and every reference is VERIFIED in the
destination, and only then is the source tree removed. There is no keep set, so no state surface can
make it unsafe; the refusal points are the bytes it moves (an unreadable index, an entry with no
reference, a manifest the tree does not hold) and each names itself. `charly clean --store` then
applies D-STORE-7 to the ingested content: it keeps what state references and evicts the rest within
the authored budget.

There is no data-root RESOLVER, and this does not add one: `D-STORE-1` (`DESIGN.md:2482`) declares
four roots and data is not among them (`spec/storeroot`'s `RootData` was reverted, spec#375), so the
pre-relocation path is a RECORDED FACT in the retirement record — spelled once, expanded by one
function that resolves no store and feeds no reader.

## The `--cache` category's routing — one reader per store, chosen by IDENTITY

`--cache` sweeps every named store's unreferenced blobs. It used to run in two halves — `spec/cache`
layouts by name, and `storekit` stores by name — and to SKIP the storekit half whenever the two
roots resolved to the same directory. `CHARLY_CACHE_DIR` is exactly that case (it is the one
override `D-STORE-1` blesses, and how a bed isolates its cache), so on those hosts the
storekit-written `registry` store was handed to `spec/cache`'s IMAGE-BLIND `GCStats`: it deleted a
live image's manifest, config and layers and left the entry answering `Lookup(ref)` for bytes that
were gone (opencharly/plugin-clean#27). The mirror mistake was available too — storekit's reader
would have decoded `spec/cache` manifests as empty entries and freed their payloads.

The reader is now chosen from the STORE'S OWN INDEX (`classifyStore`): every descriptor an
`application/vnd.opencharly.store.entry.v1+json` entry manifest → storekit; every descriptor an OCI
image manifest → `spec/cache`. The two enumerations are deduped into ONE inventory by resolved
directory, so no store is skipped and none is swept twice; a store whose index mixes media types, or
cannot be read, is REFUSED and reported by name. Because the image store is never opened by
`spec/cache` any more, no `.lock` is written inside its tree either (`D-STORE-3`, `DESIGN.md:2484`).

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
ENGINE: `schema/clean.cue` (the DECLARED category set — `#CleanPlugin.categories`, one row per
category with its flag, contract, `bare:` and `implies:`; the CLI grammar is derived from it),
`categories.go` (decodes that declaration from the SAME embedded schema `NewMeta` serves, and
resolves it into a `cleanPlan` — the `bare:`/`implies:` discipline as data, no `anyCategory := …`
chain in Go), `command.go` (the flag grammar registered FROM the declaration, `cleanCategories`,
and the per-root report),
`buildcache.go` (the `--build-cache` category: the live root's flock guard, the quarantined-sibling
discovery, and the measure-then-refuse-or-remove walk),
`store_eviction.go` (the `--store` category: `readStoreStateInventory` — the engine + quadlet
surfaces and their refusals, `planStoreEviction` — the pure per-entry verdict, `runStoreSweep` — the
live-build decline, the eviction, the reclaim and the budget verdict, plus `storeBlobBytes`),
`store_identity.go` (`classifyStore` — the reader chosen from the store's own index,
`discoverStores` — the deduped inventory, `sweepStoreBlobs` — the ONE per-store reclaim both
`--cache` and `--store` call),
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
