package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

const (
	// offloadThreshold is the size past which a result is parked and the
	// message keeps a pointer beside a head and a tail.
	//
	// 8 KiB is roughly 2k tokens: large enough that nothing ordinary crosses it
	// (a directory listing, a test run's summary, a short file) and small
	// enough that the results which DO cross it are the ones capable of filling
	// a window on their own. Below it, parking would trade bytes for a pointer
	// of the same order and lose the content for nothing.
	offloadThreshold = 8 << 10

	// offloadHeadLines and offloadTailLines are what stays inline.
	//
	// BOTH ENDS, not a head alone. The useful line of a command's output is as
	// often the last one -- an error, a total, the summary a build prints --
	// and a head-only clamp reliably keeps the noise and cuts the answer.
	offloadHeadLines = 40
	offloadTailLines = 20
)

// offload parks a large tool result and returns the message to put in the
// window.
//
// Failure is not an error here. The offload moves bytes; it does not change
// what the agent is told, so a full disk must cost a larger window and nothing
// else. A turn that works today cannot start failing because this did.
func (l *Loop) offload(ctx context.Context, t domain.Turn, callID, content string) domain.Message {
	out := domain.Message{
		Role:       domain.RoleTool,
		Content:    content,
		ToolCallID: callID,
		CreatedAt:  l.Now(),
	}
	if l.ToolOutput == nil || len(content) <= offloadThreshold {
		return out
	}
	path, err := l.ToolOutput.Put(ctx, t.SessionID, callID, content)
	if err != nil {
		return out
	}
	out.Content = clampToPointer(content, path)
	out.Offloaded = path
	return out
}

// clampToPointer keeps both ends of the output and says where the rest is.
func clampToPointer(content, path string) string {
	lines := strings.Split(content, "\n")
	if len(lines) <= offloadHeadLines+offloadTailLines {
		// Few lines, many bytes -- one enormous line, or a handful of them.
		// There is no useful middle to cut, so the whole thing is the middle:
		// say where it is and keep nothing, rather than inlining a head that is
		// itself the reason this result crossed the threshold.
		return fmt.Sprintf("[%d bytes of output, not shown; the whole of it is at %s]", len(content), path)
	}
	head := strings.Join(lines[:offloadHeadLines], "\n")
	tail := strings.Join(lines[len(lines)-offloadTailLines:], "\n")
	return fmt.Sprintf(
		"%s\n\n[... %d of %d lines not shown; the whole output, %d bytes, is at %s ...]\n\n%s",
		head, len(lines)-offloadHeadLines-offloadTailLines, len(lines), len(content), path, tail)
}

// elide replaces a parked result's remaining text with its pointer alone.
//
// This is the second half of the trade and the one compaction makes: recent
// tool output is context, old tool output is a filename. The head and tail the
// offload kept are for the iterations that FOLLOW the call, when the agent is
// still working with what it read; several turns later they are bytes nobody
// will look at, and the file they name has not moved.
//
// A result that was never parked is returned untouched. It has nowhere to point
// and its content is the same order of magnitude as a pointer would be, so
// eliding it would lose the bytes and buy nothing.
//
// Idempotent by construction: the replacement is derived from Offloaded alone,
// so running it again produces the same string rather than a pointer wrapped in
// a pointer.
func elide(m domain.Message) domain.Message {
	if m.Offloaded == "" {
		return m
	}
	m.Content = fmt.Sprintf("[earlier output, not in this window; it is at %s]", m.Offloaded)
	return m
}

// markCompaction appends ONE durable record of this turn's compaction.
//
// AN EVENTS-ONLY ASSISTANT MESSAGE, which is the one shape in this format that
// is durable and never reaches a provider. window.conversational drops exactly
// two things when it rebuilds from the transcript -- a tool result, and an
// entry with no content that carries events -- so a marker written this way is
// invisible to the model by the same rule that already hides what the loop did
// during an iteration.
//
// The alternatives both leak. A marker with Content is seeded into a rebuilt
// window and sent verbatim, as something the agent said. A marker with a novel
// Role leaks too: nothing in this harness validates a role, conversational
// drops only RoleTool, and the wire adapter passes Role through as a string.
//
// Nothing here fails a turn. The marker is a record for the member; a
// transcript that would not take it costs a divider on a screen, and the turn
// it belongs to has already done its work.
// The note is FORMATTED HERE, from the same number that fills Count, rather
// than copied off the window's Summary. Summary holds whatever the last
// DROPPING run of compaction wrote, which is not this turn's total -- and on a
// turn whose final run dropped nothing it is stale from an earlier one
// entirely. The webapp renders the count as the divider and the note inside it,
// so two sources would put two different numbers one click apart.
func (l *Loop) markCompaction(
	ctx context.Context, t domain.Turn, dropped int, sink domain.Sink,
) {
	if dropped <= 0 {
		return
	}
	note := fmt.Sprintf(
		"[%d earlier messages are not in this window; the full transcript is preserved]", dropped)
	if err := l.Transcript.Append(ctx, t.SessionID, domain.Message{
		Role: domain.RoleAssistant,
		Events: []domain.TurnEvent{{
			Kind: domain.EventCompact, Count: dropped, Detail: note,
		}},
		CreatedAt: l.Now(),
	}); err != nil {
		sink.EmitProgress(domain.Progress{
			Kind: domain.ProgressPlaceholder,
			Text: "could not record this turn's compaction: " + err.Error(),
		})
	}
}
