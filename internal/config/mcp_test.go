package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/secret"
)

// writeConfig drops a config.json into a temp dir and returns its path.
func writeMCPConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The picoclaw record, verbatim. `command: ""` is part of that shape and must
// not be mistaken for a stdio server -- if it were, the proxy's own writer would
// fail every ganglion boot.
const mcpConfigBody = `{
  "model_list": [{"model_name": "m", "params": {"base_url": "http://x/v1", "model": "m"}}],
  "tools": {"mcp": {"enabled": true, "servers": {
    "memory": {"enabled": true, "command": "", "type": "http",
      "url": "http://crab-shell-proxy:8080/v1/mcp",
      "headers": {"Authorization": "Bearer t"}}
  }}}
}`

func loadMCPFrom(t *testing.T, body string) (Registry, error) {
	t.Helper()
	return LoadRegistry(writeMCPConfig(t, body), secret.Resolver{}, func(string) string { return "" })
}

func TestLoadMCPReadsPicoclawsOwnShape(t *testing.T) {
	reg, err := loadMCPFrom(t, mcpConfigBody)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	if len(reg.MCP) != 1 {
		t.Fatalf("servers = %+v", reg.MCP)
	}
	got := reg.MCP[0]
	if got.Name != "memory" || got.URL != "http://crab-shell-proxy:8080/v1/mcp" {
		t.Errorf("server = %+v", got)
	}
	if got.Headers["Authorization"] != "Bearer t" {
		t.Errorf("headers = %+v", got.Headers)
	}
}

// An absent block is every deployment that has no memory graph, and must stay
// byte-identical to what it does today.
func TestLoadMCPAbsentBlockYieldsNoServers(t *testing.T) {
	reg, err := loadMCPFrom(t,
		`{"model_list":[{"model_name":"m","params":{"base_url":"http://x/v1","model":"m"}}]}`)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	if len(reg.MCP) != 0 {
		t.Fatalf("servers = %+v", reg.MCP)
	}
}

// Turning something off is an instruction, not a mistake: it must not refuse.
func TestLoadMCPHonoursDisabled(t *testing.T) {
	for name, body := range map[string]string{
		"the whole block": strings.Replace(mcpConfigBody, `"mcp": {"enabled": true`, `"mcp": {"enabled": false`, 1),
		"one server":      strings.Replace(mcpConfigBody, `"memory": {"enabled": true`, `"memory": {"enabled": false`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			reg, err := loadMCPFrom(t, body)
			if err != nil {
				t.Fatalf("a disabled server was refused: %v", err)
			}
			if len(reg.MCP) != 0 {
				t.Fatalf("servers = %+v", reg.MCP)
			}
		})
	}
}

// FR-C3. All three refusals name the server, because the name is the only handle
// an operator has -- and a silently absent memory is the failure the whole gate
// table exists to prevent.
func TestLoadMCPRefusesWhatItCannotServe(t *testing.T) {
	cases := map[string]string{
		"a stdio server": strings.Replace(mcpConfigBody,
			`"command": ""`, `"command": "npx @modelcontextprotocol/server-memory"`, 1),
		"a type that is not http": strings.Replace(mcpConfigBody,
			`"type": "http"`, `"type": "stdio"`, 1),
		"no type at all": strings.Replace(mcpConfigBody, `"type": "http",`, ``, 1),
		"no url": strings.Replace(mcpConfigBody,
			`"url": "http://crab-shell-proxy:8080/v1/mcp",`, ``, 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadMCPFrom(t, body)
			if err == nil {
				t.Fatal("boot was allowed to continue")
			}
			if !strings.Contains(err.Error(), `"memory"`) {
				t.Errorf("the refusal does not name the server: %v", err)
			}
		})
	}
}

// The servers arrive in a map. An order that changed between boots would change
// the model's prompt for no reason anyone could see.
func TestLoadMCPOrdersServersByName(t *testing.T) {
	body := `{
  "model_list": [{"model_name": "m", "params": {"base_url": "http://x/v1", "model": "m"}}],
  "tools": {"mcp": {"enabled": true, "servers": {
    "zeta":  {"type": "http", "url": "http://z/v1/mcp"},
    "alpha": {"type": "http", "url": "http://a/v1/mcp"},
    "mid":   {"type": "http", "url": "http://m/v1/mcp"}
  }}}
}`
	reg, err := loadMCPFrom(t, body)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	var names []string
	for _, s := range reg.MCP {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "alpha,mid,zeta" {
		t.Fatalf("order = %v", names)
	}
}
