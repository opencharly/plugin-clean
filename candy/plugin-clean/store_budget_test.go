package clean

// store_budget_test.go — opencharly/plugin-clean#28: the store ceiling is the MACHINE's, authored in
// the user configuration, and it must actually reach the sweep.
//
// The four "unset" levels are asserted on the REAL spec types by decoding a real per-host document,
// not through a test double: `spec.DeployConfig` carries yaml tags, so a document without `clean:` and
// one without `store_budget:` are the same two nil levels the loader would produce. What this file
// cannot prove — CUE closedness and the NodeDoc gate — is proven upstream by opencharly/spec#384's own
// `TestCleanSectionIsClosedAndBounded` / `TestCleanSectionReachesBothPerHostModels`; the split is
// stated so nobody reads this file as covering the decode rules a second time.

import (
	"context"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/opencharly/spec/spec"
)

// decodeUserConfig decodes ONE per-host document into the type the loader returns, so a test can
// start from the YAML an operator writes instead of from a Go literal.
func decodeUserConfig(t *testing.T, doc string) *spec.UnifiedFile {
	t.Helper()
	var uf spec.UnifiedFile
	if err := yaml.Unmarshal([]byte(doc), &uf); err != nil {
		t.Fatalf("decoding the user configuration: %v", err)
	}
	return &uf
}

