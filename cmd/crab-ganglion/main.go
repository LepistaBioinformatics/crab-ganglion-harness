// Command crab-ganglion is the harness binary.
//
// This file is the composition root: the ONLY place where adapters meet. Every
// other package knows the domain and nothing else, which is what AR-4's test
// enforces and what keeps a second provider or a second ingress a file rather
// than a refactor.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/adapter/approver/proxy"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/adapter/httpsse"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/adapter/provider/openai"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/adapter/store/jsonl"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/adapter/store/window"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/adapter/tool"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/adapter/tool/exec"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/config"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/runtime"
)

func main() {
	logger := log.New(os.Stderr, "ganglion ", log.LstdFlags|log.LUTC)

	cfg, err := config.Load()
	if err != nil {
		logger.Fatalf("config: %v", err)
	}

	workspace := filepath.Join(cfg.DataDir, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		logger.Fatalf("workspace: %v", err)
	}

	loop := &runtime.Loop{
		Provider:        openai.New(cfg.BaseURL, cfg.APIKey, nil),
		Transcript:      jsonl.New(filepath.Join(cfg.DataDir, "sessions")),
		Context:         window.New(filepath.Join(cfg.DataDir, "windows")),
		Tools:           tool.NewRegistry(exec.New(workspace)),
		Model:           cfg.Model,
		System:          systemPrompt(cfg, logger),
		MaxIterations:   cfg.MaxTurnIter,
		ApprovalTimeout: cfg.ApprovalTimeout,
	}
	// Left unset, the loop installs an allow-all approver. Assigned only when
	// an endpoint exists, so a typed nil can never reach the port.
	if cfg.ApprovalEndpoint != "" {
		loop.Approver = proxy.New(cfg.ApprovalEndpoint, cfg.AuthToken, cfg.GatedTools, nil)
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
