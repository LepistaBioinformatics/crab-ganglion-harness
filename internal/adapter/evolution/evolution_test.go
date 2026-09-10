package evolution

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/config"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/skillfile"
)

// ---------------------------------------------------------------- fixtures

// fixedProvider returns one canned completion, so a test can assert what the
// engine did with a draft rather than what a model happened to write.
type fixedProvider struct {
	body  string
	err   error
	calls int
}

func (p *fixedProvider) Complete(context.Context, domain.Completion) (domain.Stream, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	return &fixedStream{body: p.body}, nil
}

type fixedStream struct {
	body string
	done bool
}

func (s *fixedStream) Next(context.Context) (domain.Delta, error) { return domain.Delta{}, io.EOF }
func (s *fixedStream) Message() domain.Message {
	return domain.Message{Role: domain.RoleAssistant, Content: s.body}
}
func (s *fixedStream) Usage() domain.Usage { return domain.Usage{} }
func (s *fixedStream) Close() error        { return nil }

// approver answers whatever it was told to.
type approver struct {
	allowed bool
	err     error
	asked   []string
}

func (a *approver) Request(_ context.Context, r domain.ActionRequest) (domain.Decision, error) {
	a.asked = append(a.asked, r.Call.Name)
	if a.err != nil {
		return domain.Decision{}, a.err
	}
	return domain.Decision{Allowed: a.allowed, Reason: "because", By: "the-test"}, nil
}

func engine(t *testing.T, mode config.EvolutionMode, enabled bool) (*Engine, string) {
	t.Helper()
	root := t.TempDir()
	return &Engine{
		Cfg: config.Evolution{
			Enabled: enabled, Mode: mode,
			MinTaskCount: config.DefaultMinTaskCount, MinSuccessRatio: config.DefaultMinSuccessRatio,
			ColdTrigger: config.ColdManual,
		},
		StateDir:  filepath.Join(root, "state"),
		SkillsDir: filepath.Join(root, skillfile.DirName),
		Now:       func() time.Time { return time.Unix(1700000000, 0) },
		Logf:      func(string, ...any) {},
	}, root
}

func turn(id string, tools []string, ok bool) domain.TurnRecord {
	r := domain.TurnRecord{
		SessionID: domain.ConversationID(id),
		Input:     "please do the thing",
		Answer:    "done",
		Failed:    !ok,
		Usage:     domain.Usage{TotalTokens: 100},
	}
	for _, n := range tools {
		r.Tools = append(r.Tools, domain.ToolOutcome{Name: n})
	}
	return r
}

func good(name string) string {
	return "---\nname: " + name + "\ndescription: When the member asks for the thing.\n---\n\n1. Do it.\n"
}

// ---------------------------------------------------------------- the ladder

// AC-1. Off is the DEFAULT, and off must mean nothing is written -- the hot path
// is not merely quiet.
func TestWithEvolutionOffNothingIsWritten(t *testing.T) {
	e, root := engine(t, config.ModeObserve, false)
	e.Observe(context.Background(), turn("s1", []string{"shell"}, true))
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("something was written with evolution disabled: %v", entries)
	}
}

// A heartbeat is not a member asking for something. Counting it would make "the
// agent always succeeds at doing nothing" the strongest pattern in the store.
func TestAHeartbeatIsNotRecorded(t *testing.T) {
	e, _ := engine(t, config.ModeObserve, true)
	r := turn("s1", []string{"shell"}, true)
	r.SessionKey = "heartbeat"
	e.Observe(context.Background(), r)

	if _, err := os.Stat(filepath.Join(e.StateDir, TaskRecordsFile)); !os.IsNotExist(err) {
		t.Fatal("a heartbeat turn was recorded")
	}
}

func TestObserveRecordsOneLinePerTurn(t *testing.T) {
	e, _ := engine(t, config.ModeObserve, true)
	for i := 0; i < 3; i++ {
		e.Observe(context.Background(), turn("s1", []string{"web_search", "web_fetch"}, true))
	}
	b, err := os.ReadFile(filepath.Join(e.StateDir, TaskRecordsFile))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 {
		t.Fatalf("wrote %d lines, want 3", len(lines))
	}
	var rec Record
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Signature != "web_search>web_fetch" {
		t.Errorf("signature = %q", rec.Signature)
	}
	if !rec.Succeeded || rec.Tokens != 100 {
		t.Errorf("record decoded wrong: %+v", rec)
	}
}

