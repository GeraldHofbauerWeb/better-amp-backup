// Package exclude decides which paths a snapshot skips.
//
// Three sources feed into that decision, in increasing precedence: a built-in
// profile for the AMP module, AMP's own per-directory .backupExclude files,
// and the user's configuration. They are all reduced to the same pattern set
// so that the walk only ever consults one matcher.
package exclude

import (
	"fmt"
	"regexp"
	"strings"
)

// Pattern is one compiled rule.
type Pattern struct {
	raw    string
	negate bool
	re     *regexp.Regexp
}

// String returns the rule as written.
func (p Pattern) String() string {
	if p.negate {
		return "!" + p.raw
	}
	return p.raw
}

// Set matches paths against an ordered list of rules. Later rules win, which
// is what makes "exclude a directory but keep one file inside it" expressible.
type Set struct {
	patterns []Pattern
}

// Compile builds a Set. Patterns use slash-separated paths relative to the
// snapshot root and support:
//
//   - any run of characters within one path segment
//     ?        one character within a path segment
//     **       any run of characters, crossing segment boundaries
//     !prefix  negation: re-include something an earlier rule excluded
//
// A pattern without a slash matches that name at any depth, the way
// .gitignore behaves. Every pattern also matches everything beneath what it
// names, so "bluemap/web" covers the whole rendered map, not just the
// directory entry.
//
// Blank lines and lines starting with '#' are ignored, so a Set can be built
// straight from a config file or a .backupExclude.
func Compile(patterns []string) (*Set, error) {
	s := &Set{}
	for _, raw := range patterns {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		p := Pattern{raw: line}
		if strings.HasPrefix(line, "!") {
			p.negate = true
			line = strings.TrimSpace(line[1:])
			p.raw = line
			if line == "" {
				return nil, fmt.Errorf("exclude: %q negates nothing", raw)
			}
		}

		re, err := compileGlob(line)
		if err != nil {
			return nil, fmt.Errorf("exclude: pattern %q: %w", raw, err)
		}
		p.re = re
		s.patterns = append(s.patterns, p)
	}
	return s, nil
}

// MustCompile is Compile for patterns known good at build time.
func MustCompile(patterns []string) *Set {
	s, err := Compile(patterns)
	if err != nil {
		panic(err)
	}
	return s
}

// Len reports how many rules the set holds.
func (s *Set) Len() int { return len(s.patterns) }

// Patterns returns the compiled rules in order.
func (s *Set) Patterns() []Pattern { return s.patterns }

// Match reports whether path is excluded. path must be slash-separated and
// relative to the snapshot root, with no leading slash.
func (s *Set) Match(path string) bool {
	path = strings.TrimPrefix(path, "/")
	excluded := false
	for _, p := range s.patterns {
		if p.re.MatchString(path) {
			excluded = !p.negate
		}
	}
	return excluded
}

// Merge returns a new Set with other's rules appended, so that the
// higher-precedence source is consulted last.
func (s *Set) Merge(other *Set) *Set {
	if other == nil {
		return s
	}
	merged := &Set{patterns: make([]Pattern, 0, len(s.patterns)+len(other.patterns))}
	merged.patterns = append(merged.patterns, s.patterns...)
	merged.patterns = append(merged.patterns, other.patterns...)
	return merged
}

// compileGlob turns one glob into an anchored regexp that also covers
// everything below the named path.
func compileGlob(glob string) (*regexp.Regexp, error) {
	glob = strings.TrimSuffix(glob, "/")
	if glob == "" {
		return nil, fmt.Errorf("empty pattern")
	}

	var b strings.Builder
	b.WriteString("^")

	// A pattern with no separator matches at any depth; one with a separator
	// is anchored at the snapshot root.
	if !strings.Contains(glob, "/") {
		b.WriteString("(?:.*/)?")
	}

	for i := 0; i < len(glob); i++ {
		switch c := glob[i]; c {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				i++
				// "**/" may match nothing at all, so that "**/cache" also
				// matches a top-level "cache".
				if i+1 < len(glob) && glob[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '[':
			// Character class, e.g. core.[0-9]* to catch JVM core dumps
			// without also catching core.conf. A leading ! negates, as in
			// shell globs.
			end := strings.IndexByte(glob[i:], ']')
			if end < 0 {
				return nil, fmt.Errorf("unclosed '[' in pattern")
			}
			body := glob[i+1 : i+end]
			if body == "" {
				return nil, fmt.Errorf("empty character class")
			}
			if strings.Contains(body, "/") {
				return nil, fmt.Errorf("character class may not contain '/'")
			}
			if body[0] == '!' {
				body = "^" + body[1:]
			}
			b.WriteString("[" + body + "]")
			i += end
		case '/':
			b.WriteString("/")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}

	// Everything under a matched path is matched too.
	b.WriteString("(?:/.*)?$")

	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, err
	}
	return re, nil
}
