package domain

// Projects, the part of them the domain owns: which project a turn belongs to,
// and the alphabet a project id may use.
//
// The id is a PATH SEGMENT by the time anything else touches it -- the
// transcript's directory, the window's, the shell's working directory -- and it
// arrives in a header. So the check lives here, at the boundary between "a
// string somebody sent" and "a name the filesystem will see", rather than at
// each of the four places that build a path from it.

import (
	"context"
	"path/filepath"
	"regexp"
)

// projectID is the alphabet crab-shell-proxy generates (agent-projects FR-5a):
// lowercase, digits, underscore and dash, never leading with a separator, and
// bounded so a path stays a path. `.` is excluded by construction, which is what
// makes `..` unrepresentable rather than merely refused.
var projectID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ValidProject reports whether an id may become a directory name. The empty
// string is NOT valid here; callers treat "no project" as a separate case,
// because a request that names a project and a request that names none are
// different requests and must not collapse into one.
func ValidProject(id string) bool { return projectID.MatchString(id) }

type projectKey struct{}

// WithProject attaches the turn's project to a context. The loop calls it once
// per turn; the stores and the workspace tools read it.
//
// In the context rather than threaded through, because the readers are a store
// port whose methods take (ctx, ConversationID) and a tool port whose Invoke
// takes (ctx, args) -- widening either would make every implementation carry a
// parameter that only project-aware ones use.
func WithProject(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, projectKey{}, id)
}

// ProjectFrom returns the turn's project, or "" for the main workspace.
func ProjectFrom(ctx context.Context) string {
	id, _ := ctx.Value(projectKey{}).(string)
	return id
}

// ProjectRoot is the directory a workspace tool should work in for this turn.
//
// One function rather than the same three lines in exec, loadimage and
// imagegen, because the three MUST agree: generate_image writes where load_image
// reads, and the shell has to be able to see both. When they disagreed for the
// main workspace it was a bug; per project it would be an invisible one.
//
// The layout is <workspace>/../workspace-<id> -- a SIBLING of the main
// workspace, not a child of it -- with the main workspace as the case where no
// project is named, so a deployment with no projects gets the paths it has
// always had.
//
// Sibling because picoclaw derives a named agent's workspace that way itself
// (pkg/agent/instance.go, resolveAgentWorkspace), so the shape is a fact about
// this stack rather than a convention either side is free to restate. The rule
// is written down in the product repo's .claude/rules/harness-layout.md, and
// the proxy builds exactly these paths from the outside.
//
// It was <workspace>/projects/<id> for one release. That bought the ganglion a
// project with no new bind -- and cost a second layout, which cost more: every
// path helper on the proxy side had to branch on the harness, and a branch that
// is missing does not fail, it reads a directory that never exists and reports
// that the member has no history.
func ProjectRoot(ctx context.Context, workspace string) string {
	p := ProjectFrom(ctx)
	if p == "" || !ValidProject(p) {
		return workspace
	}
	return ProjectWorkspace(workspace, p)
}

// ProjectWorkspace is the sibling directory for one project id.
//
// Separated from ProjectRoot because the boot needs it without a context: the
// stores are built once, per project, before any turn exists.
//
// filepath.Dir, not a stored parent: the workspace is the one path this harness
// is given, and deriving the sibling from it keeps a single source for both.
func ProjectWorkspace(workspace, id string) string {
	return filepath.Join(filepath.Dir(workspace), ProjectWorkspacePrefix+id)
}

// ProjectWorkspacePrefix is what picoclaw prepends to a project id to name its
// workspace. Written once, because the proxy writes the same string from the
// other side and a disagreement produces an empty directory rather than an
// error.
const ProjectWorkspacePrefix = "workspace-"
