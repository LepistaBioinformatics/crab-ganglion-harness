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
// Being wrong here is recoverable, and that is the point of FR-9's port split:
// compaction writes the ContextStore and cannot touch the TranscriptStore, so a
// bad policy costs the model some context on the next turn and costs the member
// nothing. The window can always be rebuilt from the transcript.
func compact(w domain.Window, budget int) domain.Window {
	if budget <= 0 || len(w.Messages) <= budget {
		return w
	}
	dropped := len(w.Messages) - budget
	w.Messages = append([]domain.Message(nil), w.Messages[dropped:]...)

	note := fmt.Sprintf("[%d earlier messages are not in this window; the full transcript is preserved]", dropped)
	if w.Summary == "" {
		w.Summary = note
	} else {
		// Replace the previous note rather than stacking them -- otherwise the
		// summary itself becomes the thing that grows without bound.
		w.Summary = note
	}
	return w
}
