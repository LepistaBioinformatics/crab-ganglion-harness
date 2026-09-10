package router

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/config"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// stub records the completion it was handed, so a test can assert what actually
// went on the wire rather than what the router intended.
type stub struct {
	name string
	seen *domain.Completion
}

func (s *stub) Complete(_ context.Context, c domain.Completion) (domain.Stream, error) {
	*s.seen = c
	return nil, errors.New("stub: " + s.name)
}

func spec(name, wire, base, key string) config.ModelSpec {
	return config.ModelSpec{Name: name, Model: wire, APIBase: base, APIKey: key, Enabled: true}
}

func newTest(t *testing.T, reg config.Registry, path string, reload Reload) (*Router, *domain.Completion) {
	t.Helper()
	var seen domain.Completion
	build := func(m config.ModelSpec) domain.Provider { return &stub{name: m.Name, seen: &seen} }
	return New(reg, path, build, reload, nil), &seen
}

// The alias is this stack's handle for an entry; `model` is what the endpoint
// understands. Sending the alias is how a registry that reads correctly returns
// "unknown model" from the provider.
func TestTheWireModelIsSentNotTheAlias(t *testing.T) {
	r, seen := newTest(t, config.Registry{
		Default: "primary",
		Models:  []config.ModelSpec{spec("primary", "deepseek-chat", "https://e/v1", "k")},
	}, "", nil)

	_, _ = r.Complete(context.Background(), domain.Completion{Model: "primary"})
	if seen.Model != "deepseek-chat" {
		t.Fatalf("provider was asked for %q, want deepseek-chat", seen.Model)
	}
}

// A keyless candidate is refused HERE rather than sent unauthenticated. The
// provider's answer to a keyless request talks about authorization, which reads
// like a WRONG key rather than a missing one -- so the loop would fall through
// for the wrong reason and report the wrong thing when the chain ran out.
func TestAModelWithNoKeyIsRefusedBeforeTheCall(t *testing.T) {
	r, seen := newTest(t, config.Registry{
		Default: "primary",
		Models:  []config.ModelSpec{spec("primary", "x", "https://e/v1", "")},
	}, "", nil)

	_, err := r.Complete(context.Background(), domain.Completion{Model: "primary"})
	if err == nil {
		t.Fatal("expected a refusal for a model with no key")
	}
	if seen.Model != "" {
		t.Fatal("the provider was called despite the missing key")
	}
	// The error has to name the variable an operator would set, or it sends
	// them looking through a config file that is not the source.
	if want := "GANGLION_MODEL_KEY_PRIMARY"; !contains(err.Error(), want) {
		t.Errorf("error %q does not name %s", err, want)
	}
}

func TestAnUnconfiguredModelIsAnError(t *testing.T) {
	r, _ := newTest(t, config.Registry{Models: []config.ModelSpec{spec("a", "x", "https://e/v1", "k")}}, "", nil)
	if _, err := r.Complete(context.Background(), domain.Completion{Model: "nope"}); err == nil {
		t.Fatal("expected an error for a model with no endpoint")
	}
}

// FR-6: an admin edit reaches a RUNNING container. Without this the model tab
// would appear to work and change nothing until someone recreated the
// container, which is the failure mode the whole feature exists to avoid.
func TestAChangedFileIsPickedUpAtTheNextTurn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	current := config.Registry{Default: "a", Models: []config.ModelSpec{spec("a", "x", "https://e/v1", "k")}}
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r, _ := newTest(t, current, path, func() (config.Registry, error) { return current, nil })

	if got := r.Chain("", domain.ModelText); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("chain = %v, want [a]", got)
	}

	current = config.Registry{Default: "b", Models: []config.ModelSpec{spec("b", "y", "https://e/v1", "k")}}
	touch(t, path)

	if got := r.Chain("", domain.ModelText); !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("chain = %v after the file changed, want [b]", got)
	}
}

// A half-written or broken file must NOT take the agent off the air. Adopting
// an empty registry because somebody was midway through a save is the opposite
// of what a hot reload is for.
func TestABrokenReloadKeepsTheRunningRegistry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	good := config.Registry{Default: "a", Models: []config.ModelSpec{spec("a", "x", "https://e/v1", "k")}}
	r, _ := newTest(t, good, path, func() (config.Registry, error) {
		return config.Registry{}, errors.New("unexpected end of JSON input")
	})

	touch(t, path)
	if got := r.Chain("", domain.ModelText); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("chain = %v after a failed reload, want the previous [a]", got)
	}
}

// Same argument, different shape: a file that parses but declares nothing is
// as bad as one that does not parse.
func TestAnEmptyReloadKeepsTheRunningRegistry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	good := config.Registry{Default: "a", Models: []config.ModelSpec{spec("a", "x", "https://e/v1", "k")}}
	r, _ := newTest(t, good, path, func() (config.Registry, error) { return config.Registry{}, nil })

	touch(t, path)
	if got := r.Chain("", domain.ModelText); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("chain = %v after an empty reload, want the previous [a]", got)
	}
}

// A broken file must not be re-read on every turn for as long as it stays
// broken: the stamp is taken before the read, not after a successful one.
func TestABrokenFileIsNotRereadEveryTurn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	reads := 0
	good := config.Registry{Default: "a", Models: []config.ModelSpec{spec("a", "x", "https://e/v1", "k")}}
	r, _ := newTest(t, good, path, func() (config.Registry, error) {
		reads++
		return config.Registry{}, errors.New("broken")
	})

	touch(t, path)
	r.Chain("", domain.ModelText)
	r.Chain("", domain.ModelText)
	r.Chain("", domain.ModelText)
	if reads != 1 {
		t.Fatalf("the broken file was read %d times, want 1", reads)
	}
}

// No path means nothing to watch -- the environment-only deployment, which is
// every container that has not been recreated since this shipped.
func TestNoPathMeansNoReload(t *testing.T) {
	reads := 0
	r, _ := newTest(t, config.Registry{Default: "a", Models: []config.ModelSpec{spec("a", "x", "https://e/v1", "k")}},
		"", func() (config.Registry, error) { reads++; return config.Registry{}, nil })
	r.Chain("", domain.ModelText)
	if reads != 0 {
		t.Fatalf("reloaded %d times with no path configured, want 0", reads)
	}
}

func touch(t *testing.T, path string) {
	t.Helper()
	// A whole second forward: mtime resolution is filesystem-dependent and a
	// nanosecond bump has silently not moved it on some of them.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
