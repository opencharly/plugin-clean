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
| `command:clean` | the `charly clean` CLI — `--dry-run`, `--images`, `--check`, `--deep`, `--cache`, `--keep`, `--invalidate` |
| `verb:retention` | the shared retention engine, invoked by peer plugins |

## The command

- **default** — the charly-labeled dangling-image sweep.
- **`--images`** — prune images by the retention policy (`keep_images`).
- **`--check`** — prune `.check` runs (`keep_check_runs`).
- **`--cache`** — the CAS `ArtifactStore` GC.
- **`--deep`** — the store-wide untagged/dangling-image purge (the CLI-only
  gap-closing capability). Strictly opt-in; never fires on a plain `charly
  clean`. `--deep --dry-run` reports the would-remove count + an UPPER-BOUND
  reclaimable-bytes figure and touches nothing.
- **`--invalidate`** — remove stale image TAGS (freeing their exclusively-held
  layers).

While any build is in flight the dangling-image sweeps remove NOTHING and print
`<label>: SKIPPED — N build(s) in flight` in place of the removed count — a
declined sweep never looks like an empty store.

## How to use it

Compose the plugin candy in a box's `candy:` list:

```yaml
- '@github.com/opencharly/plugin-clean/candy/plugin-clean:<tag>'
```

## Layout

- `candy/plugin-clean/` — the plugin module: `plugin.go`, `provider.go`,
  `command.go`, `retention.go`, `schema/clean.cue`, `cmd/serve/main.go`.
- `charly.yml` — the root project manifest (`discover: candy`).
- `.github/workflows/tag-on-merge.yml` — CalVer tag + `CHANGELOG/` on merge.

## Related

- Owning skill: `/charly-core:clean` — the `charly clean` reference.
- `/charly-check:check` — the R10 bed that witnesses `--dry-run` / `--deep`.
- [`opencharly/charly`](https://github.com/opencharly/charly) — the charly CLI.
