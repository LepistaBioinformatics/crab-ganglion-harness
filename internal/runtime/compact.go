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
// It never cuts a tool result away from the call that produced it. See
// dropOrphanTools: getting that wrong is not a matter of degraded context,
// it is a hard 400 from the provider and a turn that cannot run at all.
func compact(w domain.Window, budget int) domain.Window {
	if budget <= 0 || len(w.Messages) <= budget {
		return dropOrphanTools(w)
	}
	dropped := len(w.Messages) - budget
	w.Messages = append([]domain.Message(nil), w.Messages[dropped:]...)

	before := len(w.Messages)
	w = dropOrphanTools(w)
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
