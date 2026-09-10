// Package config reads the harness's settings from the environment.
//
// Environment rather than a file, because the proxy already materializes
// per-user secrets through its generic dotenv sink -- reusing that means the
// provisioner needs no harness-specific secret path (FR-16).
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr      string
	AuthToken string

	Model       string
	BaseURL     string
	APIKey      string
	System      string
	SystemFile  string
	DataDir     string
	MaxTurnIter int

	ApprovalEndpoint string
	ApprovalTimeout  time.Duration
	GatedTools       []string

	OTLPEndpoint string
}

// Load reads the environment. It fails loudly on what cannot be defaulted:
// BaseURL has no provider registry to fall back on, and an unset one surfaces
// as a 404 from the provider rather than as a configuration error -- a lesson
// the Hermes work paid for.
func Load() (Config, error) {
	c := Config{
		Addr:             env("GANGLION_ADDR", ":18800"),
		AuthToken:        os.Getenv("GANGLION_TOKEN"),
		Model:            env("GANGLION_MODEL", ""),
		BaseURL:          os.Getenv("GANGLION_BASE_URL"),
		APIKey:           os.Getenv("GANGLION_API_KEY"),
		System:           os.Getenv("GANGLION_SYSTEM"),
		SystemFile:       os.Getenv("GANGLION_SYSTEM_FILE"),
		DataDir:          env("GANGLION_DATA_DIR", "/data/.ganglion"),
		MaxTurnIter:      envInt("GANGLION_MAX_ITERATIONS", 12),
		ApprovalEndpoint: os.Getenv("GANGLION_APPROVAL_ENDPOINT"),
		ApprovalTimeout:  time.Duration(envInt("GANGLION_APPROVAL_TIMEOUT_SECONDS", 300)) * time.Second,
		OTLPEndpoint:     os.Getenv("GANGLION_OTLP_ENDPOINT"),
	}
	if g := os.Getenv("GANGLION_GATED_TOOLS"); g != "" {
		for _, n := range strings.Split(g, ",") {
			if n = strings.TrimSpace(n); n != "" {
				c.GatedTools = append(c.GatedTools, n)
			}
		}
	}
	if c.BaseURL == "" {
		return c, fmt.Errorf("GANGLION_BASE_URL is required: there is no provider registry mapping a name to an endpoint")
	}
	if c.Model == "" {
		return c, fmt.Errorf("GANGLION_MODEL is required")
	}
	return c, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
