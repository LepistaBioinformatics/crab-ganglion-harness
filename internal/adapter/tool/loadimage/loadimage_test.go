package loadimage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

var png = append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, 1, 2, 3)

func invoke(t *testing.T, tool *Tool, path string) domain.Result {
	t.Helper()
	args, _ := json.Marshal(map[string]string{"path": path})
	res, err := tool.Invoke(context.Background(), args)
	if err != nil {
		t.Fatalf("Invoke returned an error, which it must never do: %v", err)
	}
	return res
}

func workspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "photo.png"), png, 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestAnImageInTheWorkspaceIsAttached(t *testing.T) {
	ws := workspace(t)
	res := invoke(t, New(ws), "photo.png")
	if len(res.Attachments) != 1 {
		t.Fatalf("no attachment came back: %+v", res)
	}
	a := res.Attachments[0]
	if a.Kind != domain.AttachmentImage || a.MIME != "image/png" || len(a.Data) != len(png) {
		t.Fatalf("attachment is wrong: %+v", domain.Attachment{Kind: a.Kind, MIME: a.MIME, Name: a.Name})
	}
	if a.Name != "photo.png" {
		t.Errorf("name = %q", a.Name)
	}
	if !strings.Contains(res.Content, "photo.png") {
		t.Errorf("the model was not told what it loaded: %q", res.Content)
	}
}

// THE CONFINEMENT OF THIS TOOL.
//
// It runs in the harness process, which the Landlock domain does NOT restrain --
// that domain is applied to the shell tool's children. So this function is the
// whole boundary, and the path it is handed comes from the model, which means it
// comes from the conversation.
func TestAPathOutsideTheWorkspaceIsRefused(t *testing.T) {
	ws := workspace(t)
	outside := filepath.Join(t.TempDir(), "secret.png")
	if err := os.WriteFile(outside, png, 0o644); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{
		outside,
		"../secret.png",
		"../../etc/passwd",
		filepath.Join(ws, "..", filepath.Base(outside)),
		"/etc/hostname",
	} {
		res := invoke(t, New(ws), p)
		if len(res.Attachments) > 0 {
			t.Errorf("%q escaped the workspace", p)
		}
		if !strings.Contains(res.Content, "load_image:") {
			t.Errorf("%q did not produce a refusal: %q", p, res.Content)
		}
	}
}

// A symlink inside the workspace pointing outside passes every textual check
// ever written, which is why the check is on the RESOLVED path.
func TestASymlinkOutOfTheWorkspaceIsRefused(t *testing.T) {
	ws := workspace(t)
	outside := filepath.Join(t.TempDir(), "secret.png")
	if err := os.WriteFile(outside, png, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(ws, "innocent.png")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}

	res := invoke(t, New(ws), "innocent.png")
	if len(res.Attachments) > 0 {
		t.Fatal("a symlink out of the workspace was followed and the file attached")
	}
}

// From the BYTES, not the extension. A model told to load "notes.png" that is
// really text would otherwise send a data:image/png URL the provider rejects
// with a message about neither the file nor the tool.
func TestTheTypeComesFromTheBytes(t *testing.T) {
	ws := workspace(t)
	if err := os.WriteFile(filepath.Join(ws, "notes.png"), []byte("this is plain text"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := invoke(t, New(ws), "notes.png")
	if len(res.Attachments) > 0 {
		t.Fatal("a text file named .png was attached as an image")
	}
	if !strings.Contains(res.Content, "not an image") {
		t.Errorf("the refusal did not say why: %q", res.Content)
	}
}

func TestEveryFailureIsAResultAndNeverAnError(t *testing.T) {
	tool := New(workspace(t))
	for _, in := range []string{`{"path":"missing.png"}`, `{"path":""}`, `{}`, `not json`} {
		res, err := tool.Invoke(context.Background(), json.RawMessage(in))
		if err != nil {
			t.Errorf("Invoke(%s) returned an error: %v", in, err)
		}
		if res.Content == "" {
			t.Errorf("Invoke(%s) said nothing", in)
		}
	}
}

func TestADirectoryIsRefusedRatherThanRead(t *testing.T) {
	ws := workspace(t)
	if err := os.Mkdir(filepath.Join(ws, "pictures"), 0o755); err != nil {
		t.Fatal(err)
	}
	if res := invoke(t, New(ws), "pictures"); len(res.Attachments) > 0 {
		t.Fatal("a directory was attached")
	}
}

func TestASubdirectoryOfTheWorkspaceIsAllowed(t *testing.T) {
	ws := workspace(t)
	sub := filepath.Join(ws, "media")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "generated.png"), png, 0o644); err != nil {
		t.Fatal(err)
	}
	// This is the path generate_image writes to, so the two tools have to agree.
	if res := invoke(t, New(ws), "media/generated.png"); len(res.Attachments) != 1 {
		t.Fatalf("a generated image could not be loaded back: %q", res.Content)
	}
}

// A project turn resolves a relative path inside its own subtree, so
// `load_image("photo.png")` in a project finds THAT project's file.
func TestARelativePathResolvesInsideTheProject(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, domain.ProjectsDirName, "seed-trial")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "photo.png"), png, 0o644); err != nil {
		t.Fatal(err)
	}

	args, _ := json.Marshal(map[string]string{"path": "photo.png"})
	res, err := New(ws).Invoke(domain.WithProject(context.Background(), "seed-trial"), args)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Attachments) != 1 {
		t.Fatalf("the project's own file was not found: %q", res.Content)
	}
}

// The BOUNDARY is still the workspace, not the project. Separating a member's
// projects from each other is a convention; claiming it is containment here
// would make this function assert something it cannot enforce -- the shell tool
// beside it can walk the whole tree under one Landlock domain.
func TestTheBoundaryIsStillTheWorkspaceAndNotTheProject(t *testing.T) {
	ws := t.TempDir()
	other := filepath.Join(ws, domain.ProjectsDirName, "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "shared.png"), png, 0o644); err != nil {
		t.Fatal(err)
	}

	args, _ := json.Marshal(map[string]string{"path": "../other/shared.png"})
	res, err := New(ws).Invoke(domain.WithProject(context.Background(), "mine"), args)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Attachments) != 1 {
		t.Fatalf("a path inside the workspace was refused: %q", res.Content)
	}
	// And outside the workspace is still refused, which is the boundary that IS
	// real.
	outside := filepath.Join(t.TempDir(), "secret.png")
	if err := os.WriteFile(outside, png, 0o644); err != nil {
		t.Fatal(err)
	}
	args, _ = json.Marshal(map[string]string{"path": outside})
	res, _ = New(ws).Invoke(domain.WithProject(context.Background(), "mine"), args)
	if len(res.Attachments) > 0 {
		t.Error("a path outside the workspace was read")
	}
}
