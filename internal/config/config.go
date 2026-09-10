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

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/secret"
)

// DefaultKeyFile is where crab-shell-proxy binds the credential key file.
//
// Beside the harness's data root and NOT under its workspace: the workspace is
// the only thing the container mounts and the only hierarchy the Landlock
// ruleset grants, so the agent can reach neither this path nor the directory
// holding it. That placement is the second factor -- put it inside the
// workspace and the scheme degenerates to one.
const DefaultKeyFile = "/data/.ganglion/credential.key"

type Config struct {
	Addr      string
	AuthToken string

	Model   string
	BaseURL string
	APIKey  string
	// ConfigFile is the structural half of the configuration: the model
	// registry and, later, the tool and evolution blocks. See file.go for why
	// this one thing is not read from the environment.
	ConfigFile string
	// KeyPassphrase and KeyFile are the two factors that resolve an enc://
	// value. They are NOT credentials themselves -- either one alone
	// decrypts nothing.
	KeyPassphrase string
	KeyFile       string
	System        string
	SystemFile    string
	// SkillsRoot is the ADMIN's shared skills directory, mounted read-only by
	// crab-shell-proxy. Empty means none is bound, which is every container
	// that has not been recreated since the mount was added -- and the agent's
	// own <workspace>/skills still loads either way.
	SkillsRoot string
	// Lifecycle is the mode crab-shell-proxy runs this container in --
	// "scale-to-zero" or "continuous". The harness cannot observe it and needs
	// it for exactly one decision (R11): a scheduled analysis pass on a
	// container that stops when idle fires nothing.
	Lifecycle   string
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
		KeyPassphrase:    os.Getenv("GANGLION_KEY_PASSPHRASE"),
		KeyFile:          env("GANGLION_KEY_FILE", DefaultKeyFile),
		ConfigFile:       env("GANGLION_CONFIG_FILE", DefaultConfigFile),
		System:           os.Getenv("GANGLION_SYSTEM"),
		SystemFile:       os.Getenv("GANGLION_SYSTEM_FILE"),
		SkillsRoot:       os.Getenv("GANGLION_SKILLS_ROOT"),
		Lifecycle:        os.Getenv("GANGLION_LIFECYCLE_MODE"),
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
	// The three variables are required only when nothing else can supply a
	// model. A deployment that mounts a config file with a model_list has
	// already answered the question these errors ask, and demanding them
	// anyway would mean every such deployment carrying a decorative value.
	//
	// The check is on the FILE'S EXISTENCE rather than on its contents,
	// deliberately: parsing happens after enc:// resolution, and a config file
	// that exists but declares nothing usable must fail as "your registry is
	// empty", not as "GANGLION_MODEL is required" -- which would send an
	// operator to fix the wrong thing.
	if !fileExists(c.ConfigFile) {
		if c.BaseURL == "" {
			return c, fmt.Errorf("GANGLION_BASE_URL is required when no config file is mounted at %s: "+
				"there is no provider registry mapping a name to an endpoint", c.ConfigFile)
		}
		if c.Model == "" {
			return c, fmt.Errorf("GANGLION_MODEL is required when no config file is mounted at %s", c.ConfigFile)
		}
	}

	// enc:// values are resolved HERE, at load, so a wrong passphrase is a
	// boot failure naming the variable rather than a 401 from the provider
	// on some member's first message. Same reasoning as BaseURL above.
	res := secret.Resolver{Passphrase: c.KeyPassphrase, KeyFile: c.KeyFile}
	for _, f := range []struct {
		name string
		p    *string
	}{
		{"GANGLION_API_KEY", &c.APIKey},
		{"GANGLION_TOKEN", &c.AuthToken},
	} {
		v, err := res.Resolve(*f.p)
		if err != nil {
			return c, fmt.Errorf("%s: %w", f.name, err)
		}
		*f.p = v
	}
	return c, nil
}

// fileExists reports whether a path is a readable regular file. An unreadable
// or directory path counts as absent: the caller's next move -- fall back to
// the environment -- is right in every one of those cases.
func fileExists(path string) bool {
	if path == "" {
		return false
	}
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
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
