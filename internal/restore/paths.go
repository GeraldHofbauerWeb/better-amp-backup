package restore

import (
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/repo"
)

// selection restricts a restore to a set of named paths and everything beneath
// each of them.
//
// It exists because a glob cannot express what a file browser produces. The
// pattern engine in internal/exclude has no escape for its metacharacters, and
// a modpack is full of names like "[1.21.1] Some Mod.jar" -- there is no
// pattern that selects that file and only that file. A pattern without a slash
// also matches at any depth, so ticking a top-level "mods" would quietly drag
// in "config/foo/mods" as well. A checked box is a path, not a pattern, and
// round-tripping it through one loses information.
type selection struct {
	// paths is prefix-free and sorted, so the only candidate prefix of any
	// entry is the greatest element not greater than it -- one binary search
	// per entry rather than a scan.
	paths []string
	// dirs holds every proper ancestor directory of a selection. They are
	// restored so that a selected file lands under directories with the right
	// mode and time, but they do not pull in their other children.
	dirs map[string]bool
	// matched records which selections found something, so that a selection
	// naming a path the snapshot does not hold can be reported rather than
	// silently restoring nothing.
	matched map[string]bool
}

// newSelection validates and normalises the caller's paths.
func newSelection(paths []string) (*selection, error) {
	if len(paths) == 0 {
		return nil, nil
	}

	cleaned := make([]string, 0, len(paths))
	for _, p := range paths {
		c, err := canonicalSelectionPath(p)
		if err != nil {
			return nil, err
		}
		cleaned = append(cleaned, c)
	}
	sort.Strings(cleaned)
	cleaned = slices.Compact(cleaned)

	// Collapse anything already covered by a shorter selection. Sorted order
	// puts a parent immediately before its descendants, so one pass does it.
	// The result being prefix-free is what makes the lookup a single probe.
	prefixFree := cleaned[:1]
	for _, p := range cleaned[1:] {
		if last := prefixFree[len(prefixFree)-1]; strings.HasPrefix(p, last+"/") {
			continue
		}
		prefixFree = append(prefixFree, p)
	}

	s := &selection{paths: prefixFree, dirs: map[string]bool{}, matched: map[string]bool{}}
	for _, p := range prefixFree {
		for dir := path.Dir(p); dir != "." && dir != "/"; dir = path.Dir(dir) {
			s.dirs[dir] = true
		}
	}
	return s, nil
}

// canonicalSelectionPath applies the same rules repo.Entry.Validate applies to
// an index entry. A path that fails them is a client bug or an attack, not
// something to quietly repair.
func canonicalSelectionPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("restore: empty path in selection")
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("restore: path %q is absolute; selections are relative to the snapshot root", p)
	}
	if p != path.Clean(p) {
		return "", fmt.Errorf("restore: path %q is not in canonical form", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("restore: path %q escapes the snapshot root", p)
		}
	}
	return p, nil
}

// keep reports whether an entry belongs in the restore, recording the
// selection responsible so that unmatched ones can be reported afterwards.
func (s *selection) keep(e repo.Entry) bool {
	i := sort.SearchStrings(s.paths, e.Path)
	if i < len(s.paths) && s.paths[i] == e.Path {
		s.matched[e.Path] = true
		return true
	}
	if i > 0 {
		if parent := s.paths[i-1]; strings.HasPrefix(e.Path, parent+"/") {
			s.matched[parent] = true
			return true
		}
	}
	// An ancestor is carried along for its metadata, but it is not evidence
	// that the selection below it exists.
	return e.Type == repo.TypeDir && s.dirs[e.Path]
}

// unmatched returns the selections that found nothing.
func (s *selection) unmatched() []string {
	var out []string
	for _, p := range s.paths {
		if !s.matched[p] {
			out = append(out, p)
		}
	}
	return out
}
