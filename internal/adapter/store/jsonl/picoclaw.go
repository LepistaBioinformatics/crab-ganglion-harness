package jsonl

// Reading a transcript picoclaw wrote.
//
// THIS EXISTS FOR MIGRATION, and for nothing else. A member whose agent moves
// from picoclaw to this harness keeps their per-user directory: the same
// conversations, the same ids, the same workspace. What they do NOT keep is the
// file names -- picoclaw names a transcript after a hash of its own, which
// nothing here can compute.
//
// It can be FOUND, though, and by the same route crab-shell-proxy already uses.
// picoclaw stamps every session's *.meta.json with
// scope.values.chat = "direct:pico:<sessionKey>", and that sessionKey is exactly
// the conversation id the proxy hands this harness on the turn. So the marker is
// a value both sides already agree on, and the hash never has to be reproduced.
//
// Read-only. Nothing here writes picoclaw's shape: a migrated conversation's
// next turn is appended under THIS harness's name, and both files are then read
// as one conversation (Store.Read). Writing picoclaw's would mean maintaining a
// second format forever for the sake of a directory that has already moved.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// picoChatMarker is what picoclaw prepends to the session key in
// scope.values.chat. The proxy's internal/history names the same constant for
// the same reason: the side that BUILDS the marker and the side that matches it
// must not drift.
const picoChatMarker = "direct:pico:"

// picoCronPrefix marks a meta written by a SCHEDULED run rather than by the
// member talking to the agent. Excluded here for the reason the proxy excludes
// it: a cron run stamps the originating chat's marker, so a conversation that
// owns a daily task would otherwise read that task's transcript as its own.
const picoCronPrefix = "agent:cron-"

// picoMeta is the subset of a picoclaw *.meta.json this needs.
type picoMeta struct {
	Key   string `json:"key"`
	Scope struct {
		Values struct {
			Chat string `json:"chat"`
		} `json:"values"`
	} `json:"scope"`
}

// picoclawTranscripts returns the basenames of every picoclaw transcript
// carrying this conversation's marker, oldest first.
//
// EVERY file, not the first: picoclaw may continue one chat in a fresh session
// file and leave the old meta in place, so a marker legitimately matches several
// and reading one would drop the rest of the conversation. The proxy learned this
// the same way.
//
// Ordered by modification time, because that is the only ordering available --
// the names are hashes and carry no sequence.
func picoclawTranscripts(dir string, id string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	marker := picoChatMarker + id
	type match struct {
		name string
		mod  int64
	}
	var matches []match
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".meta.json") {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			continue
		}
		var meta picoMeta
		if json.Unmarshal(raw, &meta) != nil {
			continue
		}
		if meta.Scope.Values.Chat != marker || strings.HasPrefix(meta.Key, picoCronPrefix) {
			continue
		}
		base := strings.TrimSuffix(name, ".meta.json")
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		matches = append(matches, match{name: base, mod: info.ModTime().UnixNano()})
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].mod < matches[j].mod })

	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m.name)
	}
	return out
}
