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
// The count it returns is what the loop turns into ONE durable marker per turn.
// Returned rather than recorded here because this function is pure over a
// window and has no store, no context and no clock -- and the marker has to be
// written once for a turn in which compaction may have run several times.
func compact(w domain.Window, budget int) (domain.Window, int) {
	if budget <= 0 || len(w.Messages) <= budget {
		// NOTHING IS ELIDED HERE. Compaction runs once per iteration, so a
		// window sitting well under budget would otherwise trade an excerpt the
		// agent is still working with for a filename -- costing it one of its
		// twelve iterations to read back a file it had a moment ago, to save
		// room nothing is asking for. Eliding is for a window under pressure,
		// and dropping is what says it is.
		return repair(w), 0
	}
	dropped := len(w.Messages) - budget
	w.Messages = append([]domain.Message(nil), w.Messages[dropped:]...)

	before := len(w.Messages)
	w = repair(w)
	dropped += before - len(w.Messages)
	w = elideOldResults(w)

	// THE STATISTICS BLOCK, in MemGPT's sense: it is delivered to the model as a
	// system message (the provider adapter puts Window.Summary there), and it is
	// how the agent learns that a conversation exists which it cannot see.
	//
	// Saying only that messages are missing would be worse than saying nothing,
	// because it invites the agent to claim it remembers them. So it names the
	// affordance too -- not by tool name: this function is pure
	// over a window and cannot know which tools a deployment registered, and a
	// note that names a tool nobody wired would be a second kind of lie.
	//
	// Replaced rather than stacked, otherwise the summary itself becomes the
	// thing that grows without bound.
	w.Summary = fmt.Sprintf(
		"[%d earlier messages are not in this window. The full transcript is preserved "+
			"and is searchable.]", dropped)
	return w, dropped
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

// elisionKeep is how many of the most recent parked results keep their text.
//
// Four is a working set, not a guess about relevance: the agent is reading what
// it just ran, and one iteration can produce several results at once. Beyond
// that the output is something it already used, and what it needs from those is
// the filename.
const elisionKeep = 4

// elideOldResults trades an old parked result's remaining text for its pointer.
//
// This is the cheap half of the whole feature and the reason FR-1 comes first:
// recent tool output is context, old tool output is a filename. It costs no
// provider call, because nothing is summarized -- the bytes are already on disk
// and the message keeps the path to them.
//
// Only a PARKED result is touched. One that was never parked has nowhere to
// point and its text is the same order of magnitude as a pointer would be, so
// eliding it would lose the bytes and buy nothing.
//
// Idempotent: elide derives its replacement from Offloaded alone, which matters
// because compaction runs several times in one turn and each run sees the
// output of the last.
func elideOldResults(w domain.Window) domain.Window {
	// COPIED BEFORE ANYTHING IS WRITTEN. dropOrphanTools returns the caller's
	// own slice when there is no orphan to drop, which is the ordinary case, so
	// writing through this one in place would reach back into the window the
	// loop is still holding.
	msgs := append([]domain.Message(nil), w.Messages...)
	kept := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Offloaded == "" {
			continue
		}
		if kept < elisionKeep {
			kept++
			continue
		}
		msgs[i] = elide(msgs[i])
	}
	w.Messages = msgs
	return w
}
