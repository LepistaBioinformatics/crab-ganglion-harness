package jsonl

// The one-time move from the old project layout to the new one.
//
// A project's subtree was <workspace>/projects/<id> for one release and is
// <workspace>/../workspace-<id> now. A member who already has transcripts must
// not lose them to a rename -- which is the same reason the proxy carries
// MigratePublicDir for its own earlier rename.
//
// It lives in the HARNESS rather than the proxy because the harness is what
// wrote those directories and what names them: the proxy would have to restate
// both layouts to move them, and a restatement is what the sibling layout exists
// to remove.

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// legacyProjectsDirName is where a project's subtree used to live, under the
// workspace. Kept only so the migration can find one; nothing else may use it.
const legacyProjectsDirName = "projects"

// MigrateProjects moves every <workspace>/projects/<id> to workspace-<id> and
// returns how many it moved.
//
// Idempotent by construction: the legacy directory is gone after a successful
// pass, so the next boot reads nothing and does nothing. A workspace that never
// had the old layout -- every deployment with no projects, and every one created
// after this -- does one stat and returns.
//
// A destination that ALREADY EXISTS is not overwritten and not merged. Both
// would be guesses about which copy is current, and the cost of guessing wrong
// is a member's transcripts. The legacy directory is left in place instead and
// the caller is told, so the operator has something to look at.
func MigrateProjects(workspace string) (moved int, err error) {
	legacy := filepath.Join(workspace, legacyProjectsDirName)
	entries, rerr := os.ReadDir(legacy)
	if os.IsNotExist(rerr) {
		return 0, nil
	}
	if rerr != nil {
		return 0, fmt.Errorf("scan the legacy project directory: %w", rerr)
	}

	var stuck []string
	for _, e := range entries {
		if !e.IsDir() || !domain.ValidProject(e.Name()) {
			continue
		}
		dst := domain.ProjectWorkspace(workspace, e.Name())
		if _, serr := os.Stat(dst); serr == nil {
			stuck = append(stuck, e.Name())
			continue
		}
		if rerr := os.Rename(filepath.Join(legacy, e.Name()), dst); rerr != nil {
			// One project that cannot move must not stop the others: this runs
			// at boot, and a workspace half-migrated is better than a container
			// that will not start.
			stuck = append(stuck, e.Name())
			continue
		}
		moved++
	}
	if len(stuck) > 0 {
		return moved, fmt.Errorf("left in place, a destination already exists or the move failed: %v", stuck)
	}
	// Only when everything moved, and only when empty -- os.Remove refuses a
	// directory that still holds anything, which is the check we want rather
	// than one written here.
	_ = os.Remove(legacy)
	return moved, nil
}
