package subagents

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

func research(t *testing.T, r *Research, ctx context.Context, in string) string {
	t.Helper()
	res, err := r.Invoke(ctx, json.RawMessage(in))
	if err != nil {
		t.Fatalf("Invoke returned an error, which it must never do: %v", err)
	}
	return res.Content
}

// AC-12. Without a search provider this is a model asked to RECALL, which is the
// failure mode the tool exists to replace. So it is absent, not present and
// apologetic.
func TestWithoutSearchThereIsNoResearchTool(t *testing.T) {
	d := New(&fakeRunner{}, 5, 0, 0)
	if NewResearch(d, false) != nil {
		t.Error("research was offered with no web provider configured")
	}
	if NewResearch(nil, true) != nil {
		t.Error("research was offered with no dispatcher")
	}
	if NewResearch(d, true) == nil {
		t.Error("research was withheld with both present")
	}
}

// AC-13. deep is decompose -> parallel research -> hand back. THERE IS NO
// SYNTHESIS CHILD: a child has never seen the conversation, so a summary it
// wrote would answer a question nobody asked.
func TestDeepDecomposesResearchesInParallelAndSynthesizesNothing(t *testing.T) {
	r := &fakeRunner{answer: func(task domain.SubTask) domain.SubReport {
		if task.Label == "plan" {
			return domain.SubReport{Answer: "1. What does A cost?\n2. How long does B take?\n3. Who maintains C?"}
		}
		return domain.SubReport{Answer: "found something for " + task.Label + "\nhttps://example.test/x"}
	}}
	out := research(t, NewResearch(New(r, 5, 0, 0), true), ctxWith(16),
		`{"question":"is this worth doing","mode":"deep","breadth":3}`)

	seen := r.tasks()
	if len(seen) != 4 {
		t.Fatalf("ran %d children, want 4 (one plan and three questions):\n%v", len(seen), seen)
	}
	if seen[0].Label != "plan" {
		t.Errorf("the first child was %q, want the decomposition", seen[0].Label)
	}
	// Membership, not position: the research children run in PARALLEL, so the
	// order they start in is not a property of the design and asserting it
	// would make this test flaky for a reason unrelated to what it checks.
	// Order of the RESULT is asserted where it belongs, in the dispatcher test.
	asked := strings.Join([]string{seen[1].Task, seen[2].Task, seen[3].Task}, "\n")
	for _, want := range []string{"What does A cost?", "How long does B take?", "Who maintains C?"} {
		if !strings.Contains(asked, want) {
			t.Errorf("no child was given the sub-question %q", want)
		}
	}
	if !strings.Contains(out, "MATERIAL, not an answer") {
		t.Errorf("the result reads like an answer rather than findings:\n%s", out)
	}
	if !strings.Contains(out, "https://example.test/x") {
		t.Errorf("the sources were dropped:\n%s", out)
	}
}

// A decomposition nothing can be read out of falls back to researching the
// original question. A worse plan is a much better outcome than an error, and it
// costs exactly what quick mode costs.
func TestAnUnreadableDecompositionFallsBackToQuick(t *testing.T) {
	r := &fakeRunner{answer: func(task domain.SubTask) domain.SubReport {
		if task.Label == "plan" {
			return domain.SubReport{Answer: "Sure!\n\n...\n\n?"}
		}
		return domain.SubReport{Answer: "found something"}
	}}
	out := research(t, NewResearch(New(r, 5, 0, 0), true), ctxWith(16),
		`{"question":"a question long enough to research","mode":"deep"}`)

	seen := r.tasks()
	if len(seen) != 2 {
		t.Fatalf("ran %d children, want 2 (the plan and one fallback):\n%v", len(seen), seen)
	}
	if !strings.Contains(seen[1].Task, "a question long enough to research") {
		t.Errorf("the fallback did not research the original question: %q", seen[1].Task)
	}
	if !strings.Contains(out, "found something") {
		t.Errorf("no findings came back:\n%s", out)
	}
}

func TestQuickRunsExactlyOneChild(t *testing.T) {
	r := &fakeRunner{}
	research(t, NewResearch(New(r, 5, 0, 0), true), ctxWith(16), `{"question":"what is this"}`)
	if len(r.tasks()) != 1 {
		t.Fatalf("ran %d children, want 1", len(r.tasks()))
	}
	if !strings.Contains(r.tasks()[0].Task, "web_search") {
		t.Errorf("the child was not told how to research: %q", r.tasks()[0].Task)
	}
}

// AC-23 in the spec's numbering: research and subagents share ONE budget,
// because research is a caller of the same dispatcher and not a second one.
func TestResearchSpendsTheSameTurnBudgetAsTheDispatcher(t *testing.T) {
	r := &fakeRunner{}
	d := New(r, 5, 0, 0)
	ctx := ctxWith(2)

	call(t, d, ctx, modeParallel, "a", "b") // spends both
	out := research(t, NewResearch(d, true), ctx, `{"question":"anything at all"}`)

	if len(r.tasks()) != 2 {
		t.Errorf("research started %d children past the budget", len(r.tasks())-2)
	}
	if !strings.Contains(out, "budget") {
		t.Errorf("research did not explain itself:\n%s", out)
	}
}

func TestSubQuestionsReadsOrdinalsAndBulletsAndRefusesNoise(t *testing.T) {
	plan := domain.SubReport{Answer: strings.Join([]string{
		"Here are the questions:", // too short after trimming? no -- kept, see below
		"1. What is the first thing?",
		"- What is the second thing?",
		"2) What is the third thing?",
		"?",
		"",
	}, "\n")}
	got := subQuestions(plan, 6)
	// The preamble line survives, because nothing distinguishes it from a
	// question without understanding it -- and a spurious sub-question costs one
	// child, while a dropped one costs an answer. The short noise does not.
	for _, want := range []string{"What is the first thing?", "What is the second thing?", "What is the third thing?"} {
		found := false
		for _, g := range got {
			if g == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %q from %v", want, got)
		}
	}
	for _, g := range got {
		if g == "?" {
			t.Error("punctuation was read as a sub-question")
		}
	}
}

func TestEveryResearchFailureIsAResultAndNeverAnError(t *testing.T) {
	tool := NewResearch(New(&fakeRunner{}, 5, 0, 0), true)
	for _, in := range []string{`{"question":"x long enough"}`, `{"question":""}`, `{}`, `not json`} {
		res, err := tool.Invoke(ctxWith(16), json.RawMessage(in))
		if err != nil {
			t.Errorf("Invoke(%s) returned an error: %v", in, err)
		}
		if res.Content == "" {
			t.Errorf("Invoke(%s) said nothing", in)
		}
	}
}
