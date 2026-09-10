// Package loadimage lets the agent look at an image that is already in its
// workspace.
//
// # WHY THIS IS THE INBOUND PATH, RATHER THAN THE TURN REQUEST
//
// A member's image reaches this stack by being uploaded into their workspace --
// that is what the webapp's composer attachment and the shared-files panel both
// do, and crab-shell-proxy's turn request carries a plain string and no media.
// Teaching the whole chain (composer, BFF, proxy wire, harness ingress) to carry
// bytes would be a change to the turn format that picoclaw shares.
//
// The image is ALREADY where the agent can reach it. So the tool reads it from
// there, and the loop turns the result into a message the vision chain can see.
// picoclaw arrives at the same design from the other direction: its load_image
// takes a workspace path too, and its media refs exist to serve channels this
// harness does not have.
//
// The harness ingress accepts inline attachments as well (httpsse), so a caller
// that does carry bytes is served without this tool. Neither path is the other's
// fallback; they are two ways in, and this is the one this stack's own upload
// flow already feeds.
package loadimage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// maxBytes bounds one image. Beyond this a provider rejects the request anyway,
// and the base64 expansion of a very large file is memory this process holds for
// the whole turn.
const maxBytes = 20 << 20 // picoclaw's DefaultMaxMediaSize

// Tool implements load_image.
type Tool struct {
	// workspace is the only directory a path may resolve inside.
	workspace string
}

func New(workspace string) *Tool { return &Tool{workspace: workspace} }

func (t *Tool) Name() string { return "load_image" }

func (t *Tool) Schema() domain.ToolSchema {
	return domain.ToolSchema{
		Name: "load_image",
		Description: "Look at an image file in the workspace. Use it when the member has " +
			"uploaded a picture, or to inspect an image you generated. The image is shown to " +
			"you on the next step of this turn.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {"path": {"type": "string", "description": "Path to the image, inside the workspace"}},
  "required": ["path"]
}`),
	}
}

func (t *Tool) Invoke(ctx context.Context, raw json.RawMessage) (domain.Result, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return domain.Result{Content: "load_image: could not read the arguments: " + err.Error()}, nil
	}
	path, err := t.resolve(ctx, a.Path)
	if err != nil {
		// A refusal is a Result, not an error: the model should be able to try
		// a different path rather than lose the turn.
		return domain.Result{Content: "load_image: " + err.Error()}, nil
	}

	st, err := os.Stat(path)
	if err != nil {
		return domain.Result{Content: fmt.Sprintf("load_image: cannot read %s: %v", a.Path, err)}, nil
	}
	if st.IsDir() {
		return domain.Result{Content: "load_image: " + a.Path + " is a directory."}, nil
	}
	if st.Size() > maxBytes {
		return domain.Result{Content: fmt.Sprintf(
			"load_image: %s is %d bytes, over the %d-byte limit.", a.Path, st.Size(), maxBytes)}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return domain.Result{Content: fmt.Sprintf("load_image: cannot read %s: %v", a.Path, err)}, nil
	}
	mime, ok := imageMIME(data)
	if !ok {
		// From the BYTES, never from the extension. A model told to load
		// "notes.png" that is really a text file would otherwise send a
		// data:image/png URL a provider rejects with a message about neither.
		return domain.Result{Content: "load_image: " + a.Path + " is not an image this harness recognises " +
			"(png, jpeg, gif or webp)."}, nil
	}

	return domain.Result{
		Content: "Image loaded: " + filepath.Base(path),
		Attachments: []domain.Attachment{{
			Kind: domain.AttachmentImage,
			MIME: mime,
			Name: filepath.Base(path),
			Data: data,
		}},
	}, nil
}

// resolve turns a model-supplied path into an absolute one inside the
// workspace, or refuses.
//
// THE PATH COMES FROM THE MODEL, WHICH MEANS IT COMES FROM THE CONVERSATION.
// This tool runs in the harness process, which the Landlock domain does NOT
// confine -- that domain is applied to the shell tool's children. So the
// confinement here has to be this function, and it works on the RESOLVED path
// (symlinks followed) rather than on the string, because a symlink inside the
// workspace pointing at /etc passes every textual check.
func (t *Tool) resolve(ctx context.Context, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("`path` is required")
	}
	p := raw
	if !filepath.IsAbs(p) {
		// Relative to the PROJECT when there is one, so `load_image("x.png")`
		// inside a project finds that project's file. The escape check below
		// still measures against the whole workspace: separating a member's
		// projects from each other is a convention, and dressing it up as a
		// boundary here would make this function claim something it cannot do.
		p = filepath.Join(domain.ProjectRoot(ctx, t.workspace), p)
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		// A path that does not exist cannot be escaped through either; report
		// it as the missing file it is, using the cleaned form.
		real = filepath.Clean(p)
	}
	root, err := filepath.EvalSymlinks(t.workspace)
	if err != nil {
		root = filepath.Clean(t.workspace)
	}
	rel, err := filepath.Rel(root, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside the workspace", raw)
	}
	return real, nil
}

// imageMIME identifies an image from its magic bytes.
func imageMIME(b []byte) (string, bool) {
	switch {
	case len(b) >= 8 && bytes.Equal(b[:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}):
		return "image/png", true
	case len(b) >= 3 && b[0] == 0xff && b[1] == 0xd8 && b[2] == 0xff:
		return "image/jpeg", true
	case len(b) >= 6 && bytes.HasPrefix(b, []byte("GIF8")):
		return "image/gif", true
	case len(b) >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return "image/webp", true
	}
	return "", false
}
