package clean

// categories.go — the `charly clean` CATEGORY SET, decoded from the plugin's OWN served schema.
//
// SDD, applied to a CLI grammar. The category set is an AUTHORED surface of this plugin — what a
// category is, which flag names it, what a reader is told about it, whether a BARE `charly clean`
// runs it, and which categories naming it also runs — so it is declared ONCE, in
// `schema/clean.cue`, and the Go grammar is DERIVED from that declaration:
//
//   - `newCleanFlagSet` (command.go) registers one flag per declared row, with the row's own
//     `contract:` as its help text;
//   - `cleanCategories` below computes the run set from the rows' `bare`/`implies` fields.
//
// Neither the flag words nor the default set is transcribed in Go. Before this file, the
// discipline lived in code as `anyCategory := images || check || deep || cacheGC || scopes` plus
// `doScopes = scopes || deep || !anyCategory` — a hand-written rule that had to be re-derived by
// every reader and re-edited by hand for every new category, and whose documented meaning drifted
// from the code's the moment either changed.
//
// The declaration is read through `spec/schemaconcat`, the SAME reader the host's plugin load gate
// uses, over the SAME embedded FS `NewMeta` serves — so the schema the CLI obeys and the schema
// the host splices cannot be two different files.

import (
	"fmt"
	"sync"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"

	"github.com/opencharly/spec/schemaconcat"
)

// category is ONE row of the declared category set (schema/clean.cue `#CleanCategory`).
type category struct {
	// Flag is the CLI word: `--<Flag>`.
	Flag string `json:"flag"`
	// Contract is what the category reclaims, in one line — also the flag's help text.
	Contract string `json:"contract"`
	// Bare is true when a plain `charly clean`, naming no category, runs this category.
	Bare bool `json:"bare"`
	// Implies are the categories that also run when this one is named.
	Implies []string `json:"implies,omitempty"`
}

var (
	schemaOnce sync.Once
	schemaVal  cue.Value
	schemaErr  error

	categoriesOnce sync.Once
	categoriesDecl []category
	categoriesErr  error
)

// pluginSchema compiles the plugin's OWN served schema exactly once per process. It is the ONE
// compile every declaration read goes through (the category set today, the store budget beside
// it): two independent compiles of the same embedded bytes would be two chances to read a
// different file, and the CLI obeys the schema the host splices only if there is one of them.
func pluginSchema() (cue.Value, error) {
	schemaOnce.Do(func() {
		src, _, err := schemaconcat.ConcatSchema(schemaFS, "schema", nil)
		if err != nil {
			schemaErr = fmt.Errorf("clean: reading the plugin schema: %w", err)
			return
		}
		schemaVal = cuecontext.New().CompileString(src)
		schemaErr = schemaVal.Err()
		if schemaErr != nil {
			schemaErr = fmt.Errorf("clean: the plugin schema does not compile: %w", schemaErr)
			schemaVal = cue.Value{}
		}
	})
	return schemaVal, schemaErr
}

// storeBudgetOnce caches D-STORE-7's authored size budget (schema/clean.cue
// `#CleanPlugin.store_budget`).
var (
	storeBudgetOnce sync.Once
	storeBudgetVal  int64
	storeBudgetErr  error
)

// declaredStoreBudget returns the store ceiling in BYTES as the plugin's own schema declares it.
// A hard-coded constant is a defect and so is a silent fallback: a schema that cannot answer is
// reported, because a budget the sweep invented would bound the store by a number nobody authored.
func declaredStoreBudget() (int64, error) {
	storeBudgetOnce.Do(func() {
		v, err := pluginSchema()
		if err != nil {
			storeBudgetErr = err
			return
		}
		b := v.LookupPath(cue.ParsePath("#CleanPlugin.store_budget.bytes"))
		if derr := b.Err(); derr != nil {
			storeBudgetErr = fmt.Errorf("clean: the plugin schema declares no #CleanPlugin.store_budget.bytes: %w", derr)
			return
		}
		got, derr := b.Int64()
		if derr != nil {
			storeBudgetErr = fmt.Errorf("clean: #CleanPlugin.store_budget.bytes is not an int: %w", derr)
			return
		}
		if got <= 0 {
			storeBudgetErr = fmt.Errorf("clean: #CleanPlugin.store_budget.bytes is %d; a budget must be positive", got)
			return
		}
		storeBudgetVal = got
	})
	return storeBudgetVal, storeBudgetErr
}

// declaredCategories returns the category set declared in the plugin's own schema, compiled and
// decoded exactly once per process. A failure here is a DEFECT IN THE EMBEDDED SCHEMA, not a
// runtime condition of the host, so it is reported rather than worked around: the grammar cannot
// guess what the operator may ask for.
func declaredCategories() ([]category, error) {
	categoriesOnce.Do(func() {
		categoriesDecl, categoriesErr = loadDeclaredCategories()
	})
	return categoriesDecl, categoriesErr
}

