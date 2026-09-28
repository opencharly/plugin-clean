# AGENTS.md — plugin-clean

Standalone plugin repo owning the externalized `charly clean` command
(`command:clean`, compiled-in) and the shared retention engine
(`verb:retention`). The plugin is a Go module at `candy/plugin-clean/` (module
path `github.com/opencharly/plugin-clean/candy/plugin-clean`); the root
`charly.yml` only declares `discover: candy` so the repo is a project and its
candy is scanned.

Canonical files:

- `candy/plugin-clean/charly.yml` — the `plugin-clean:` candy entity
  (`plugin:` block, `plan:` checks).
- `candy/plugin-clean/plugin.go` / `provider.go` — `NewProvider()` /
  `NewMeta()` / the `Invoke(OpRun)` surface.
- `candy/plugin-clean/command.go` — the `charly clean` kong CLI tree.
- `candy/plugin-clean/retention.go` — the shared retention engine.
- `candy/plugin-clean/schema/clean.cue` — the self-contained plugin schema.
- `charly.yml` — the root project manifest (`discover: candy`).
- `.github/workflows/tag-on-merge.yml` — CalVer tag + `CHANGELOG/` on merge.
- `README.md` — user overview only; never agent guidance.

## Load these skills first (R0)

- `/charly-core:clean` — the `charly clean` reference (retention, `keep_images`
  / `keep_check_runs`, image-tag pruning, `.check` run cleanup). Load before
  changing the categories or the retention engine.
- `/charly-internals:plugin` — the plugin authoring reference: the `plugin:`
  block, the `command` + `verb` provider classes, the per-plugin CUE-schema
  contract.
- `/charly-internals:git-workflow` — before any git/PR action.

## Build / validate / test

- `go build ./...` in `candy/plugin-clean/` — compile the plugin module.
- `go test ./...` in `candy/plugin-clean/` — the plugin's Go tests (the
  retention engine, the `--cache` CAS GC, the multi-tag/floor cases).
- `charly box validate` at the repo root — the structural check (the candy +
  `plugin:` block, CUE schema).
- The merge gate is the **org-wide** `charly/pr-validator` (required check
  `validate / validate`, defined in `opencharly/.github`); this repo has **no**
  per-repo candy gate.
- The R10 witness is the disposable `check-commands-local` bed (in
  `opencharly/charly`): `charly clean --dry-run` / `--deep --dry-run` exit 0.

## Modify this repo

- Edit the `plugin-clean:` candy entity, the Go source, and `schema/clean.cue`
  **together** — the schema is the single source for the plugin's served
  declaration surface.
- The retention engine is shared: `charly clean` calls it locally, and peer
  plugins reach it over `verb:retention`. Keep one implementation (R3).

## Landing

- PR-only. Every change lands through a pull request; the org-required
  `charly/pr-validator` validates the diff and body and arms native auto-merge on
  PASS. Direct pushes to `main` are blocked.
- History lives in `CHANGELOG/` (written by `tag-on-merge` at merge time); the PR
  body IS the changelog.
- The authoritative rulebook is the umbrella `AGENTS.md` in
  `opencharly/opencharly` and `charly/AGENTS.md` in the charly repo. Do not
  restate its rules here.
