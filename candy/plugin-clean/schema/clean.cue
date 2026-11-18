// plugin-clean's OWN self-contained CUE schema — the plugin's declaration
// surface, used two ways exactly like every other plugin's schema (there is
// no schema-less plugin):
//
//  1. SERVE over Describe — the host splices `base ++ plugin` at the load gate, so the
//     plugin's declarations travel WITH it and a self-contained schema that will not
//     splice is a LOUD load failure.
//  2. DOCUMENT the plugin's published surface — the reference site's per-plugin page is
//     rendered from its providers, this schema, and the candy description.
//
// command:clean's authored input is its pass-through CLI grammar (the OpRun `{args:
// [...]}` envelope) and verb:retention is invoked by peer plugins via InvokeProvider, so
// this schema DOCUMENTS both capability contracts rather than a structured
// plugin_input. SELF-CONTAINED: it imports nothing and references no base def, so it compiles
// STANDALONE — which is not a preference but a MEASURED constraint: the in-process compile this
// plugin runs over its own schema (the `cleanCategories` path) has no CUE module for `spec`, so an
// `import "github.com/opencharly/spec/schema"` fails there with "package ... imported but not
// defined", while charly's own tooling resolves it. The consequence is recorded where it belongs:
// `#CleanStoreBudget` deliberately does not answer to the contract's name (opencharly/plugin-clean#35).
#CleanPlugin: {
	// The declared capability words (the plugin.providers surface) — the command:clean
	// CLI plus the verb:retention engine peers invoke — recorded here as part of the
	// plugin's published declaration surface.
	providers: [...string]

	// The command word the plugin serves.
	command: "clean"

	// The verb word the shared retention engine is invoked by.
	verb: "retention"

	// What the command does, in one line (the public-docs surface).
	contract: string & !=""

	// The configuration surface: env var names the plugin reads, recorded here as part
	// of the plugin's published declaration surface.
	config?: [string]: string

	// store_budget — D-STORE-7's SIZE BUDGET for the charly image store. The CEILING is the
	// machine's: a host authors it in its USER CONFIGURATION (`~/.config/charly/charly.yml`,
	// D-STORE-2) as `clean: {store_budget: {bytes: ...}}`, declared by opencharly/spec
	// (`#CleanSection` / `#StoreBudget`, spec#384) and read by this plugin through
	// `loaderkit.LoadHostCleanSectionViaExecutor` (sdk#566). What is declared HERE is the
	// FALLBACK a machine that authors nothing gets, and the report names which applied —
	// `ceiling: user config` or `ceiling: plugin default`.
	//
	// The SHAPE below is deliberately local and deliberately NAMED DIFFERENTLY from the
	// contract's `#StoreBudget`: one name is one S1 definition (opencharly/plugin-clean#35), so
	// this definition must not answer to the contract's name. It cannot simply import
	// `schema.#StoreBudget` either, and that was MEASURED rather than assumed: charly's own
	// tooling resolves the import (`charly candy params plugin-clean` succeeds), but this
	// plugin's in-process compile of its own schema — the path `cleanCategories` uses — has no
	// CUE module for `spec` and fails with "package
	// github.com/opencharly/spec/schema imported but not defined". Two homes for one SHAPE
	// remain, which R2 accepts here only because one of them cannot see the other; the VALUE
	// still lives only in this file, since `spec` declares no default.
	//
	// The `store` category reports the bytes the store RETAINS against the ceiling; it never
	// evicts a state-referenced entry to meet it, and a store whose own referenced entries
	// keep it above the ceiling is reported as OVER it, naming the referenced bytes.
	store_budget: #CleanStoreBudget
	store_budget: {
		// 32 GiB: a full multi-stage stack for one box family is a few GiB, so this holds
		// several live families beside the referenced set while still bounding a host whose
		// store grows by one artifact per rebuild (the 283 GiB / 172-entry store measured
		// when D-STORE-7 was implemented had no ceiling at all).
		bytes: 32 * 1024 * 1024 * 1024
	}

	// categories — the AUTHORED category set of `charly clean`. This list is the ONE
	// declaration of what a category IS: its flag word, the one line a reader is told
	// about it, whether a BARE invocation runs it, and what naming it also runs. The Go
	// flag grammar is DERIVED from it (command.go: newCleanFlagSet registers one flag per
	// row, and cleanCategories computes the run set from `bare`/`implies`), so a category
	// cannot exist in the CLI and be missing here, or the reverse — a second, hand-written
	// copy of this discipline would be the drift this declaration exists to end.
	//
	// The list is CLOSED: a category is added by adding a row, and a flag that is not a
	// category (`--dry-run`, `--keep`, `--invalidate` — the MODIFIERS, which tune or target
	// the categories rather than name one) stays in Go and is deliberately absent here.
	//
	// The two halves below are ONE field (CUE unifies them): the first is the row SHAPE, the
	// second the value. `bare` is load-bearing for exactly one question — a category flag
	// named on the command line SUPPRESSES the default set — and `implies` is that rule's
	// other half: `--deep` also reaps build scopes, because a store-wide image purge on a
	// host that has a hung build scope is the wrong half of the job.
	categories: [...#CleanCategory]
	categories: [
		{
			flag: "images"
			contract: "Only image-tag retention: prune each box's image tags to the project's keep_images (the newest N distinct images, at most N tags of each), then the charly-labeled dangling-image sweep and the dead buildah staging sweep"
			bare: true
		},
		{
			flag: "check"
			contract: "Only check-run retention: trim each .check/<bed> run dir to the project's keep_check_runs; NOTES.md is always preserved"
			bare: true
		},
		{
			flag: "deep"
			contract: "Purge EVERY untagged/dangling image in local storage, not only the charly-labeled ones, and report UP TO the summed image size (layers shared with kept images reduce the actual reclaim, so pair it with --invalidate); never fires on a bare run"
			bare: false
			implies: ["scopes"]
		},
		{
			flag: "cache"
			contract: "Reclaim unreferenced blobs from every named charly cache — the content-addressed store's own GC — and sweep the retired cache roots; reports each store's live entries and the blobs/bytes reclaimed"
			bare: false
		},
		{
			flag: "scopes"
			contract: "Reap HUNG transient build scopes — the systemd user scopes buildah/podman put a build container into. A scope is stopped only when it has been active for at least the reaper's age bound (named in the report) AND no buildah/podman builder is alive anywhere on the host; runs on a bare run and with --deep, and any other explicit category suppresses it"
			bare: true
		},
		{
			flag: "store"
			contract: "Only the charly image store's ENTRY retention (D-STORE-7): keep the entries the host's charly state still references — the container engine's containers and the quadlet units — evict the rest, free the blobs no surviving entry reaches, and report the retained size against the store budget; it REFUSES to evict when a state surface cannot be read, because an unreadable surface is not an empty one. Never fires on a bare run"
			bare: false
		},
		{
			flag: "build-cache"
			// ⚑ No BACKTICKS in a contract string: Go's flag package reads a backtick-quoted name
			// as the flag's VALUE NAME, so a contract carrying one is printed as
			// "-build-cache <cache root>/buildkitd" — a category flag that appears to take an
			// argument it does not accept (measured on a live run). The paths below are named in
			// prose instead.
			contract: "Reclaim the builder's persistent cache — the BuildKit daemon's StateRoot() at <cache root>/buildkitd and every quarantined sibling (buildkitd.broken-*). The live root is removed only when no daemon holds its one-instance lock (a running daemon is named and the root is left alone), and a tree this user cannot delete whole is reported by name and left whole. Never fires on a bare run"
			bare: false
		},
	]
}

