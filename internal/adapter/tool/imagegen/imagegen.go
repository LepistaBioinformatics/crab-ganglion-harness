// Package imagegen is the generate_image tool.
//
// picoclaw has no equivalent -- an exhaustive search of v0.3.1 for
// generate_image, image_generation, dall, t2i, txt2img and diffusion returns
// nothing. So this is the one part of the four features with no compatibility
// constraint to honour, and the keys it reads (agents.defaults.image_gen_model
// and image_gen_model_fallbacks) are named to sit beside picoclaw's existing
// image_model pair so a future picoclaw that grows the feature has an obvious
// place to land.
//
// The tool writes files and returns their paths. It does not deliver them: the
// agent already has a shell inside the workspace and the proxy already serves
// what is in there, so a second delivery mechanism would be a second thing to
// keep correct.
package imagegen

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/config"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// MediaDirName is where generated images land, inside the workspace so the
// member can reach them and the sandbox permits the write.
const MediaDirName = "media"

const maxImages = 4

// Tool implements generate_image.
type Tool struct {
	// candidates are the image models in order. The first that answers wins;
	// this is the same "fall through on failure" rule the turn loop uses, and
	// for the same reason.
	candidates []config.ModelSpec
	workspace  string
	hc         *http.Client
	now        func() time.Time
	logf       func(string, ...any)
}

// New builds the tool, or returns nil when no generation model is configured.
//
// Nil means ABSENT from the tool list. A model told it can generate images and
// then answered "not configured" has spent a turn learning something the boot
// already knew -- and unlike search, there is no keyless fallback that could
// have worked.
func New(reg config.Registry, workspace string, hc *http.Client, logf func(string, ...any)) *Tool {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if hc == nil {
		hc = &http.Client{Timeout: 120 * time.Second} // generation is slow
	}
	var cands []config.ModelSpec
	for _, name := range reg.Chain("", config.KindImageGen) {
		if m, ok := reg.Find(name); ok && m.APIKey != "" {
			cands = append(cands, m)
		}
	}
	if len(cands) == 0 {
		return nil
	}
	return &Tool{candidates: cands, workspace: workspace, hc: hc, now: time.Now, logf: logf}
}

func (t *Tool) Name() string { return "generate_image" }

func (t *Tool) Schema() domain.ToolSchema {
	return domain.ToolSchema{
		Name: "generate_image",
		Description: "Generate one or more images from a text prompt. Returns the paths of the " +
			"files written inside the workspace, which you can then read or send to the member.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "prompt": {"type": "string", "description": "What the image should show"},
    "size": {"type": "string", "description": "Pixel size such as 1024x1024"},
    "n": {"type": "integer", "description": "How many images (default 1, max 4)", "minimum": 1, "maximum": 4}
  },
  "required": ["prompt"]
}`),
	}
}

type args struct {
	Prompt string `json:"prompt"`
	Size   string `json:"size"`
	N      int    `json:"n"`
}

func (t *Tool) Invoke(ctx context.Context, raw json.RawMessage) (domain.Result, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return domain.Result{Content: "generate_image: could not read the arguments: " + err.Error()}, nil
	}
	a.Prompt = strings.TrimSpace(a.Prompt)
	if a.Prompt == "" {
		return domain.Result{Content: "generate_image: `prompt` is required."}, nil
	}
	if a.N <= 0 {
		a.N = 1
	}
	if a.N > maxImages {
		a.N = maxImages
	}

	var lastErr error
	for _, m := range t.candidates {
		images, err := t.generate(ctx, m, a)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", m.Name, err)
			t.logf("generate_image: %v", lastErr)
			continue
		}
		paths, err := t.write(images)
		if err != nil {
			// A write failure is NOT a reason to try another model: the model
			// did its job and the disk did not, and re-generating would spend
			// the money again to fail the same way.
			return domain.Result{Content: "generate_image: could not save the image: " + err.Error()}, nil
		}
		return domain.Result{Content: fmt.Sprintf(
			"Generated %d image(s) with %s:\n%s", len(paths), m.Name, strings.Join(paths, "\n"))}, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no image model is configured")
	}
	return domain.Result{Content: "generate_image failed: " + lastErr.Error()}, nil
}

// generate calls one model's images endpoint.
func (t *Tool) generate(ctx context.Context, m config.ModelSpec, a args) ([][]byte, error) {
	body := map[string]any{
		"model":  wireModel(m),
		"prompt": a.Prompt,
		"n":      a.N,
		// b64_json rather than a URL: a URL would have to be fetched from
		// inside the agent network, and the bytes are what gets written
		// anyway. It also means a generated image never exists on a third
		// party's CDN with a guessable address.
		"response_format": "b64_json",
	}
	if a.Size != "" {
		body["size"] = a.Size
	}
	for k, v := range m.ExtraBody {
		if k != "model" && k != "prompt" {
			body[k] = v
		}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(m.APIBase, "/")+"/images/generations", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range m.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer "+m.APIKey)

	resp, err := t.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Data []struct {
			B64 string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&out); err != nil {
		return nil, err
	}
	var images [][]byte
	for _, d := range out.Data {
		raw, err := base64.StdEncoding.DecodeString(d.B64)
		if err != nil || len(raw) == 0 {
			continue
		}
		images = append(images, raw)
	}
	if len(images) == 0 {
		return nil, fmt.Errorf("the endpoint returned no image data")
	}
	return images, nil
}

func wireModel(m config.ModelSpec) string {
	if m.Model != "" {
		return m.Model
	}
	return m.Name
}

// write puts the images in the workspace and returns their paths.
//
// The FILENAME IS OURS, never the model's. A prompt-chosen name is
// member-influenced input, and this process is not confined by the Landlock
// domain the shell tool runs under -- so a name is the one place a traversal
// could reach outside the workspace.
func (t *Tool) write(images [][]byte) ([]string, error) {
	dir := filepath.Join(t.workspace, MediaDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	stamp := t.now().UTC().Format("20060102-150405.000")
	var paths []string
	for i, img := range images {
		name := fmt.Sprintf("generated-%s-%d%s", stamp, i+1, extFor(img))
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, img, 0o644); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, nil
}

// extFor names the file after what it actually is.
//
// From the magic bytes rather than from a Content-Type the endpoint claims: the
// member opens this file, and a PNG called .jpg fails in whatever they open it
// with, for a reason that points at us rather than at the provider.
func extFor(b []byte) string {
	switch {
	case len(b) >= 8 && bytes.Equal(b[:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}):
		return ".png"
	case len(b) >= 3 && b[0] == 0xff && b[1] == 0xd8 && b[2] == 0xff:
		return ".jpg"
	case len(b) >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return ".webp"
	case len(b) >= 6 && bytes.HasPrefix(b, []byte("GIF8")):
		return ".gif"
	}
	return ".bin"
}