// `observe` records and generates NOTHING. That is the whole of the bottom rung.
func TestObserveModeGeneratesNoDraft(t *testing.T) {
	e, _ := engine(t, config.ModeObserve, true)
	p := &fixedProvider{body: good("web-search")}
	e.Provider = p
	for i := 0; i < 5; i++ {
		e.Observe(context.Background(), turn("s1", []string{"web_search"}, true))
	}
	e.RunCold(context.Background())

	if p.calls != 0 {
		t.Error("observe mode asked the model to draft")
	}
	if d, _ := e.Drafts(); len(d) != 0 {
		t.Fatalf("observe mode produced %d drafts", len(d))
	}
}

// `draft` generates and writes NO skill. The middle rung is a review workflow,
// not a slower apply.
func TestDraftModeGeneratesButWritesNoSkill(t *testing.T) {
	e, _ := engine(t, config.ModeDraft, true)
	e.Provider = &fixedProvider{body: good("web-search")}
	for i := 0; i < 3; i++ {
		e.Observe(context.Background(), turn("s1", []string{"web_search"}, true))
	}
	e.RunCold(context.Background())

	drafts, _ := e.Drafts()
	if len(drafts) != 1 || drafts[0].Status != StatusCandidate {
		t.Fatalf("drafts = %+v", drafts)
	}
	if _, err := os.Stat(filepath.Join(e.SkillsDir, "web-search", skillfile.FileName)); !os.IsNotExist(err) {
		t.Fatal("draft mode wrote a skill to disk")
	}
}

// ---------------------------------------------------------------- thresholds

// Both thresholds gate, and a failure is RECORDED rather than dropped -- the
// ratio is what min_success_ratio measures, so discarding failures would make
// every pattern look perfect.
func TestBothThresholdsGate(t *testing.T) {
	records := []Record{
		{Signature: "a", Succeeded: true}, {Signature: "a", Succeeded: true},
		{Signature: "b", Succeeded: true},
		{Signature: "c", Succeeded: true}, {Signature: "c", Succeeded: false},
		{Signature: "c", Succeeded: false}, {Signature: "c", Succeeded: false},
	}
	got := patterns(records, 2, 0.7)
	if len(got) != 1 || got[0].Signature != "a" {
		t.Fatalf("patterns = %+v; b is below the count, c is below the ratio", got)
	}
}

// A turn with no tool call has no procedure to write down -- only an answer.
func TestATurnWithNoToolsIsNeverAPattern(t *testing.T) {
	got := patterns([]Record{
		{Signature: "", Succeeded: true}, {Signature: "", Succeeded: true},
		{Signature: "", Succeeded: true},
	}, 2, 0.7)
	if len(got) != 0 {
		t.Fatalf("patterns = %+v, want none", got)
	}
}

// A repeated call inside one turn is the model retrying, not a different shape
// of work. Order between DIFFERENT tools does carry meaning and is kept.
func TestTheSignatureDeduplicatesRunsButKeepsOrder(t *testing.T) {
	for in, want := range map[string]string{
		"a,a,a":     "a",
		"a,b,a":     "a>b>a",
		"a,a,b,b":   "a>b",
		"web,fetch": "web>fetch",
		"":          "",
	} {
		var tools []string
		if in != "" {
			tools = strings.Split(in, ",")
		}
		if got := signature(tools); got != want {
			t.Errorf("signature(%q) = %q, want %q", in, got, want)
		}
	}
}

// `after_turn` runs the cold path on every turn. Without idempotence by pattern
// it would produce one draft per turn forever.
func TestAPatternIsDraftedOnlyOnce(t *testing.T) {
	e, _ := engine(t, config.ModeDraft, true)
	e.Cfg.ColdTrigger = config.ColdAfterTurn
	e.Provider = &fixedProvider{body: good("web-search")}
	for i := 0; i < 6; i++ {
		e.Observe(context.Background(), turn("s1", []string{"web_search"}, true))
	}
	if d, _ := e.Drafts(); len(d) != 1 {
		t.Fatalf("produced %d drafts across 6 turns, want 1", len(d))
	}
}

