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
// The layout is <workspace>/projects/<id>, with the main workspace as the case
// where no project is named -- so a deployment with no projects gets the paths
// it has always had.
func ProjectRoot(ctx context.Context, workspace string) string {
	p := ProjectFrom(ctx)
	if p == "" || !ValidProject(p) {
		return workspace
	}
	return filepath.Join(workspace, ProjectsDirName, p)
}

// ProjectsDirName is the reserved directory under the workspace that holds the
// per-project subtrees. Reserved: a conversation or a file named "projects" in
// the main workspace would otherwise collide with the tree.
const ProjectsDirName = "projects"
