# plugin-clean

The `charly clean` build-artifact retention/prune surface for OpenCharly — served
as a charly `command:clean` plugin (compiled-in), plus the shared retention
engine as `verb:retention`.

The plugin owns the flag grammar, the category orchestration, the report output,
AND the retention engine itself (`retention.go`). Peer plugins
(`candy/plugin-box`'s post-build prune / `box list tags`, `candy/plugin-check`'s
post-run prune) reach the engine over `verb:retention`.

## What it provides

| Capability | Surface |
|---|---|
| `command:clean` | the `charly clean` CLI — `--dry-run`, `--images`, `--check`, `--deep`, `--cache`, `--scopes`, `--build-cache`, `--store`, `--keep`, `--invalidate` |
| `verb:retention` | the shared retention engine, invoked by peer plugins |

## The command

- **default** — the charly-labeled dangling-image sweep.
- **`--images`** — prune images by the retention policy (`keep_images`).
- **`--check`** — prune `.check` runs (`keep_check_runs`).
- **`--cache`** — the CAS `ArtifactStore` GC. The reader for each store is chosen from the
  store's OWN index (storekit entry manifests → storekit, OCI image manifests → `spec/cache`), never
  from the root it sits under: the image store must never be swept by the image-blind reader
  (opencharly/plugin-clean#27), and a store that cannot be classified is refused and reported.
- **`--store`** — `D-STORE-7`: keep the charly image store's entries that the host's charly state
  still references (the container engine's containers and the quadlet units), evict the rest, free
  the blobs no surviving entry reaches, and report the retained size against the AUTHORED budget
  (`schema/clean.cue` `store_budget`). It REFUSES — evicting nothing — when a state surface cannot
  be read, because an unreadable surface is not an empty one. Strictly opt-in.
- **`--scopes`** — reap HUNG transient build scopes: the systemd user scopes
  buildah/podman put a build container into (`runc-buildah-*.scope`). A scope is
  stopped only when it has been active for at least an hour AND no
  `buildah`/`podman` builder process is alive anywhere on the host, so a running
  build is never touched. This is the one category that also runs on a plain
  `charly clean` and with `--deep`: a scope whose builder died keeps spinning
  forever (one measured live at seven days, 12.27 load on 16 cores) and nothing
  else reaps it — the run metadata that `charly reap-orphans` scans is gone with
  the worktree. Any other explicit category suppresses it.
- **`--deep`** — the store-wide untagged/dangling-image purge (the CLI-only
  gap-closing capability). Strictly opt-in; never fires on a plain `charly
  clean`. `--deep --dry-run` reports the would-remove count + an UPPER-BOUND
  reclaimable-bytes figure and touches nothing.
- **`--invalidate`** — remove stale image TAGS (freeing their exclusively-held
  layers).
- **`--build-cache`** — reclaim the builder's persistent cache: the buildkitd
  daemon's state root (`<cache root>/buildkitd`) and every quarantined sibling
  (`buildkitd.broken-*`). The live root is removed only when no daemon holds its
  one-instance lock — a running daemon is NAMED and the root left alone — and a
  quarantined sibling is reclaimed unconditionally, because a store is only
  quarantined once it has been abandoned. Bytes are reported per root. A tree
  this user cannot delete whole (a rootless build leaves its store owned by
  container-mapped UIDs) is reported by name with the blocking path and left
  exactly as it was, never half-deleted. Strictly opt-in.

The category set itself is declared once, in `candy/plugin-clean/schema/clean.cue`:
one row per category with its flag, its one-line contract, whether a bare
`charly clean` runs it, and which categories naming it also runs. The CLI grammar
and the default set are derived from those rows, so a new category is a row — not
an edit to a hand-written rule in Go.

While any build is in flight the dangling-image sweeps remove NOTHING and print
`<label>: SKIPPED — N build(s) in flight` in place of the removed count — a
declined sweep never looks like an empty store. The scope reaper declines the
same way (`scopes: SKIPPED — …`), with its own reason when the host has no user
systemd session to enumerate scopes in, and `--build-cache` declines
(`build-cache live: SKIPPED — …`) naming the daemon that holds the store's lock.

## Image-tag ordering

`keep_images: N` budgets two ordinals — the newest N distinct IMAGES (a distinct
image being a distinct content identity: the image-config digest the engine
reports as the image ID) and at most N tags of each. Ordering is by CREATION
TIME, the only recency key total over the tags charly mints (a `--tag` build
replaces the CalVer tag, so a bed build's `check-<bed>-<calver>` parses as no
CalVer at all), with the build-tag CalVer as the tiebreak. The
`ai.opencharly.version` label is **not** an ordering key: the schema-versioning
removal stopped emitting it, and ordering by it made a pre-cutover (labelled)
image outrank a newer unlabelled one — an inversion that could delete the newest
image (`opencharly/plugin-clean#10`). Nothing charly writes populates that label
today.

## How to use it

Compose the plugin candy in a box's `candy:` list:

```yaml
- '@github.com/opencharly/plugin-clean/candy/plugin-clean:<tag>'
```

## Layout

- `candy/plugin-clean/` — the plugin module: `plugin.go`, `provider.go`,
  `command.go`, `categories.go`, `buildcache.go`, `retention.go`,
  `schema/clean.cue`, `cmd/serve/main.go`.
- `charly.yml` — the root project manifest (`discover: candy`).
- `.github/workflows/tag-on-merge.yml` — CalVer tag + `CHANGELOG/` on merge.

## Related

- Owning skill: `/charly-core:clean` — the `charly clean` reference.
- `/charly-check:check` — the R10 bed that witnesses `--dry-run` / `--deep`.
- [`opencharly/charly`](https://github.com/opencharly/charly) — the charly CLI.
