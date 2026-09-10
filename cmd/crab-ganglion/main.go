// Command crab-ganglion is the harness binary.
//
// This file is the composition root: the ONLY place where adapters meet. Every
// other package knows the domain and nothing else, which is what AR-4's test
// enforces and what keeps a second provider or a second ingress a file rather
// than a refactor.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/adapter/approver/proxy"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/adapter/httpsse"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/adapter/provider/openai"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/adapter/provider/router"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/adapter/skills"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/adapter/store/jsonl"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/adapter/store/window"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/adapter/telemetry/otlp"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/adapter/tool"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/adapter/tool/exec"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/adapter/tool/exec/landlock"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/adapter/tool/imagegen"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/adapter/tool/loadimage"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/adapter/tool/websearch"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/config"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/runtime"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/secret"
)

func main() {
	// BEFORE ANYTHING. In sandbox mode this process is a command the agent
	// asked for: it applies the Landlock domain and execve's the shell, and
	// must not read configuration, open the transcript or reach the network
	// on the way -- every one of those would run unconfined.
	if len(os.Args) > 1 && os.Args[1] == exec.SandboxArg {
		if err := exec.RunSandboxed(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "sandbox: %v\n", err)
			os.Exit(126)
		}
		return
	}

	logger := log.New(os.Stderr, "ganglion ", log.LstdFlags|log.LUTC)

	if len(os.Args) > 1 && os.Args[1] == "encrypt" {
		if err := encrypt(); err != nil {
			logger.Fatalf("encrypt: %v", err)
		}
		return
	}

	cfg, err := config.Load()
	if err != nil {
		logger.Fatalf("config: %v", err)
	}

	// Fail CLOSED, and fail at BOOT.
	//
	// There is no setting that disables the confinement, so a kernel that
	// cannot provide one is a deployment that must not serve turns. Checked
	// here rather than at the first tool call because this stack has repeatedly
	// paid for the other choice: a missing persona path and three unset
	// variables all surfaced mid-conversation, as behaviour nobody could
	// attribute, instead of as one line at startup.
	abi, err := landlock.ABI()
	if err != nil {
		logger.Fatalf("sandbox: %v", err)
	}
	logger.Printf("sandbox: landlock ABI %d", abi)

	self, err := os.Executable()
	if err != nil {
		logger.Fatalf("sandbox: cannot locate my own binary to re-exec: %v", err)
	}

	// H-1. Everything lives under the "workspace" segment, because that is where
	// crab-shell-proxy already looks: config.SessionsDir resolves to
	// <userDir>/<segment>/sessions, and the mount puts <userDir> at DataDir.
	//
	// Writing one level shallower -- which is what this did first -- produced a
	// transcript nothing could read: reloading a conversation returned an empty
	// history, because the proxy was looking in a directory the harness never
	// wrote to. Durability is worth nothing without a reader.
	workspace := filepath.Join(cfg.DataDir, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		logger.Fatalf("workspace: %v", err)
	}

	models, err := modelRouter(cfg, logger)
	if err != nil {
		logger.Fatalf("models: %v", err)
	}
	reg := models.Registry()
	logger.Printf("models: %d configured, default %q", len(reg.Models), reg.Default)

	transcript := jsonl.New(filepath.Join(workspace, "sessions"))
	loop := &runtime.Loop{
		Provider:   models,
		Models:     models,
		Transcript: transcript,
		// Same store, second port: it appends AND checkpoints, but the loop
		// only ever sees the narrow interface for each job.
		Checkpoints: transcript,
		Context:     window.New(filepath.Join(workspace, "windows")),
		Tools:       tool.NewRegistry(tools(workspace, self, reg, logger)...),
		Model:       cfg.Model,
		System:      systemPrompt(cfg, logger),
		Prompt: &skills.Prompt{
			PersonaFile: cfg.SystemFile,
			Persona:     cfg.System,
			Loader: skills.Loader{
				Workspace:  workspace,
				SharedRoot: cfg.SkillsRoot,
				Logf:       logger.Printf,
			},
			Logf: logger.Printf,
		},
		MaxIterations:   cfg.MaxTurnIter,
		ApprovalTimeout: cfg.ApprovalTimeout,
	}
	// FR-10. Unset endpoint means the exporter posts nothing, so a deployment
	// without a collector behaves exactly as before rather than logging a
	// failed request per turn.
	if cfg.OTLPEndpoint != "" {
		tel := otlp.New(cfg.OTLPEndpoint, "crab-ganglion", nil)
		tel.Logf = logger.Printf
		loop.Telemetry = tel
		logger.Printf("telemetry: exporting to %s", cfg.OTLPEndpoint)
	}

	// Left unset, the loop installs an allow-all approver. Assigned only when
	// an endpoint exists, so a typed nil can never reach the port.
	if cfg.ApprovalEndpoint != "" {
		loop.Approver = proxy.New(cfg.ApprovalEndpoint, cfg.AuthToken, cfg.GatedTools, nil)
	}

	// Recovery runs before the first turn is served. Under scale-to-zero this
	// start IS the turn after a crash, so an interrupted answer becomes an
	// ordinary message before the model is asked to continue the conversation.
	// Housekeeping beside the partial recovery: the sandbox gives commands
	// their TMPDIR inside the workspace (nothing outside it is writable), and
	// nothing removes what a command leaves there. Cleared at boot rather than
	// per turn -- a scale-to-zero agent boots often, and a running one holding
	// its own scratch for the length of a session is the ordinary behaviour of
	// a /tmp anyway.
	if err := os.RemoveAll(filepath.Join(workspace, exec.TmpDirName)); err != nil {
		logger.Printf("clear the command scratch dir: %v", err)
	}

	if folded, dropped, err := transcript.RecoverPartials(context.Background()); err != nil {
		logger.Printf("partial recovery: %v", err)
	} else if folded > 0 || dropped > 0 {
		logger.Printf("recovered %d interrupted answer(s), dropped %d stale checkpoint(s)", folded, dropped)
	}

	var ingress domain.Ingress = httpsse.New(cfg.Addr, cfg.AuthToken)
	if s, ok := ingress.(*httpsse.Server); ok {
		s.Logf = logger.Printf
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Printf("listening on %s, model %s", cfg.Addr, cfg.Model)
	if err := ingress.Serve(ctx, loop.Run); err != nil {
		logger.Fatalf("serve: %v", err)
	}
	logger.Print("stopped")
}

