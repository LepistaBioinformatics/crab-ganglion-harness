package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// kindChain answers per KIND, so a test can assert which slot a turn was routed
// to rather than merely which model answered.
type kindChain map[domain.ModelKind][]string

func (k kindChain) Chain(_ string, kind domain.ModelKind) []string { return k[kind] }

func imageTurn() domain.Turn {
	return domain.Turn{
		SessionID: "s",
		Input: domain.Message{
			Role:    domain.RoleUser,
			Content: "what is in this picture?",
			Attachments: []domain.Attachment{
				{Kind: domain.AttachmentImage, MIME: "image/png", Data: []byte("\x89PNG-ish")},
			},
		},
	}
}

// R4: a turn carrying an image goes to the model configured to read one, not to
// the ordinary conversation model.
func TestATurnWithAnImageIsRoutedToTheVisionChain(t *testing.T) {
	p := &scripted{content: map[string]string{"vision": "a cat"}, deltas: map[string][]string{"vision": {"a cat"}}}
	l := fallbackLoop(t, p, kindChain{
		domain.ModelText:   {"chat"},
		domain.ModelVision: {"vision"},
	})
	var sink recorder
	if _, err := l.Run(context.Background(), imageTurn(), sink.sink()); err != nil {
		t.Fatal(err)
	}
	if len(p.asked) != 1 || p.asked[0] != "vision" {
		t.Fatalf("models asked = %v, want [vision]", p.asked)
	}
}

// A turn with no image must not be diverted: the vision model is usually the
// expensive one, and paying for it on every message is a bill nobody asked for.
func TestATurnWithNoImageStaysOnTheTextChain(t *testing.T) {
	p := &scripted{content: map[string]string{"chat": "hello"}, deltas: map[string][]string{"chat": {"hello"}}}
	l := fallbackLoop(t, p, kindChain{
		domain.ModelText:   {"chat"},
		domain.ModelVision: {"vision"},
	})
	var sink recorder
	if _, err := l.Run(context.Background(),
		domain.Turn{SessionID: "s", Input: domain.Message{Role: domain.RoleUser, Content: "hi"}}, sink.sink()); err != nil {
		t.Fatal(err)
	}
	if len(p.asked) != 1 || p.asked[0] != "chat" {
		t.Fatalf("models asked = %v, want [chat]", p.asked)
	}
}

// AC-2, AND THE POINT OF THE WHOLE FEATURE.
//
// picoclaw ends the turn here with an error. Because the media reference stays
// in the session history, every LATER turn in that conversation then fails the
// same way -- the production failure this repository carries
// vision-unsupported-glm.patch for. The turn must degrade instead.
func TestWhenNothingCanReadTheImageTheTurnDegradesInsteadOfDying(t *testing.T) {
	p := &scripted{
		fail:    map[string]error{"vision": errors.New("does not support image input")},
		content: map[string]string{"chat": "I could not see the image, but from your text..."},
		deltas:  map[string][]string{"chat": {"I could not see the image, but from your text..."}},
	}
	l := fallbackLoop(t, p, kindChain{
		domain.ModelText:   {"chat"},
		domain.ModelVision: {"vision"},
	})
	var sink recorder
	out, err := l.Run(context.Background(), imageTurn(), sink.sink())
	if err != nil {
		t.Fatalf("the turn must not die: %v", err)
	}
	if out == "" {
		t.Fatal("the member got no answer at all")
	}
	if len(p.asked) != 2 || p.asked[1] != "chat" {
		t.Fatalf("models asked = %v, want the vision chain then the text chain", p.asked)
	}
}

