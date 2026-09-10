package httpsse

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

func capture(t *testing.T, seen *domain.Turn, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	s := New(":0", "")
	s.Heartbeat = 0
	return serve(t, s, func(_ context.Context, turn domain.Turn, _ domain.Sink) (string, error) {
		*seen = turn
		return "ok", nil
	}, oneUser, hdr)
}

// AC-A2's ingress half. The project comes from the header the proxy sets, and
// the harness never derives it -- the proxy owns the store that maps a chat to a
// project, and a second implementation of somebody else's convention is how two
// components silently disagree.
func TestTheProjectComesFromTheHeader(t *testing.T) {
	var seen domain.Turn
	capture(t, &seen, map[string]string{"X-Ganglion-Project": "seed-trial"})
	if seen.Project != "seed-trial" {
		t.Errorf("project = %q", seen.Project)
	}
}

func TestNoHeaderMeansTheMainWorkspace(t *testing.T) {
	var seen domain.Turn
	capture(t, &seen, nil)
	if seen.Project != "" {
		t.Errorf("project = %q, want empty", seen.Project)
	}
}

// AC-A3. THE ID BECOMES A DIRECTORY NAME, so this is the whole distance between
// a header and a path traversal -- and the refusal happens BEFORE the stream
// opens, so the caller gets a status rather than an SSE event.
func TestAnInvalidProjectIsRefusedBeforeTheStreamOpens(t *testing.T) {
	for _, bad := range []string{
		"../../etc", "..", "/absolute", "has space", "UPPER", "-leading", "a/b", "a.b",
		strings.Repeat("x", 65),
	} {
		var seen domain.Turn
		rec := capture(t, &seen, map[string]string{"X-Ganglion-Project": bad})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%q -> %d, want 400", bad, rec.Code)
		}
		if seen.Project != "" {
			t.Errorf("%q reached the handler", bad)
		}
		if strings.Contains(rec.Body.String(), "data:") {
			t.Errorf("%q opened a stream before being refused", bad)
		}
	}
}

// Refused, NOT ignored. A request naming a project that cannot exist must never
// fall through to the main workspace: that writes a member's conversation into
// the wrong place, which is worse than an error and far harder to notice.
func TestAnInvalidProjectDoesNotFallThroughToTheMainWorkspace(t *testing.T) {
	s := New(":0", "")
	s.Heartbeat = 0
	called := false
	serve(t, s, func(_ context.Context, _ domain.Turn, _ domain.Sink) (string, error) {
		called = true
		return "", nil
	}, oneUser, map[string]string{"X-Ganglion-Project": "../escape"})
	if called {
		t.Fatal("the turn ran with the project silently dropped")
	}
}

func TestTheValidAlphabetIsTheProxysOwn(t *testing.T) {
	for _, ok := range []string{"a", "seed-trial", "proj_1", "a1", strings.Repeat("x", 64)} {
		if !domain.ValidProject(ok) {
			t.Errorf("%q was refused", ok)
		}
	}
	// Empty is NOT valid: "no project" and "a project called nothing" are
	// different requests and must not collapse into one.
	if domain.ValidProject("") {
		t.Error("the empty string was accepted as a project id")
	}
}
