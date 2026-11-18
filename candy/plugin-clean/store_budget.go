package clean

// store_budget.go — the MACHINE's store ceiling (D-STORE-7) and where it comes from.
//
// # The value, and why it is not the plugin's constant
//
// `#CleanPlugin.store_budget` (schema/clean.cue) is the FALLBACK: 32 GiB, authored in the plugin's own
// schema, used when the machine says nothing. A machine that wants a different ceiling authors it in
// its USER CONFIGURATION — `~/.config/charly/charly.yml`, the per-host file D-STORE-2 makes the home
// for what the user sets — as a declared section beside `cache:`/`ledger:`/`deploy:`/`system:`:
//
//	clean:
//	  store_budget:
//	    bytes: 274877906944      # 256 GiB
//
// The field is declared in opencharly/spec (`#CleanSection` / `#StoreBudget`, spec#384) and arrives
// here through `deploykit.DeployConfig` (a type ALIAS of `spec.DeployConfig`), so this file needs no
// wire type, no new seam and no resolver of its own. `bytes` is `int & >0`, projected as `int64`
// because 256 GiB does not fit a 32-bit `int` and the overflow on 386/arm would be a silent wrap.
//
// # Three states, and never a sentinel
//
//	absent (nil at every level)      -> the plugin's 32 GiB fallback
//	authored, positive               -> the authored number
//	UNREADABLE, or authoring <= 0    -> REFUSE, naming the file
//
// The third line is the one that matters. Falling back to 32 GiB because the file could not be PARSED
// would evict a machine's builds under a ceiling it never wrote — the same "an unreadable surface is
// not an empty one" rule the state inventory follows, applied to the configuration. And `0` is never
// read as "absent": `int64`'s zero value cannot tell them apart, so presence is the field's
// nil-ness, never a number.

import (
	"context"
	"fmt"

	"github.com/opencharly/sdk"
	"github.com/opencharly/sdk/deploykit"
	"github.com/opencharly/sdk/loaderkit"
)

// Where an effective ceiling came from, as the report prints it.
const (
	storeBudgetSourceUser    = "user config"
	storeBudgetSourceDefault = "plugin default"
)

// storeBudget is ONE resolved ceiling: the bytes, and the surface that authored them.
type storeBudget struct {
	Bytes  int64
	Source string
}

// userStoreBudget reads the machine's authored ceiling. A package-level seam for the same reason
// `resolveEngine` / `listContainerImageRefs` / `liveBuildFloor` / `quadletDir` are: the reader needs
// the host reverse channel, which a unit test cannot have, so the test drives the four states the
// budget can be in through this seam and the real reader is exercised in the compiled-in placement.
var userStoreBudget = loadUserStoreBudget

// loadUserStoreBudget returns the authored ceiling and whether the machine authored one.
//
// An unreadable user configuration is an ERROR, never a silent (0, false): see this file's header.
func loadUserStoreBudget(ctx context.Context, exec *sdk.Executor, dir string) (int64, bool, error) {
	if exec == nil {
		return 0, false, fmt.Errorf("charly clean --store requires compiled-in placement (the user configuration is read over the reverse channel, which the out-of-process CLI does not have)")
	}
	cfg, err := loaderkit.LoadDeployConfigViaExecutor(ctx, exec, dir)
	if err != nil {
		return 0, false, fmt.Errorf("reading the user configuration: %w", err)
	}
	return budgetFromDeployConfig(cfg)
}

// budgetFromDeployConfig is the DECISION, split out from the read so every level of "unset" can be
// asserted on the real spec types instead of through a test double: the four levels are
//
//	cfg == nil                    -> no user configuration at all        ((0, false, nil))
//	cfg.Clean == nil              -> a configuration with no clean: block ((0, false, nil))
//	Clean.StoreBudget == nil      -> a clean: block with no store_budget: ((0, false, nil))
//	Bytes > 0                     -> the authored ceiling                ((Bytes, true, nil))
//
// and a NON-POSITIVE authored value is an error, because `int64`'s zero cannot tell "authored zero"
// from "absent" and guessing between them is how a machine's builds get evicted under a ceiling it
// never wrote.
func budgetFromDeployConfig(cfg *deploykit.DeployConfig) (int64, bool, error) {
	if cfg == nil || cfg.Clean == nil || cfg.Clean.StoreBudget == nil {
		return 0, false, nil
	}
	if bytes := cfg.Clean.StoreBudget.Bytes; bytes > 0 {
		return bytes, true, nil
	} else {
		return 0, false, fmt.Errorf("the user configuration authors `clean.store_budget.bytes: %d`, which is not a positive ceiling — refusing to fall back to the plugin default", bytes)
	}
}

// effectiveStoreBudget resolves the ONE ceiling the sweep uses: the machine's authored value when it
// authored one, else the plugin's declared default. It never invents a value and never silently
// substitutes the default for an authored one.
func effectiveStoreBudget(authored int64, hasAuthored bool) (storeBudget, error) {
	if hasAuthored {
		return storeBudget{Bytes: authored, Source: storeBudgetSourceUser}, nil
	}
	fallback, err := declaredStoreBudget()
	if err != nil {
		return storeBudget{}, fmt.Errorf("the image store's fallback ceiling: %w", err)
	}
	return storeBudget{Bytes: fallback, Source: storeBudgetSourceDefault}, nil
}