// systemPrompt prefers the file, so the proxy's persona cascade can mount one
// read-only without the container needing a restart to pick up a new value.
// modelRouter builds the model registry and the router over it.
//
// Two sources, and the order between them is the back-compatibility contract:
// a mounted config file wins, and when there is none the three environment
// variables synthesize the single entry every deployed container runs on today.
// A container that has not been recreated since this feature shipped therefore
// behaves identically, which is the only reason it is safe to ship at all.
func modelRouter(cfg config.Config, logger *log.Logger) (*router.Router, error) {
	res := secret.Resolver{Passphrase: cfg.KeyPassphrase, KeyFile: cfg.KeyFile}
	load := func() (config.Registry, error) {
		return config.LoadRegistry(cfg.ConfigFile, res, os.Getenv)
	}
	reg, err := load()
	if err != nil {
		return nil, err
	}
	path := cfg.ConfigFile
	if len(reg.Models) == 0 {
		// No file, or a file declaring nothing. The environment is the whole
		// configuration, and there is no path to watch.
		if cfg.Model == "" || cfg.BaseURL == "" {
			return nil, fmt.Errorf("no model is configured: %s declares none and GANGLION_MODEL/GANGLION_BASE_URL are unset",
				cfg.ConfigFile)
		}
		reg = config.Registry{
			Default: config.DefaultModelName,
			Models: []config.ModelSpec{{
				Name:    config.DefaultModelName,
				Model:   cfg.Model,
				APIBase: cfg.BaseURL,
				APIKey:  cfg.APIKey,
				Enabled: true,
			}},
		}
		path = ""
	}
	build := func(m config.ModelSpec) domain.Provider {
		hc := http.DefaultClient
		if m.TimeoutSec > 0 {
			hc = &http.Client{Timeout: time.Duration(m.TimeoutSec) * time.Second}
		}
		c := openai.New(m.APIBase, m.APIKey, hc)
		c.ExtraBody = m.ExtraBody
		c.Headers = m.Headers
		return c
	}
	return router.New(reg, path, build, load, logger.Printf), nil
}

// tools assembles the tool set.
//
// A tool that cannot work is ABSENT rather than present-and-explaining-itself.
// Telling the model about web_search and then answering "not configured" costs
// a whole turn to discover something the boot already knew.
//
// The set is fixed at boot even though the model registry reloads. Adding or
// removing a tool changes what the model was told it could do partway through a
// conversation, and a conversation that has already been offered a capability
// should not silently lose it -- a container recreate is the honest way to
// change the tool set, and the proxy already recreates on a bind change.
func tools(workspace, self string, reg config.Registry, logger *log.Logger) []tool.Tool {
	// load_image is unconditional: an image in the workspace is something any
	// deployment can have, and the tool costs nothing when none is there. What
	// varies is whether a model can SEE the result, which the vision chain
	// decides at completion time rather than here.
	out := []tool.Tool{shellTool(workspace, self), loadimage.New(workspace)}
	if s := websearch.New(reg.Web, nil, logger.Printf); s != nil {
		out = append(out, s, websearch.NewFetch(reg.Web.FetchLimitBytes, logger.Printf))
		logger.Printf("tools: web_search and web_fetch enabled")
	}
	if g := imagegen.New(reg, workspace, nil, logger.Printf); g != nil {
		out = append(out, g)
		logger.Printf("tools: generate_image enabled")
	}
	return out
}

func systemPrompt(cfg config.Config, logger *log.Logger) string {
	if cfg.SystemFile == "" {
		return cfg.System
	}
	b, err := os.ReadFile(cfg.SystemFile)
	if err != nil {
		logger.Printf("persona file %s unreadable, falling back to GANGLION_SYSTEM: %v", cfg.SystemFile, err)
		return cfg.System
	}
	return string(b)
}

// shellTool is the ONE place the shell tool is constructed, and it always sets
// Self. There is no branch here and no configuration reaching it: an operator
// cannot turn the sandbox off, because there is nothing to turn.
func shellTool(workspace, self string) *exec.Tool {
	t := exec.New(workspace)
	t.Self = self
	return t
}

// encrypt turns a plaintext on stdin into the enc:// value to paste into the
// deployment's environment.
//
// stdin rather than an argument: an argument is in the shell history, in the
// process list, and in any terminal recording.
func encrypt() error {
	pt, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	plain := strings.TrimRight(string(pt), "\r\n")
	if plain == "" {
		return fmt.Errorf("nothing on stdin; pipe the credential in, do not pass it as an argument")
	}
	// The two factors read directly, not through config.Load: encrypting a
	// value must work on a machine that has no model, no provider and no
	// transcript directory, and Load rightly refuses to return cleanly there.
	keyFile := os.Getenv("GANGLION_KEY_FILE")
	if keyFile == "" {
		keyFile = config.DefaultKeyFile
	}
	v, err := secret.Seal(secret.Resolver{
		Passphrase: os.Getenv("GANGLION_KEY_PASSPHRASE"),
		KeyFile:    keyFile,
	}, plain)
	if err != nil {
		return err
	}
	fmt.Println(v)
	return nil
}
