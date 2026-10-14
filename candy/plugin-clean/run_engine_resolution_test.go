package clean

import (
	"errors"
	"testing"

	"github.com/opencharly/sdk/kit"
	"github.com/opencharly/spec/hostenv"
)

// run_engine_resolution_test.go pins the ONE decision this change carries: the engine the
// image-tag inventory shells is the RUN engine (opencharly/opencharly#481 — the image builder is
// BuildKit and is not a container engine, so a CONSUMER that needs to run containers reads
// `engine.run`). The compiler already removed the field that used to be read; this file keeps the
// replacement from silently becoming anything else.

// stubRuntime swaps the resolved runtime for one the test controls, and restores it after.
func stubRuntime(t *testing.T, rt *hostenv.ResolvedRuntime, err error) {
	t.Helper()
	orig := kit.ResolveRuntime
	t.Cleanup(func() { kit.ResolveRuntime = orig })
	kit.ResolveRuntime = func() (*hostenv.ResolvedRuntime, error) { return rt, err }
}

// TestResolveEngineNameIsTheRunEngine is the arm a mutation to any fixed engine name fails: the
// name comes FROM the resolved runtime, so two different run engines must produce two different
// answers.
func TestResolveEngineNameIsTheRunEngine(t *testing.T) {
	for _, want := range []string{"podman", "docker", "nerdctl"} {
		stubRuntime(t, &hostenv.ResolvedRuntime{RunEngine: want}, nil)
		got, err := resolveEngineName()
		if err != nil {
			t.Fatalf("resolveEngineName(%s): %v", want, err)
		}
		if got != want {
			t.Fatalf("resolveEngineName() = %q, want %q — the engine must come from the RUN engine, not from a constant", got, want)
		}
	}
}

// TestResolveEngineNamePropagatesFailure keeps the decision honest at its edge: a runtime that
// cannot be resolved is the caller's error, never a silent default engine.
func TestResolveEngineNamePropagatesFailure(t *testing.T) {
	want := errors.New("no runtime")
	stubRuntime(t, nil, want)
	if _, err := resolveEngineName(); !errors.Is(err, want) {
		t.Fatalf("a failed resolution must surface, got %v", err)
	}
	if _, err := resolveEngineBinary(); !errors.Is(err, want) {
		t.Fatalf("the binary path must not be resolved from a failed runtime, got %v", err)
	}
}

// TestResolveEngineBinaryFollowsTheRunEngine pins the wiring: the binary is the one the RUN engine
// names, compared against the same mapping the SDK exposes (not against a hard-coded path, which
// would make the arm a host dependency rather than a claim).
func TestResolveEngineBinaryFollowsTheRunEngine(t *testing.T) {
	stubRuntime(t, &hostenv.ResolvedRuntime{RunEngine: "podman"}, nil)
	got, err := resolveEngineBinary()
	if err != nil {
		t.Fatalf("resolveEngineBinary(): %v", err)
	}
	if want := kit.EngineBinary("podman"); got != want {
		t.Fatalf("resolveEngineBinary() = %q, want the podman mapping %q", got, want)
	}
}
