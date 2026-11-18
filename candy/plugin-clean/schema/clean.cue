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
// plugin_input. SELF-CONTAINED: it references no base def, so it compiles STANDALONE
// (the property that lets the SDK compile it serve-side).
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

	// store_budget — D-STORE-7's SIZE BUDGET for the charly image store, authored here
	// because it is a VALUE the sweep decides against and no hard-coded constant may stand
	// in for it (SDD: authored configuration is CUE before it is code). The `store`
	// category reports the bytes the store RETAINS against this ceiling; it never evicts a
	// state-referenced entry to meet it, and a store its own referenced entries keep above
	// the budget is reported as OVER budget, naming the referenced bytes as the reason.
	//
	// The budget is the plugin's own declaration surface rather than a project `defaults:`
	// key because that key would have to be a field of the shared `#Defaults` in
	// opencharly/spec — a cross-repo schema change, deliberately not guessed at here
	// (opencharly/plugin-clean#28 carries that half). Until it lands, this value is the ONE
	// place the ceiling is written down, and the Go that reads it derives it here.
	store_budget: #StoreBudget
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

// #StoreBudget is D-STORE-7's size budget for the charly image store (see
// #CleanPlugin.store_budget). It is a DEF because the ceiling will grow clauses — what a
// sweep does when the store is over it is a policy question this row has to be able to
// answer later without re-shaping every reader.
#StoreBudget: {
	// bytes — the ceiling, in bytes, of what the store may RETAIN after a sweep: the bytes
	// the entries that survive still reach. A budget is a ceiling on the RETAINED set, never
	// a licence to evict a referenced entry, so a store whose own referenced entries exceed
	// it is reported (never "fixed" by deleting live state). Zero is rejected rather than
	// read as "unlimited": a disabled budget and an unset one would be the same value, and
	// the sweep could not tell "the operator set no ceiling" from "the ceiling is zero".
	bytes: int & >0
}