// #CleanCategory is ONE row of `charly clean`'s category set (see #CleanPlugin.categories).
#CleanCategory: {
	// flag — the CLI word: `--<flag>`. A USER-TERMS word (D-NAME-3): a tool's own name is
	// never a category word, so the daemon whose cache `build-cache` reclaims is named in
	// that row's `contract:` and in the report, never here.
	flag: string & !=""

	// contract — what the category reclaims, in one line. It IS the flag's help text.
	contract: string & !=""

	// bare — a plain `charly clean`, with no category flag named, runs this category.
	bare: bool

	// implies — the categories that ALSO run when this one is named. Every entry names a
	// declared row (checked when the set is decoded, so a typo fails loudly).
	implies?: [...string]
}

// #CleanStoreBudget is THIS PLUGIN'S fallback ceiling for the charly image store (see
// #CleanPlugin.store_budget) — not the contract's `#StoreBudget`, which is `spec`'s and answers
// to that name alone (opencharly/plugin-clean#35). Same shape, different name, for the measured
// reason given at the field: the in-process schema compile cannot resolve a `spec` import.
#CleanStoreBudget: {
	// bytes — the fallback ceiling, in bytes, of what the store may RETAIN after a sweep: the
	// bytes the entries that survive still reach. It is a ceiling on the RETAINED set, never a
	// licence to evict a referenced entry. Zero is rejected rather than read as "unlimited":
	// a disabled budget and an unset one would otherwise be the same value.
	bytes: int & >0
}

