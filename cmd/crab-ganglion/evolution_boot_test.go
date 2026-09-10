package main

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/config"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

type noApprover struct{}

func (noApprover) Request(context.Context, domain.ActionRequest) (domain.Decision, error) {
	return domain.Decision{Allowed: true}, nil
}

func quiet() *log.Logger { return log.New(io.Discard, "", 0) }

func evo(mode config.EvolutionMode, trigger config.ColdTrigger) config.Registry {
	return config.Registry{Evolution: config.Evolution{
		Enabled: true, Mode: mode, ColdTrigger: trigger,
		MinTaskCount: config.DefaultMinTaskCount, MinSuccessRatio: config.DefaultMinSuccessRatio,
	}}
}

// Off is the default and every deployment today. It must build nothing and
// refuse nothing.
func TestEvolutionOffBuildsNoEngine(t *testing.T) {
	e, err := learner(config.Config{}, config.Registry{}, nil, t.TempDir(), nil, quiet())
	if err != nil || e != nil {
		t.Fatalf("engine=%v err=%v, want both nil", e, err)
	}
}

// D-2 / R10.1, AND IT IS THE POINT OF THE DECISION RATHER THAN AN INCONVENIENCE.
//
// The loop's default approver allows everything. `apply` with no endpoint
// configured would write skills with nobody asked, and "apply requires approval"
// would be a sentence that changes nothing. A downgrade to `draft` would be
// worse than the refusal: the operator would believe apply was on.
func TestApplyWithNoApproverRefusesToBoot(t *testing.T) {
	_, err := learner(config.Config{}, evo(config.ModeApply, config.ColdAfterTurn), nil, t.TempDir(), nil, quiet())
	if err == nil {
		t.Fatal("apply mode booted with no approver: skills would be written with nobody asked")
	}
	// The message has to name the variable an operator would set, and the mode
	// they could use instead, or it sends them reading source.
	for _, want := range []string{"GANGLION_APPROVAL_ENDPOINT", "draft"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

func TestApplyWithAnApproverBoots(t *testing.T) {
	e, err := learner(config.Config{}, evo(config.ModeApply, config.ColdAfterTurn),
		nil, t.TempDir(), noApprover{}, quiet())
	if err != nil {
		t.Fatalf("apply with an approver was refused: %v", err)
	}
	if e == nil || e.Approver == nil {
		t.Fatal("the engine was built without its approver")
	}
}

// The lower rungs write no skill, so they need no approver.
func TestObserveAndDraftNeedNoApprover(t *testing.T) {
	for _, mode := range []config.EvolutionMode{config.ModeObserve, config.ModeDraft} {
		if _, err := learner(config.Config{}, evo(mode, config.ColdAfterTurn),
			nil, t.TempDir(), nil, quiet()); err != nil {
			t.Errorf("mode %s was refused without an approver: %v", mode, err)
		}
	}
}

// R11. A stopped container fires no timers, so a scheduled pass on a
// scale-to-zero agent stores an intention and runs nothing. This stack made
// exactly this call once already, for cron.
func TestScheduledOnScaleToZeroRefusesToBoot(t *testing.T) {
	cfg := config.Config{Lifecycle: "scale-to-zero"}
	_, err := learner(cfg, evo(config.ModeDraft, config.ColdScheduled), nil, t.TempDir(), nil, quiet())
	if err == nil {
		t.Fatal("a scheduled pass was accepted on a container that stops when idle")
	}
	if !strings.Contains(err.Error(), "scale-to-zero") {
		t.Errorf("the refusal does not name the mode: %v", err)
	}
}

// The same trigger is fine on a container that stays up, and the other triggers
// are fine either way -- they run inside a turn, when the container is by
// definition running.
func TestScheduledIsFineWhenTheContainerStaysUp(t *testing.T) {
	cfg := config.Config{Lifecycle: "continuous"}
	if _, err := learner(cfg, evo(config.ModeDraft, config.ColdScheduled), nil, t.TempDir(), nil, quiet()); err != nil {
		t.Fatalf("scheduled was refused on a continuous agent: %v", err)
	}
	zero := config.Config{Lifecycle: "scale-to-zero"}
	for _, trig := range []config.ColdTrigger{config.ColdAfterTurn, config.ColdManual} {
		if _, err := learner(zero, evo(config.ModeDraft, trig), nil, t.TempDir(), nil, quiet()); err != nil {
			t.Errorf("trigger %s was refused under scale-to-zero: %v", trig, err)
		}
	}
}

// What the agent has learned must survive a container recreate, which under
// scale-to-zero happens routinely.
func TestTheStateLivesInsideTheWorkspace(t *testing.T) {
	ws := t.TempDir()
	e, err := learner(config.Config{}, evo(config.ModeObserve, config.ColdAfterTurn), nil, ws, nil, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(e.StateDir, ws) {
		t.Errorf("state dir %q is outside the workspace %q", e.StateDir, ws)
	}
	if !strings.HasPrefix(e.SkillsDir, ws) {
		t.Errorf("skills dir %q is outside the workspace %q", e.SkillsDir, ws)
	}
}
