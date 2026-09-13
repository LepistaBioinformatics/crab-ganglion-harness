package runtime

import (
	"fmt"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// compact rebuilds the context window when it grows past budget.
//
// v1 policy is drop-oldest, and it is deliberately NOT called summarization:
// it drops the oldest messages and records in Summary how many went, rather
// than pretending to have compressed them. Summarizing properly needs a
// provider call and a policy nobody has chosen yet -- DQ-1.
//
// It never cuts a tool result away from the call that produced it, and it never
// leaves one separated from it. See repair: getting either wrong is not a matter
// of degraded context, it is a hard 400 from the provider and a turn that cannot
// run at all.
func compact(w domain.Window, budget int) domain.Window {
	if budget <= 0 || len(w.Messages) <= budget {
		return repair(w)
	}
	dropped := len(w.Messages) - budget
	w.Messages = append([]domain.Message(nil), w.Messages[dropped:]...)

	before := len(w.Messages)
	w = repair(w)
	dropped += before - len(w.Messages)

	// Replace the previous note rather than stacking them -- otherwise the
	// summary itself becomes the thing that grows without bound.
	w.Summary = fmt.Sprintf(
		"[%d earlier messages are not in this window; the full transcript is preserved]", dropped)
	return w
}

// dropOrphanTools removes leading tool results whose call is no longer in the
// window.
//
// A provider rejects the whole request when it sees one:
//
//	Messages with role 'tool' must be a response to a preceding message
//	with 'tool_calls'
//
// Which is what drop-oldest produced the first time a long tool-using
// conversation crossed the budget: the cut landed between an assistant
// carrying tool_calls and the results answering it, and every subsequent turn
// in that conversation failed at the provider. Not degraded -- dead.
//
// Only LEADING tools can be orphaned. Messages are dropped from the front, so
// a tool later in the window still has its assistant somewhere before it.
//
// It also repairs a window that was already saved broken, which matters
// because the bad state outlives the bug: a conversation that hit this keeps
// its orphan on disk until something removes it.
func dropOrphanTools(w domain.Window) domain.Window {
	i := 0
	for i < len(w.Messages) && w.Messages[i].Role == domain.RoleTool {
		i++
	}
	if i == 0 {
		return w
	}
	w.Messages = append([]domain.Message(nil), w.Messages[i:]...)
	return w
}

// repair makes a window structurally acceptable to a provider.
//
// TWO FAULTS, and they are mirror images of each other. Both are fatal rather
// than degrading, and both OUTLIVE the bug that caused them -- the bad window is
// on disk, so every later turn in that conversation replays it and fails the
// same way. That is why this runs on load and not only on save.
func repair(w domain.Window) domain.Window {
	return groupToolRuns(dropOrphanTools(w))
}

// groupToolRuns puts the results answering one assistant message back together.
//
// A provider requires the tool messages answering an assistant's tool_calls to
// follow it CONTIGUOUSLY. Ours says so literally:
//
//	An assistant message with 'tool_calls' must be followed by tool messages
//	responding to each 'tool_call_id'. (insufficient tool messages following
//	tool_calls message)
//
// "Insufficient" is the tell: the results were all there, something was sitting
// between them. That something was the synthetic user message carrying media a
// tool returned -- appended as each call finished, so with two calls in one batch
// the first call's image split the run. See the loop's tool batch.
//
// The splitter is MOVED, never dropped. It carries an image the member or a tool
// actually produced, and the model is meant to see it; dropping it would trade a
// dead conversation for a silently incomplete one. After the run is what picoclaw
// does too, and the ordering carries the same meaning either way.
func groupToolRuns(w domain.Window) domain.Window {
	out := make([]domain.Message, 0, len(w.Messages))
	for i := 0; i < len(w.Messages); i++ {
		m := w.Messages[i]
		out = append(out, m)
		if len(m.ToolCalls) == 0 {
			continue
		}
		// The span this assistant's answers live in: everything up to the next
		// assistant, or the end. Bounded by the assistant rather than by a count
		// of answers because a run can legitimately be short -- a turn that died
		// mid-batch leaves fewer results than calls, and reordering must not then
		// swallow the rest of the conversation.
		j := i + 1
		for j < len(w.Messages) && len(w.Messages[j].ToolCalls) == 0 &&
			w.Messages[j].Role != domain.RoleAssistant {
			j++
		}
		span := w.Messages[i+1 : j]
		for _, s := range span {
			if s.Role == domain.RoleTool {
				out = append(out, s)
			}
		}
		for _, s := range span {
			if s.Role != domain.RoleTool {
				out = append(out, s)
			}
		}
		i = j - 1
	}
	w.Messages = out
	return w
}