// The degradation has to be SAID. An answer that silently ignores the image
// reads as a model that looked at it and had nothing to say.
func TestTheDegradationIsSaidOutLoud(t *testing.T) {
	p := &scripted{
		fail:    map[string]error{"vision": errors.New("does not support image input")},
		content: map[string]string{"chat": "ok"},
		deltas:  map[string][]string{"chat": {"ok"}},
	}
	l := fallbackLoop(t, p, kindChain{domain.ModelText: {"chat"}, domain.ModelVision: {"vision"}})
	var sink recorder
	if _, err := l.Run(context.Background(), imageTurn(), sink.sink()); err != nil {
		t.Fatal(err)
	}
	said := strings.ToLower(strings.Join(sink.progress, " "))
	if !strings.Contains(said, "image") || !strings.Contains(said, "could not") {
		t.Fatalf("the member was not told the image was unreadable: %v", sink.progress)
	}
}

// The retry must actually strip the media, or it fails identically to the
// attempt before it and the degradation is theatre.
func TestTheDegradedRetryCarriesNoImage(t *testing.T) {
	var sawAttachmentOn []string
	p := &scripted{
		fail:    map[string]error{"vision": errors.New("does not support image input")},
		content: map[string]string{"chat": "ok"},
		deltas:  map[string][]string{"chat": {"ok"}},
	}
	p.observe = func(c domain.Completion) {
		for _, m := range c.Messages {
			if len(m.Attachments) > 0 {
				sawAttachmentOn = append(sawAttachmentOn, c.Model)
			}
		}
	}
	l := fallbackLoop(t, p, kindChain{domain.ModelText: {"chat"}, domain.ModelVision: {"vision"}})
	var sink recorder
	if _, err := l.Run(context.Background(), imageTurn(), sink.sink()); err != nil {
		t.Fatal(err)
	}
	for _, m := range sawAttachmentOn {
		if m == "chat" {
			t.Fatal("the degraded retry still carried the image")
		}
	}
	if len(sawAttachmentOn) == 0 {
		t.Fatal("the vision attempt carried no image either -- the routing never worked")
	}
}

// The member's image must survive in the durable window. Dropping it because
// one model could not read it would mean a model configured LATER could never
// read it either.
func TestStrippingIsACopyAndDoesNotEraseTheImage(t *testing.T) {
	w := domain.Window{Messages: []domain.Message{{
		Role:        domain.RoleUser,
		Content:     "look",
		Attachments: []domain.Attachment{{Kind: domain.AttachmentImage, Data: []byte("bytes")}},
	}}}
	stripped := stripAttachments(w)
	if len(stripped.Messages[0].Attachments) != 0 {
		t.Fatal("the copy still carries the attachment")
	}
	if len(w.Messages[0].Attachments) != 1 {
		t.Fatal("the ORIGINAL window lost its attachment -- the member's image was destroyed")
	}
}

// A message that was only an image becomes an empty user turn once stripped,
// which some providers reject outright.
func TestAnImageOnlyMessageGetsAPlaceholderWhenStripped(t *testing.T) {
	w := domain.Window{Messages: []domain.Message{{
		Role:        domain.RoleUser,
		Attachments: []domain.Attachment{{Kind: domain.AttachmentImage, Data: []byte("b")}},
	}}}
	if got := stripAttachments(w).Messages[0].Content; got == "" {
		t.Fatal("an image-only message became an empty user message")
	}
}

// A ModelChain with no vision slot must still answer the turn. The registry
// resolves this itself, but the loop asserting it too is what keeps the turn
// safe whatever implements the port -- and [""] as a model name is a request no
// provider can serve.
func TestAnEmptyVisionChainFallsBackToTheTextChain(t *testing.T) {
	p := &scripted{content: map[string]string{"chat": "a cat"}, deltas: map[string][]string{"chat": {"a cat"}}}
	l := fallbackLoop(t, p, kindChain{domain.ModelText: {"chat"}})
	var sink recorder
	if _, err := l.Run(context.Background(), imageTurn(), sink.sink()); err != nil {
		t.Fatal(err)
	}
	if len(p.asked) != 1 || p.asked[0] != "chat" {
		t.Fatalf("models asked = %v, want [chat]", p.asked)
	}
}