// ---------------------------------------------------------------- the review

// AC-4. The scan is a narrow guardrail, not a boundary -- but it must catch the
// shapes a credential actually takes.
func TestADraftContainingACredentialIsQuarantined(t *testing.T) {
	for _, body := range []string{
		"---\nname: x\ndescription: d\n---\n\nRun with sk-live-abc123.\n",
		"---\nname: x\ndescription: d\n---\n\ncurl -H api_key=deadbeef\n",
		"---\nname: x\ndescription: d\n---\n\n-----BEGIN RSA PRIVATE KEY-----\n",
		"---\nname: x\ndescription: d\n---\n\nSet it to enc://AAAA\n",
		"---\nname: x\ndescription: d\n---\n\nexport TOKEN=ghp_abcdefghij\n",
	} {
		err := Review("x", body)
		if err == nil {
			t.Errorf("a credential passed review:\n%s", body)
			continue
		}
		// The matched TEXT must not be echoed -- quoting it would copy the
		// credential into a log line, which is the thing being prevented.
		if strings.Contains(err.Error(), "deadbeef") || strings.Contains(err.Error(), "abc123") {
			t.Errorf("the error echoed the secret: %v", err)
		}
	}
}

func TestTheReviewRejectsAStructurallyBadDraft(t *testing.T) {
	for name, body := range map[string]string{
		"no frontmatter":     "just a body\n",
		"no description":     "---\nname: x\n---\n\nbody\n",
		"name disagrees":     "---\nname: other\ndescription: d\n---\n\nbody\n",
		"unknown field":      "---\nname: x\ndescription: d\nallow_exec: true\n---\n\nbody\n",
		"no body at all":     "---\nname: x\ndescription: d\n---\n\n",
		"unterminated front": "---\nname: x\ndescription: d\n",
	} {
		if err := Review("x", body); err == nil {
			t.Errorf("%s: passed review", name)
		}
	}
}

// A frontmatter field nobody knows about is how a future key -- one some later
// loader acts on -- gets written by a model rather than by a person.
func TestOnlyNameAndDescriptionAreAllowedInFrontmatter(t *testing.T) {
	if err := Review("x", good("x")); err != nil {
		t.Fatalf("a valid skill failed review: %v", err)
	}
	if err := Review("x", "---\nname: x\ndescription: d\ntools: [shell]\n---\n\nbody\n"); err == nil {
		t.Fatal("an unknown frontmatter field passed review")
	}
}

// The name is also the directory. Traversal must not survive it.
func TestAnUnsafeSkillNameIsRefused(t *testing.T) {
	for _, n := range []string{"../escape", "a/b", ".", "", "UPPER", "-leading", "trailing-"} {
		if err := Review(n, good(n)); err == nil {
			t.Errorf("skill name %q passed review", n)
		}
	}
}

// A quarantined draft is the most interesting thing this package produces.
// Deleting it would hide what the model tried to write.
func TestAQuarantinedDraftIsKeptAndExplained(t *testing.T) {
	e, _ := engine(t, config.ModeDraft, true)
	e.Provider = &fixedProvider{body: "---\nname: web-search\ndescription: d\n---\n\nUse sk-live-oops\n"}
	for i := 0; i < 3; i++ {
		e.Observe(context.Background(), turn("s1", []string{"web_search"}, true))
	}
	e.RunCold(context.Background())

	drafts, _ := e.Drafts()
	if len(drafts) != 1 {
		t.Fatalf("drafts = %+v", drafts)
	}
	if drafts[0].Status != StatusQuarantined {
		t.Fatalf("status = %s, want quarantined", drafts[0].Status)
	}
	if drafts[0].Reason == "" {
		t.Error("a quarantined draft with no reason is one somebody will apply by hand")
	}
	if drafts[0].Body == "" {
		t.Error("the offending body was discarded; nobody can see what the model tried to write")
	}
}

// ---------------------------------------------------------------- apply