// TestStoreBudgetFourLevels is #28's matrix, on real documents: three ways of saying nothing all
// yield "no authored ceiling", the authored 256 GiB yields exactly 274877906944, and an authored
// zero or negative value is REFUSED rather than read as absent.
func TestStoreBudgetFourLevels(t *testing.T) {
	cases := []struct {
		name    string
		doc     string
		want    int64
		has     bool
		wantErr bool
	}{
		{name: "no user configuration at all", doc: "", want: 0, has: false},
		{name: "a configuration with no clean: block", doc: "cache:\n  git: {}\n", want: 0, has: false},
		{name: "a clean: block with no store_budget:", doc: "clean: {}\n", want: 0, has: false},
		{
			name: "the operator's 256 GiB",
			doc:  "clean:\n  store_budget:\n    bytes: 274877906944\n",
			want: 274877906944, has: true,
		},
		{
			name:    "an authored ZERO is refused, never read as absent",
			doc:     "clean:\n  store_budget:\n    bytes: 0\n",
			wantErr: true,
		},
		{
			name:    "a negative ceiling is refused",
			doc:     "clean:\n  store_budget:\n    bytes: -1\n",
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, has, err := budgetFromCleanSection(decodeUserConfig(t, c.doc).Clean)
			if c.wantErr {
				if err == nil {
					t.Fatalf("budgetFromCleanSection = (%d, %v, nil); want a refusal", got, has)
				}
				if !strings.Contains(err.Error(), "not a positive ceiling") {
					t.Fatalf("refusal = %q, want it to name the ceiling", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("budgetFromCleanSection: %v", err)
			}
			if got != c.want || has != c.has {
				t.Fatalf("budgetFromCleanSection = (%d, %v), want (%d, %v)", got, has, c.want, c.has)
			}
		})
	}
	// The nil CONFIG is its own level (a host with no file), distinct from a file without the block.
	if _, has, err := budgetFromCleanSection(nil); err != nil || has {
		t.Fatalf("budgetFromCleanSection(nil) = (_, %v, %v); want (_, false, nil)", has, err)
	}
}

// TestEffectiveStoreBudgetFallsBackToExactly32GiB: with nothing authored the effective ceiling is the
// plugin's own declared default, and it is asserted EXACTLY (the shipped number, not "some default").
func TestEffectiveStoreBudgetFallsBackToExactly32GiB(t *testing.T) {
	fallback, err := declaredStoreBudget()
	if err != nil {
		t.Fatalf("declaredStoreBudget: %v", err)
	}
	if fallback != 32*1024*1024*1024 {
		t.Fatalf("the plugin's fallback = %d, want 32 GiB (34359738368)", fallback)
	}
	got, err := effectiveStoreBudget(0, false)
	if err != nil {
		t.Fatalf("effectiveStoreBudget: %v", err)
	}
	if got.Bytes != fallback || got.Source != storeBudgetSourceDefault {
		t.Fatalf("effectiveStoreBudget(absent) = %+v, want {%d, %q}", got, fallback, storeBudgetSourceDefault)
	}
}

// TestEffectiveStoreBudgetTakesTheAuthoredValue is the half that matters: an authored ceiling must
// come out of the resolution unchanged, with the user-config provenance, because a resolution path
// that silently ignored it would evict a machine's builds under the default.
func TestEffectiveStoreBudgetTakesTheAuthoredValue(t *testing.T) {
	const authored = int64(274877906944) // 256 GiB
	got, err := effectiveStoreBudget(authored, true)
	if err != nil {
		t.Fatalf("effectiveStoreBudget: %v", err)
	}
	if got.Bytes != authored || got.Source != storeBudgetSourceUser {
		t.Fatalf("effectiveStoreBudget(authored) = %+v, want {%d, %q}", got, authored, storeBudgetSourceUser)
	}
}

// TestAuthoredCeilingReachesTheSweep is the end-to-end arm: a real user-config DOCUMENT goes through
// the decode, the resolution and a real sweep over a real store, and both the sweep and its report
// carry 256.0 GiB from the user config. "Resolved but never threaded" fails here.
func TestAuthoredCeilingReachesTheSweep(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CHARLY_CACHE_DIR", root)
	seamNoLiveBuild(t)
	seamQuadlets(t, t.TempDir())
	seamEngine(t, nil, nil, nil)
	putImageEntry(t, root, "ghcr.io/opencharly/live:2026.322.0001", "2026-11-18T10:00:00Z", "layer")

	authored, hasAuthored, err := budgetFromCleanSection(decodeUserConfig(t, "clean:\n  store_budget:\n    bytes: 274877906944\n").Clean)
	if err != nil {
		t.Fatalf("budgetFromCleanSection: %v", err)
	}
	budget, err := effectiveStoreBudget(authored, hasAuthored)
	if err != nil {
		t.Fatalf("effectiveStoreBudget: %v", err)
	}

	out, err := runStoreSweep("podman", true, budget)
	if err != nil {
		t.Fatalf("runStoreSweep: %v", err)
	}
	if out.Budget != 274877906944 {
		t.Fatalf("the sweep measured against %d, want 274877906944 — the authored ceiling did not reach it", out.Budget)
	}
	if out.BudgetSource != storeBudgetSourceUser {
		t.Fatalf("BudgetSource = %q, want %q", out.BudgetSource, storeBudgetSourceUser)
	}
	got := strings.Join(storeLines(out), "")
	t.Logf("report rendered from a real user-config document:\n%s", got)
	if !strings.Contains(got, "of 256.0 GiB") || !strings.Contains(got, "ceiling: user config") {
		t.Fatalf("the report does not carry the authored ceiling and its provenance:\n%s", got)
	}
}

// TestReportNamesThePluginDefault: the same report with nothing authored must say so, and print the
// fallback in the same unit. The pair (this and the arm above) is what stops one provenance being
// presented as the other.
func TestReportNamesThePluginDefault(t *testing.T) {
	budget, err := effectiveStoreBudget(0, false)
	if err != nil {
		t.Fatalf("effectiveStoreBudget: %v", err)
	}
	lines := strings.Join(storeLines(&storeSweepOutcome{Budget: budget.Bytes, BudgetSource: budget.Source, DryRun: true, Held: 4096}), "")
	if !strings.Contains(lines, "of 32.0 GiB") || !strings.Contains(lines, "ceiling: plugin default") {
		t.Fatalf("the report does not name the plugin default:\n%s", lines)
	}
}

// TestUserStoreBudgetRequiresCompiledInPlacement: the configuration is read over the reverse channel,
// so the out-of-process CLI must REFUSE rather than report a ceiling it never read — the live arm of
// this file's rule, and the same placement constraint the keep-default categories already carry.
func TestUserStoreBudgetRequiresCompiledInPlacement(t *testing.T) {
	_, _, err := loadUserStoreBudget(context.Background(), nil)
	if err == nil {
		t.Fatal("the out-of-process path resolved a ceiling without a reverse channel")
	}
	if !strings.Contains(err.Error(), "compiled-in placement") {
		t.Fatalf("refusal = %q, want it to name the placement requirement", err)
	}
}

// TestStoreSweepRefusesWithoutAResolvedCeiling: the engine must not sweep against a zero budget if a
// future caller forgets to resolve one.
func TestStoreSweepRefusesWithoutAResolvedCeiling(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CHARLY_CACHE_DIR", root)
	seamNoLiveBuild(t)
	seamQuadlets(t, t.TempDir())
	seamEngine(t, nil, nil, nil)

	out := runRetentionOutcome(spec.RetentionRequest{Dir: t.TempDir(), DryRun: true}, cleanPlan{store: true}, storeBudget{})
	if out.Reply.Error == "" || !strings.Contains(out.Reply.Error, "without a resolved ceiling") {
		t.Fatalf("outcome error = %q, want a refusal naming the missing ceiling", out.Reply.Error)
	}
}

// TestStoreBudgetSurvivesTheInt64Projection: 274877906944 does not fit a 32-bit int. If the field
// were projected as `int`, this value would wrap to a small (or negative) ceiling on 386/arm and the
// sweep would evict under a number nobody authored — the reason `#StoreBudget.bytes` carries
// `@go(Bytes, type=int64)` and this package holds it in `int64` from the decode to the report.
func TestStoreBudgetSurvivesTheInt64Projection(t *testing.T) {
	authored := int64(274877906944) // a VARIABLE: an int32 conversion of a constant is an overflow error
	got, has, err := budgetFromCleanSection(decodeUserConfig(t, "clean:\n  store_budget:\n    bytes: 274877906944\n").Clean)
	if err != nil || !has || got != authored {
		t.Fatalf("budgetFromDeployConfig = (%d, %v, %v), want (%d, true, nil)", got, has, err, authored)
	}
	//nolint:gosec // deliberate: the assertion is that a 32-bit int CANNOT carry this value.
	narrowed := int32(got)
	if int64(narrowed) == got {
		t.Fatalf("274877906944 round-tripped through int32; the int64 projection is not doing its job")
	}
}
