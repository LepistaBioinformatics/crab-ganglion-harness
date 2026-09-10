package imagegen

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/config"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

var pngBytes = append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, 0, 1, 2)

func reg(models ...config.ModelSpec) config.Registry {
	r := config.Registry{Models: models}
	if len(models) > 0 {
		r.ImageGen = models[0].Name
		for _, m := range models[1:] {
			r.ImageGenFalls = append(r.ImageGenFalls, m.Name)
		}
	}
	return r
}

func spec(name, base, key string) config.ModelSpec {
	return config.ModelSpec{Name: name, Model: "dall-ish", APIBase: base, APIKey: key, Enabled: true}
}

func imageServer(t *testing.T, status int, n int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		var data []map[string]string
		for i := 0; i < n; i++ {
			data = append(data, map[string]string{"b64_json": base64.StdEncoding.EncodeToString(pngBytes)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
}

// AC-5. A text model asked to generate an image returns prose describing one,
// which is worse than an absent tool because it looks like success.
func TestWithNoGenerationModelThereIsNoTool(t *testing.T) {
	if New(config.Registry{}, t.TempDir(), nil, nil) != nil {
		t.Fatal("a registry with no image_gen_model must yield no tool")
	}
	// Configured but keyless is the same thing: it can never succeed.
	r := reg(spec("art", "https://e", ""))
	if New(r, t.TempDir(), nil, nil) != nil {
		t.Fatal("a keyless generation model must yield no tool")
	}
}

func TestAGeneratedImageIsWrittenIntoTheWorkspace(t *testing.T) {
	srv := imageServer(t, http.StatusOK, 2)
	defer srv.Close()
	ws := t.TempDir()

	tool := New(reg(spec("art", srv.URL, "k")), ws, srv.Client(), nil)
	res, err := tool.Invoke(context.Background(), json.RawMessage(`{"prompt":"a crab","n":2}`))
	if err != nil {
		t.Fatalf("Invoke returned an error, which it must never do: %v", err)
	}
	files, _ := os.ReadDir(filepath.Join(ws, MediaDirName))
	if len(files) != 2 {
		t.Fatalf("wrote %d files, want 2:\n%s", len(files), res.Content)
	}
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".png") {
			t.Errorf("file %q was not named after what it actually is", f.Name())
		}
		if !strings.Contains(res.Content, f.Name()) {
			t.Errorf("the result did not tell the model where %q is", f.Name())
		}
	}
}

// The filename is OURS, never the model's. This process is not confined by the
// Landlock domain the shell tool runs under, so a prompt-chosen name would be
// the one place a traversal could reach outside the workspace.
func TestTheModelNeverChoosesTheFilename(t *testing.T) {
	srv := imageServer(t, http.StatusOK, 1)
	defer srv.Close()
	ws := t.TempDir()

	tool := New(reg(spec("art", srv.URL, "k")), ws, srv.Client(), nil)
	_, _ = tool.Invoke(context.Background(), json.RawMessage(
		`{"prompt":"../../../../etc/cron.d/pwned"}`))

	files, _ := os.ReadDir(filepath.Join(ws, MediaDirName))
	if len(files) != 1 {
		t.Fatalf("wrote %d files, want 1", len(files))
	}
	if !strings.HasPrefix(files[0].Name(), "generated-") {
		t.Fatalf("filename %q did not come from us", files[0].Name())
	}
}

// The same rule the turn loop uses, for the same reason: a model that failed
// costs nothing to skip, and there is another one configured.
func TestAFailingModelFallsThroughToTheNext(t *testing.T) {
	bad := imageServer(t, http.StatusInternalServerError, 0)
	defer bad.Close()
	good := imageServer(t, http.StatusOK, 1)
	defer good.Close()
	ws := t.TempDir()

	tool := New(reg(spec("first", bad.URL, "k"), spec("second", good.URL, "k")), ws, good.Client(), nil)
	res, _ := tool.Invoke(context.Background(), json.RawMessage(`{"prompt":"x"}`))
	if !strings.Contains(res.Content, "second") {
		t.Fatalf("the second model did not answer:\n%s", res.Content)
	}
}

func TestEveryFailureIsAResultAndNeverAnError(t *testing.T) {
	srv := imageServer(t, http.StatusInternalServerError, 0)
	defer srv.Close()
	tool := New(reg(spec("art", srv.URL, "k")), t.TempDir(), srv.Client(), nil)

	for _, in := range []string{`{"prompt":"x"}`, `{"prompt":""}`, `not json`} {
		res, err := tool.Invoke(context.Background(), json.RawMessage(in))
		if err != nil {
			t.Errorf("Invoke(%s) returned an error: %v", in, err)
		}
		if res.Content == "" {
			t.Errorf("Invoke(%s) said nothing", in)
		}
	}
}

// b64_json, not a URL: the bytes are what gets written anyway, and a URL means
// a member's generated image sits on a third party's CDN at a guessable
// address.
func TestTheEndpointIsAskedForBytesRatherThanALink(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(pngBytes)}},
		})
	}))
	defer srv.Close()

	tool := New(reg(spec("art", srv.URL, "k")), t.TempDir(), srv.Client(), nil)
	_, _ = tool.Invoke(context.Background(), json.RawMessage(`{"prompt":"x","size":"512x512"}`))
	if body["response_format"] != "b64_json" {
		t.Errorf("response_format = %v, want b64_json", body["response_format"])
	}
	if body["size"] != "512x512" {
		t.Errorf("size was not forwarded: %v", body["size"])
	}
	if body["model"] != "dall-ish" {
		t.Errorf("model = %v, want the wire model rather than the alias", body["model"])
	}
}

func TestTheFileExtensionComesFromTheBytesNotTheClaim(t *testing.T) {
	for _, tc := range []struct {
		b    []byte
		want string
	}{
		{pngBytes, ".png"},
		{[]byte{0xff, 0xd8, 0xff, 0x00}, ".jpg"},
		{append([]byte("RIFF0000WEBP"), 0), ".webp"},
		{[]byte("GIF89a"), ".gif"},
		{[]byte("not an image"), ".bin"},
	} {
		if got := extFor(tc.b); got != tc.want {
			t.Errorf("extFor(%q) = %s, want %s", tc.b[:min(4, len(tc.b))], got, tc.want)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// generate_image writes where load_image reads. The two must agree, per
// project as they already do for the main workspace -- a generated image the
// model cannot then load back is worse than no generation at all.
func TestAProjectsGeneratedImageLandsInThatProjectsMediaDirectory(t *testing.T) {
	srv := imageServer(t, http.StatusOK, 1)
	defer srv.Close()
	ws := t.TempDir()

	tool := New(reg(spec("art", srv.URL, "k")), ws, srv.Client(), nil)
	ctx := domain.WithProject(context.Background(), "seed-trial")
	res, _ := tool.Invoke(ctx, json.RawMessage(`{"prompt":"a crab"}`))

	dir := filepath.Join(ws, domain.ProjectsDirName, "seed-trial", MediaDirName)
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("nothing was written to %s (%v):\n%s", dir, err, res.Content)
	}
	if _, err := os.Stat(filepath.Join(ws, MediaDirName)); !os.IsNotExist(err) {
		t.Error("the project's image also landed in the main workspace")
	}
}