// D-2. The approver is asked, and a refusal leaves the disk untouched.
func TestApplyAsksTheApproverAndARefusalWritesNothing(t *testing.T) {
	e, _ := engine(t, config.ModeApply, true)
	e.Provider = &fixedProvider{body: good("web-search")}
	a := &approver{allowed: false}
	e.Approver = a
	for i := 0; i < 3; i++ {
		e.Observe(context.Background(), turn("s1", []string{"web_search"}, true))
	}
	e.RunCold(context.Background())

	if len(a.asked) != 1 || a.asked[0] != ApprovalAction {
		t.Fatalf("approver asked %v, want one %s", a.asked, ApprovalAction)
	}
	if _, err := os.Stat(filepath.Join(e.SkillsDir, "web-search", skillfile.FileName)); !os.IsNotExist(err) {
		t.Fatal("a refused draft was written anyway")
	}
	if d, _ := e.Drafts(); len(d) != 1 || d[0].Status != StatusRefused {
		t.Fatalf("drafts = %+v", d)
	}
}

// FAIL CLOSED. An approver that cannot be reached is not an approver that said
// yes -- and this is the one write that changes what the agent does on every
// later turn.
func TestAnUnreachableApproverIsARefusal(t *testing.T) {
	e, _ := engine(t, config.ModeApply, true)
	e.Provider = &fixedProvider{body: good("web-search")}
	e.Approver = &approver{err: errors.New("connection refused")}
	for i := 0; i < 3; i++ {
		e.Observe(context.Background(), turn("s1", []string{"web_search"}, true))
	}
	e.RunCold(context.Background())

	if _, err := os.Stat(filepath.Join(e.SkillsDir, "web-search", skillfile.FileName)); !os.IsNotExist(err) {
		t.Fatal("a skill was written while the approver was unreachable")
	}
	d, _ := e.Drafts()
	if len(d) != 1 || d[0].Status != StatusRefused {
		t.Fatalf("drafts = %+v", d)
	}
}

func TestAnApprovedDraftReachesDisk(t *testing.T) {
	e, _ := engine(t, config.ModeApply, true)
	e.Provider = &fixedProvider{body: good("web-search")}
	e.Approver = &approver{allowed: true}
	for i := 0; i < 3; i++ {
		e.Observe(context.Background(), turn("s1", []string{"web_search"}, true))
	}
	e.RunCold(context.Background())

	b, err := os.ReadFile(filepath.Join(e.SkillsDir, "web-search", skillfile.FileName))
	if err != nil {
		t.Fatalf("the skill was not written: %v", err)
	}
	if !strings.Contains(string(b), "name: web-search") {
		t.Errorf("wrote the wrong thing:\n%s", b)
	}
	if d, _ := e.Drafts(); len(d) != 1 || d[0].Status != StatusApplied {
		t.Fatalf("drafts = %+v", d)
	}
	// AC-2, the other direction: what evolution writes must load back through
	// the same format the loader reads.
	if n, desc := skillfile.Frontmatter(string(b)); n != "web-search" || desc == "" {
		t.Errorf("the written skill does not parse as one: name=%q desc=%q", n, desc)
	}
}

// AC-5. An overwrite is backed up first, and what lands is re-validated -- the
// failure guarded is not "the draft was bad", which Review caught, but "the file
// on disk is not what we thought we wrote".
func TestAnOverwriteIsBackedUpFirst(t *testing.T) {
	e, _ := engine(t, config.ModeApply, true)
	dir := filepath.Join(e.SkillsDir, "web-search")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	previous := "---\nname: web-search\ndescription: the ORIGINAL.\n---\n\nold steps\n"
	if err := os.WriteFile(filepath.Join(dir, skillfile.FileName), []byte(previous), 0o644); err != nil {
		t.Fatal(err)
	}

	e.Provider = &fixedProvider{body: good("web-search")}
	e.Approver = &approver{allowed: true}
	for i := 0; i < 3; i++ {
		e.Observe(context.Background(), turn("s1", []string{"web_search"}, true))
	}
	e.RunCold(context.Background())

	var found bool
	_ = filepath.Walk(filepath.Join(e.StateDir, BackupsDir), func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if b, rerr := os.ReadFile(p); rerr == nil && strings.Contains(string(b), "the ORIGINAL") {
			found = true
		}
		return nil
	})
	if !found {
		t.Fatal("the overwritten skill was not backed up; there is no way back")
	}
}