func loadDeclaredCategories() ([]category, error) {
	v, err := pluginSchema()
	if err != nil {
		return nil, err
	}
	def := v.LookupPath(cue.ParsePath("#CleanPlugin.categories"))
	if derr := def.Err(); derr != nil {
		return nil, fmt.Errorf("clean: the plugin schema declares no #CleanPlugin.categories: %w", derr)
	}
	var rows []category
	if derr := def.Decode(&rows); derr != nil {
		return nil, fmt.Errorf("clean: #CleanPlugin.categories does not decode: %w", derr)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("clean: the plugin schema declares an EMPTY category set")
	}
	// The declaration is the grammar's input, so its own consistency is checked here rather than
	// discovered as a duplicate-flag panic or a silently ignored implication later: a repeated
	// flag would register twice, and an `implies:` entry naming no declared row would never fire.
	declared := make(map[string]bool, len(rows))
	for _, r := range rows {
		if r.Flag == "" || r.Contract == "" {
			return nil, fmt.Errorf("clean: a category row is missing its flag or contract: %+v", r)
		}
		if declared[r.Flag] {
			return nil, fmt.Errorf("clean: category %q is declared twice", r.Flag)
		}
		declared[r.Flag] = true
	}
	for _, r := range rows {
		for _, imp := range r.Implies {
			if !declared[imp] {
				return nil, fmt.Errorf("clean: category %q implies %q, which is not a declared category", r.Flag, imp)
			}
		}
		// A declared category with no cleanPlan field bound to it would be a flag the operator can
		// name and the engine cannot run; the decode is the ONE place both callers pass through, so
		// the mismatch is reported here rather than as a nil-func panic during a clean.
		if categoryPlanFields[r.Flag] == nil {
			return nil, fmt.Errorf("clean: category %q is declared in the schema but no cleanPlan field is bound to it", r.Flag)
		}
	}
	return rows, nil
}

// cleanPlan is WHICH categories one `charly clean` invocation runs, resolved from the declared
// set and the flags the operator named. It is a NAMED struct rather than the growing positional
// bool tuple the CLI used to thread through: with six categories, a call site reading
// `print(w, tag, a, b, c, d, e, f, out)` says nothing about which `true` is which.
type cleanPlan struct {
	images     bool
	check      bool
	deep       bool
	cache      bool
	scopes     bool
	buildCache bool
	store      bool
}

// any reports whether the plan runs at least one category — the guard that decides whether the
// engine is entered at all.
func (p cleanPlan) any() bool {
	return p.images || p.check || p.deep || p.cache || p.scopes || p.buildCache || p.store
}

// categoryPlanFields binds each DECLARED category flag to the cleanPlan field that carries it. It
// is the ONE place a declared word meets a Go identifier, and the plan-vs-schema test proves the
// two sets are EQUAL: a row added to the schema with no field here cannot run, and a field added
// here with no row can never be named — both fail the gate instead of drifting in silence.
var categoryPlanFields = map[string]func(*cleanPlan) *bool{
	"images":      func(p *cleanPlan) *bool { return &p.images },
	"check":       func(p *cleanPlan) *bool { return &p.check },
	"deep":        func(p *cleanPlan) *bool { return &p.deep },
	"cache":       func(p *cleanPlan) *bool { return &p.cache },
	"scopes":      func(p *cleanPlan) *bool { return &p.scopes },
	"build-cache": func(p *cleanPlan) *bool { return &p.buildCache },
	"store":       func(p *cleanPlan) *bool { return &p.store },
}

// cleanCategories resolves the category flags the operator NAMED into the set this invocation
// runs. The whole discipline is the declaration's, not this function's prose:
//
//   - a category runs when its own flag is named;
//   - it also runs when a category that IMPLIES it is named — `--deep` also reaps build scopes,
//     because a store-wide image purge on a host with a hung build scope is the wrong half of the
//     job;
//   - a category declared `bare: true` runs when NO category flag was named at all, so naming ANY
//     one category SUPPRESSES the default set — the rule every category here has always obeyed,
//     now stated once as data instead of re-derived per category.
func cleanCategories(named map[string]bool) (cleanPlan, error) {
	cats, err := declaredCategories()
	if err != nil {
		return cleanPlan{}, err
	}
	anyNamed := false
	for _, c := range cats {
		if named[c.Flag] {
			anyNamed = true
			break
		}
	}
	// Seed: a category is selected when its own flag is named, and a category declared `bare: true`
	// is selected by a run that names NO category at all (a named category suppresses the defaults).
	selected := make(map[string]bool, len(cats))
	for _, c := range cats {
		selected[c.Flag] = named[c.Flag] || (!anyNamed && c.Bare)
	}
	// Close over `implies:` — transitively, so a declared chain (a implies b, b implies c) runs all
	// three rather than stopping at the first hop. The closure terminates because it only ever
	// ADDS a declared row and the set is finite.
	for grew := true; grew; {
		grew = false
		for _, c := range cats {
			if !selected[c.Flag] {
				continue
			}
			for _, imp := range c.Implies {
				if !selected[imp] {
					selected[imp] = true
					grew = true
				}
			}
		}
	}
	var plan cleanPlan
	for _, c := range cats {
		if selected[c.Flag] {
			*categoryPlanFields[c.Flag](&plan) = true
		}
	}
	return plan, nil
}
