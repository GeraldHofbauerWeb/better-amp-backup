package exclude

import (
	"bufio"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// AMPExcludeFile is the name AMP uses for its per-directory exclusion lists.
const AMPExcludeFile = ".backupExclude"

// Which AMP versions actually read these files is version-dependent, and worth
// knowing before relying on them. On a 2.8 Minecraft instance the string
// ".backupExclude" appears only in GenericModule; the backup plugin builds its
// exclusion set from an internal map (manualExclusions / autoExclusions) that
// is written through FileManagerPlugin.ChangeExclusion, so a hand-placed file
// is inert there. Measured directly: an instance with these files in place
// still had every excluded path in AMP's next ZIP.
//
// They are still parsed here, because they are the documented convention, they
// do work on other module types and older builds, and reading them costs
// nothing. But do not promise a user that writing one shrinks AMP's own
// backups — use the API for that.
//
// AMP's semantics differ from gitignore in three ways that matter, all of
// which are reproduced here so that a user who has curated exclusions for
// AMP's own backups gets them honoured without re-writing anything:
//
//  1. A rule applies only to the directory holding the file. There is no
//     inheritance into subdirectories.
//  2. A rule names an entry in that directory. Sub-paths ("dir/file") are not
//     supported by AMP and are rejected rather than silently reinterpreted.
//  3. An empty file excludes the whole directory it sits in.
//
// ParseAMPExcludeFile turns one such file into patterns anchored at the
// snapshot root. dir is the file's directory, relative to the root and
// slash-separated; use "" for the root itself.
func ParseAMPExcludeFile(r io.Reader, dir string) ([]string, error) {
	dir = strings.Trim(strings.ReplaceAll(dir, string(filepath.Separator), "/"), "/")

	var rules []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, "/") {
			return nil, fmt.Errorf("%s in %q: rule %q contains a path separator, "+
				"which AMP does not support (put a %s in that subdirectory instead)",
				AMPExcludeFile, dirLabel(dir), line, AMPExcludeFile)
		}
		if dir == "" {
			rules = append(rules, line)
		} else {
			rules = append(rules, dir+"/"+line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s in %q: %w", AMPExcludeFile, dirLabel(dir), err)
	}

	// An empty list means "exclude this whole directory". At the root that
	// would exclude everything, which is almost certainly a mistake rather
	// than an instruction, so it is reported instead of obeyed.
	if len(rules) == 0 {
		if dir == "" {
			return nil, fmt.Errorf("%s at the instance root is empty, which would "+
				"exclude the entire instance; remove the file or list entries in it",
				AMPExcludeFile)
		}
		return []string{dir}, nil
	}
	return rules, nil
}

func dirLabel(dir string) string {
	if dir == "" {
		return "."
	}
	return dir
}

// CollectAMPExcludes walks root and gathers every .backupExclude into one
// pattern list anchored at root.
//
// The walk deliberately does not skip directories that earlier rules already
// excluded: an exclusion file inside an excluded directory costs nothing to
// read, and skipping would make the result depend on traversal order.
func CollectAMPExcludes(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory we cannot read is a problem for the backup itself and
			// will be reported there; do not fail exclusion discovery over it.
			if os.IsPermission(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || d.Name() != AMPExcludeFile {
			return nil
		}

		rel, rerr := filepath.Rel(root, filepath.Dir(p))
		if rerr != nil {
			return fmt.Errorf("exclude: locate %s: %w", p, rerr)
		}
		if rel == "." {
			rel = ""
		}

		f, oerr := os.Open(p)
		if oerr != nil {
			return fmt.Errorf("exclude: open %s: %w", p, oerr)
		}
		defer f.Close()

		rules, perr := ParseAMPExcludeFile(f, filepath.ToSlash(rel))
		if perr != nil {
			return perr
		}
		out = append(out, rules...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RenderAMPExcludeFiles is the inverse: it groups root-anchored patterns by
// the directory AMP expects them in. Patterns that cannot be expressed in
// AMP's format — anything using ** or a negation — are returned separately so
// the caller can tell the user what will not carry over.
func RenderAMPExcludeFiles(patterns []string) (files map[string][]string, unsupported []string) {
	files = make(map[string][]string)
	for _, raw := range patterns {
		p := strings.TrimSpace(raw)
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		if strings.HasPrefix(p, "!") || strings.Contains(p, "**") {
			unsupported = append(unsupported, raw)
			continue
		}
		dir, name := path.Split(strings.TrimSuffix(p, "/"))
		dir = strings.Trim(dir, "/")
		if name == "" {
			unsupported = append(unsupported, raw)
			continue
		}
		files[dir] = append(files[dir], name)
	}
	return files, unsupported
}