// A write that lands as something that does not validate is rolled back to what
// was there before.
func TestAFailedPostWriteValidationRestoresTheBackup(t *testing.T) {
	e, _ := engine(t, config.ModeApply, true)
	dir := filepath.Join(e.SkillsDir, "keep-me")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, skillfile.FileName)
	previous := "---\nname: keep-me\ndescription: the one that must survive.\n---\n\nsteps\n"
	if err := os.WriteFile(target, []byte(previous), 0o644); err != nil {
		t.Fatal(err)
	}

	// A draft whose NAME disagrees with its frontmatter passes nothing, so it
	// is forced past Review by calling write directly -- which is exactly the
	// "what landed is not what we meant" case the post-write check exists for.
	err := e.write(Draft{Name: "keep-me", Body: "---\nname: something-else\ndescription: d\n---\n\nx\n"})
	if err == nil {
		t.Fatal("an invalid write reported success")
	}
	b, rerr := os.ReadFile(target)
	if rerr != nil {
		t.Fatalf("the original was destroyed: %v", rerr)
	}
	if !strings.Contains(string(b), "must survive") {
		t.Fatalf("the rollback did not restore the original:\n%s", b)
	}
}

// Creating a skill that did not exist, then rolling back, must leave NO file --
// and must not fail trying to restore something that was never there.
func TestARollbackOfANewSkillRemovesIt(t *testing.T) {
	e, _ := engine(t, config.ModeApply, true)
	err := e.write(Draft{Name: "brand-new", Body: "---\nname: mismatch\ndescription: d\n---\n\nx\n"})
	if err == nil {
		t.Fatal("an invalid write reported success")
	}
	if _, serr := os.Stat(filepath.Join(e.SkillsDir, "brand-new", skillfile.FileName)); !os.IsNotExist(serr) {
		t.Fatal("a rolled-back new skill was left on disk")
	}
}

// ---------------------------------------------------------------- resilience

// The file is append-only and fsync'd, so the only way to a torn line is a kill
// mid-append. Losing the whole history to it would be the opposite of what the
// durability is for.
func TestATornLineDoesNotLoseTheHistory(t *testing.T) {
	e, _ := engine(t, config.ModeObserve, true)
	e.Observe(context.Background(), turn("s1", []string{"shell"}, true))
	p := filepath.Join(e.StateDir, TaskRecordsFile)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"at":"broken`)
	_ = f.Close()

	got, err := e.records()
	if err != nil {
		t.Fatalf("a torn line failed the whole read: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("read %d records, want the 1 good one", len(got))
	}
}

// No provider is not no draft: the signature IS the procedure at its coarsest,
// and a stub a human can edit is worth more than a draft that never appeared.
func TestWithNoProviderTheDeterministicSkeletonIsUsed(t *testing.T) {
	e, _ := engine(t, config.ModeDraft, true)
	for i := 0; i < 3; i++ {
		e.Observe(context.Background(), turn("s1", []string{"web_search", "web_fetch"}, true))
	}
	e.RunCold(context.Background())

	d, _ := e.Drafts()
	if len(d) != 1 || d[0].Status != StatusCandidate {
		t.Fatalf("drafts = %+v", d)
	}
	if !strings.Contains(d[0].Body, "web_search") || !strings.Contains(d[0].Body, "web_fetch") {
		t.Errorf("the skeleton lost the procedure:\n%s", d[0].Body)
	}
	if err := Review(d[0].Name, d[0].Body); err != nil {
		t.Errorf("the skeleton does not pass its own review: %v", err)
	}
}

// Two runs over the same records must propose the same drafts in the same
// order, or `after_turn` builds a different library depending on map iteration.
func TestClusteringIsDeterministic(t *testing.T) {
	records := []Record{}
	for _, sig := range []string{"z", "a", "m", "z", "a", "m"} {
		records = append(records, Record{Signature: sig, Succeeded: true})
	}
	first := patterns(records, 2, 0.7)
	for i := 0; i < 20; i++ {
		got := patterns(records, 2, 0.7)
		for j := range got {
			if got[j].Signature != first[j].Signature {
				t.Fatalf("order changed: %v then %v", first, got)
			}
		}
	}
}
