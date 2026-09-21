// Package recall lets the agent read the part of its own conversation that no
// longer fits in the window.
//
// Compaction drops the oldest messages and says so in the window's summary. The
// transcript still holds them -- it is append-only and nothing may shorten it --
// but until now the agent had no way to reach one: the summary told it that a
// conversation existed which it could not see, which is worse than not being
// told, because it invites the agent to claim it remembers.
//
// WHAT MAKES THIS CHEAP. Nothing is summarized and no provider is called. The
// cost of compaction moves rather than disappearing: free when the window is
// shortened, paid only on the turns where the agent actually reaches back. That
// is the right trade for a conversation where most turns never look.
//
// AND WHAT MAKES IT WORK AT ALL. An affordance the agent does not know about is
// not an affordance -- MemGPT keeps a statistics block in the context for
// exactly this reason. Here the two halves are already in place: the window's
// summary says HOW MUCH is outside it, and this tool's description says what to
// do about it. Neither is useful without the other.
package recall

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

const (
	// maxHits bounds how many matches come back. A pattern like "the" matches
	// most of a long conversation, and an unbounded answer would put the whole
	// transcript back in the window -- undoing, in one tool result, the
	// compaction that made this tool necessary.
	maxHits = 12

	// maxSnippet is how much of one message is quoted, in runes. Whole messages
	// are what the window already holds; this is a pointer into the
	// conversation, not a second copy of it.
	maxSnippet = 400

	// maxPattern stops a pathological argument from being echoed back in an
	// error the length of the pattern itself.
	maxPattern = 200
)

// Name is the tool's name, exported so the composition root can withhold it
// from sub-agents without importing this package's type.
const Name = "search_history"

// Tool implements search_history.
type Tool struct {
	transcript domain.TranscriptStore
}

func New(t domain.TranscriptStore) *Tool { return &Tool{transcript: t} }

func (t *Tool) Name() string { return Name }

func (t *Tool) Schema() domain.ToolSchema {
	return domain.ToolSchema{
		Name: "search_history",
		// Says WHEN, and says WHAT IT DOES NOT COVER.
		//
		// The second half is not padding. This searches the transcript, and a
		// transcript holds what was SAID -- tool results were never written to
		// one, because the member never saw them. An agent told only "search
		// this conversation" would look here for a command it ran, find
		// nothing, and conclude the command never happened: a confident
		// negative about its own history, which is worse than having no tool at
		// all. So the description names the other place and how to read it.
		Description: "Search what was SAID earlier in this conversation -- your messages and " +
			"the member's, including ones no longer in your context window. Use it when the " +
			"conversation refers to something you cannot see. Matching is plain " +
			"case-insensitive substring, so search for a distinctive word rather than a " +
			"sentence.\n\nIt does NOT cover tool output: results are not part of the " +
			"conversation. A large one was written to a file under .tool-output/ and the " +
			"message that carried it names the path; read or grep that path with the shell.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {"type": "string", "description": "Text to look for, matched case-insensitively"},
    "role": {"type": "string", "enum": ["user", "assistant"], "description": "Only search what this speaker said"}
  },
  "required": ["pattern"]
}`),
	}
}

func (t *Tool) Invoke(ctx context.Context, raw json.RawMessage) (domain.Result, error) {
	var a struct {
		Pattern string `json:"pattern"`
		Role    string `json:"role"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return domain.Result{Content: "search_history: could not read the arguments: " + err.Error()}, nil
	}
	pattern := strings.TrimSpace(a.Pattern)
	if pattern == "" {
		return domain.Result{Content: "search_history: pattern is required."}, nil
	}
	if len(pattern) > maxPattern {
		return domain.Result{Content: fmt.Sprintf(
			"search_history: the pattern is %d bytes, over the %d-byte limit.", len(pattern), maxPattern)}, nil
	}

	// A refusal is a Result, never an error: the agent should be able to carry
	// on without this rather than lose the turn to it.
	id := domain.ConversationFrom(ctx)
	if id == "" {
		return domain.Result{Content: "search_history: this turn has no conversation to search."}, nil
	}
	msgs, err := t.transcript.Read(ctx, id)
	if err != nil {
		return domain.Result{Content: "search_history: could not read the conversation: " + err.Error()}, nil
	}

	hits, scanned := search(msgs, pattern, a.Role)
	if len(hits) == 0 {
		return domain.Result{Content: fmt.Sprintf(
			"search_history: nothing matching %q in the %d earlier messages.", pattern, scanned)}, nil
	}
	return domain.Result{Content: render(hits, scanned, pattern)}, nil
}

// hit is one matching message, with where it sits in the conversation.
type hit struct {
	// index is the message's position in the whole transcript, 1-based. It is
	// reported because "the 4th message" and "the 340th" are different claims
	// about how old something is, and the agent has no other way to tell.
	index   int
	role    domain.Role
	snippet string
}

// search walks the transcript oldest-first and keeps the LAST maxHits matches.
//
// The newest ones, not the first: a conversation that says "the report" forty
// times is asking about the most recent one, and a head-biased answer would
// hand back the oldest forty and stop.
func search(msgs []domain.Message, pattern, role string) ([]hit, int) {
	needle := strings.ToLower(pattern)
	var hits []hit
	scanned := 0
	for i, m := range msgs {
		// Only what was SAID. A transcript also holds the loop's own
		// per-iteration event records, which carry no content -- matching one
		// would answer a search with an empty quotation.
		if m.Role != domain.RoleUser && m.Role != domain.RoleAssistant {
			continue
		}
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		scanned++
		if role != "" && string(m.Role) != role {
			continue
		}
		if !strings.Contains(strings.ToLower(m.Content), needle) {
			continue
		}
		hits = append(hits, hit{index: i + 1, role: m.Role, snippet: around(m.Content, needle)})
		if len(hits) > maxHits {
			hits = hits[1:]
		}
	}
	return hits, scanned
}

// around quotes the match in its surroundings rather than the head of the
// message. A match 3000 characters into an answer is invisible in a head clamp,
// which would report a hit and show nothing resembling it.
func around(content, needle string) string {
	r := []rune(content)
	if len(r) <= maxSnippet {
		return content
	}
	at := strings.Index(strings.ToLower(content), needle)
	if at < 0 {
		return string(r[:maxSnippet]) + "..."
	}
	// Byte offset to rune offset, so a multi-byte match is not cut mid-rune.
	mid := len([]rune(content[:at]))
	start := mid - maxSnippet/2
	if start < 0 {
		start = 0
	}
	end := start + maxSnippet
	if end > len(r) {
		end = len(r)
		start = end - maxSnippet
	}
	out := string(r[start:end])
	if start > 0 {
		out = "..." + out
	}
	if end < len(r) {
		out += "..."
	}
	return out
}

func render(hits []hit, scanned int, pattern string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d match(es) for %q in %d earlier messages", len(hits), pattern, scanned)
	if len(hits) == maxHits {
		// Said out loud. A capped answer that does not say it was capped is one
		// the agent reads as "this is everything".
		fmt.Fprintf(&b, " (the %d most recent are shown; there may be older ones)", maxHits)
	}
	b.WriteString(":\n")
	for _, h := range hits {
		fmt.Fprintf(&b, "\n[message %d, %s] %s\n", h.index, h.role, h.snippet)
	}
	return b.String()
}
