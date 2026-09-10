// Package skillfile is the SKILL.md format: where they live and how their
// frontmatter reads.
//
// A package of its own rather than a corner of the skills adapter, because TWO
// adapters need it -- the loader that reads skills into the prompt, and
// evolution, which writes them -- and internal/domain/arch_test.go forbids one
// adapter importing another (AR-4). That rule caught this exact coupling on the
// first build, which is what it is for.
//
// It is the FORMAT, not the policy. Nothing here decides which skills win a
// name collision, what the prompt budget is, or whether a draft may be written;
// each of those lives with the adapter that owns the decision.
//
// The format is picoclaw's, unchanged, so a skill written by one harness's
// evolution loads in the other.
package skillfile

import "strings"

// DirName is the skills directory inside a workspace.
const DirName = "skills"

// FileName is the file inside each skill's directory.
const FileName = "SKILL.md"

// Frontmatter pulls `name` and `description` out of a SKILL.md.
//
// Deliberately NOT a YAML parser. picoclaw's own validator rejects any
// frontmatter field other than these two (apply.go, validateAppliedSkillBody),
// so the grammar this has to read is two `key: value` lines between `---`
// fences -- and a YAML dependency for that would be the only third-party import
// in a module that has none.
func Frontmatter(body string) (name, description string) {
	rest, ok := strings.CutPrefix(strings.TrimLeft(body, "\ufeff \t\r\n"), "---")
	if !ok {
		return "", ""
	}
	rest = strings.TrimPrefix(rest, "\n")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", ""
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		k, v, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch strings.TrimSpace(k) {
		case "name":
			name = v
		case "description":
			description = v
		}
	}
	return name, description
}
