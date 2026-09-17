// Package snaptree gives a snapshot's index a shape a file browser can walk.
//
// The index is flat, sorted by path, zstd-compressed and streaming-only: there
// is no random access and no way to ask what is inside one directory. `amp-bb
// ls` answers that by streaming the whole thing and filtering on a prefix,
// which is right for one question and wrong for a tree view, where a person
// expanding twenty directories would pay for twenty full decompressions.
//
// So the tree is built once per snapshot and cached. On the production
// instance an index holds about 3 700 entries and streams in a tenth of a
// second, so this is comfort rather than necessity -- but the comfort is what
// makes ticking boxes feel like a file manager instead of a query language.
package snaptree

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/repo"
)

// Node is one entry as a browser wants it.
type Node struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// Type is "f", "d" or "l", matching repo.EntryType.
	Type    string `json:"type"`
	Size    int64  `json:"size,omitempty"`
	ModTime int64  `json:"mtime,omitempty"`
	Target  string `json:"target,omitempty"`

	// Children and SubBytes describe what is below a directory, so that the
	// cost of ticking it is visible without expanding it first.
	Children int   `json:"children,omitempty"`
	SubBytes int64 `json:"sub_bytes,omitempty"`
}

// IsDir reports whether the node is a directory.
func (n Node) IsDir() bool { return n.Type == string(repo.TypeDir) }

// Tree is one snapshot's directory structure.
type Tree struct {
	snapshot string
	children map[string][]Node
	byPath   map[string]Node
	entries  int
	bytes    int64
	built    time.Duration
}

// Snapshot returns the id this tree describes.
func (t *Tree) Snapshot() string { return t.snapshot }

// Entries is how many index entries went into it.
func (t *Tree) Entries() int { return t.entries }

// Bytes is the total logical size of the files in it.
func (t *Tree) Bytes() int64 { return t.bytes }

// BuildTime is how long the single streaming pass took.
func (t *Tree) BuildTime() time.Duration { return t.built }

// List returns one page of a directory's children, directories first and then
// by name.
//
// Paging is not optional: a Minecraft world's region directory really does
// hold thousands of files, and sending all of them to a browser to render is
// how a tab stops responding.
func (t *Tree) List(dir string, offset, limit int) (nodes []Node, total int, ok bool) {
	dir = normalise(dir)
	kids, ok := t.children[dir]
	if !ok {
		return nil, 0, false
	}
	total = len(kids)
	if offset < 0 {
		offset = 0
	}
	if offset >= total {
		return nil, total, true
	}
	end := total
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	return slices.Clone(kids[offset:end]), total, true
}

// Stat returns one node.
func (t *Tree) Stat(p string) (Node, bool) {
	n, ok := t.byPath[normalise(p)]
	return n, ok
}

// Search returns entries whose name contains q, case-insensitively.
//
// It is a substring match on the base name rather than a pattern, because the
// person typing into the box is looking for a file they half-remember, not
// writing a rule.
func (t *Tree) Search(q string, limit int) []Node {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return nil
	}
	if limit <= 0 {
		limit = 100
	}
	var out []Node
	for _, kids := range t.children {
		for _, n := range kids {
			if strings.Contains(strings.ToLower(n.Name), q) {
				out = append(out, n)
				if len(out) >= limit*4 {
					break
				}
			}
		}
	}
	slices.SortFunc(out, func(a, b Node) int { return cmp.Compare(a.Path, b.Path) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// ErrTooLarge is returned for an index bigger than the cache is willing to
// hold in memory. An out-of-memory kill would take the scheduler with it.
var ErrTooLarge = errors.New("snaptree: snapshot has more entries than the tree cache allows")

// Build reads an index and assembles the tree.
func Build(ctx context.Context, r *repo.Repository, snapshot string, maxEntries int) (*Tree, error) {
	started := time.Now()
	ir, closeIdx, err := r.OpenIndex(snapshot)
	if err != nil {
		return nil, err
	}
	defer closeIdx()

	t := &Tree{
		snapshot: snapshot,
		children: map[string][]Node{"": {}},
		byPath:   map[string]Node{},
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		e, err := ir.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if maxEntries > 0 && t.entries >= maxEntries {
			return nil, fmt.Errorf("%w (limit %d)", ErrTooLarge, maxEntries)
		}
		t.add(e)
	}

	t.rollUp()
	t.sort()
	t.built = time.Since(started)
	return t, nil
}

func (t *Tree) add(e repo.Entry) {
	t.entries++
	if e.Type == repo.TypeFile {
		t.bytes += e.Size
	}

	node := Node{
		Name:    path.Base(e.Path),
		Path:    e.Path,
		Type:    string(e.Type),
		Size:    e.Size,
		ModTime: e.ModTime,
		Target:  e.Target,
	}
	t.byPath[e.Path] = node

	parent := parentOf(e.Path)
	t.ensureDir(parent)
	t.children[parent] = append(t.children[parent], node)
	if e.Type == repo.TypeDir {
		if _, ok := t.children[e.Path]; !ok {
			t.children[e.Path] = nil
		}
	}
}

// ensureDir invents a parent the index did not name.
//
// scan.Walk emits a directory entry for every directory, so this does nothing
// today. It is four lines, and it is what stops the file browser going subtly
// wrong against an index written by some future version of the scanner.
func (t *Tree) ensureDir(dir string) {
	for dir != "" {
		if _, ok := t.children[dir]; ok {
			return
		}
		t.children[dir] = nil
		if _, ok := t.byPath[dir]; !ok {
			node := Node{Name: path.Base(dir), Path: dir, Type: string(repo.TypeDir)}
			t.byPath[dir] = node
			parent := parentOf(dir)
			t.children[parent] = append(t.children[parent], node)
		}
		dir = parentOf(dir)
	}
}

// rollUp totals each directory's contents up the chain, so that a browser can
// show what ticking a directory would cost without expanding it.
func (t *Tree) rollUp() {
	totals := make(map[string]*Node, len(t.children))
	for p, node := range t.byPath {
		if node.IsDir() {
			copied := node
			totals[p] = &copied
		}
	}
	for p, node := range t.byPath {
		if node.IsDir() {
			continue
		}
		for dir := parentOf(p); ; dir = parentOf(dir) {
			if agg, ok := totals[dir]; ok {
				agg.Children++
				agg.SubBytes += node.Size
			}
			if dir == "" {
				break
			}
		}
	}
	// Write the totals back into both views of each directory.
	for p, agg := range totals {
		t.byPath[p] = *agg
		siblings := t.children[parentOf(p)]
		for i := range siblings {
			if siblings[i].Path == p {
				siblings[i] = *agg
			}
		}
	}
}

func (t *Tree) sort() {
	for dir := range t.children {
		slices.SortFunc(t.children[dir], func(a, b Node) int {
			// Directories first: that is what every file manager does, and a
			// world folder buried between four thousand region files is not
			// findable.
			if a.IsDir() != b.IsDir() {
				if a.IsDir() {
					return -1
				}
				return 1
			}
			return cmp.Compare(a.Name, b.Name)
		})
	}
}

// parentOf returns the directory holding p, with "" for the root.
func parentOf(p string) string {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return ""
	}
	return p[:i]
}

// normalise accepts the several ways a browser might name the root.
func normalise(p string) string {
	p = strings.Trim(p, "/")
	if p == "." {
		return ""
	}
	return p
}

// Cache holds a few trees, so that expanding a directory is a map lookup.
type Cache struct {
	maxTrees   int
	idle       time.Duration
	maxEntries int
	clock      func() time.Time

	mu      sync.Mutex
	entries map[string]*cacheEntry
}

type cacheEntry struct {
	// once makes concurrent requests for the same cold snapshot share one
	// build. Two browser tabs opening the same snapshot must not scan twice.
	once     sync.Once
	tree     *Tree
	err      error
	lastUsed time.Time
}

// NewCache builds a cache. maxTrees is how many snapshots to keep, idle is how
// long an unused one survives, maxEntries bounds a single tree.
func NewCache(maxTrees int, idle time.Duration, maxEntries int) *Cache {
	if maxTrees <= 0 {
		maxTrees = 3
	}
	if idle <= 0 {
		idle = 5 * time.Minute
	}
	if maxEntries <= 0 {
		maxEntries = 2_000_000
	}
	return &Cache{
		maxTrees: maxTrees, idle: idle, maxEntries: maxEntries,
		clock: time.Now, entries: map[string]*cacheEntry{},
	}
}

// SetClock replaces the cache's clock, for tests.
func (c *Cache) SetClock(clock func() time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clock = clock
}

// Get returns a snapshot's tree, building it if necessary.
func (c *Cache) Get(ctx context.Context, r *repo.Repository, snapshot string) (*Tree, error) {
	c.mu.Lock()
	c.evictLocked()
	entry, ok := c.entries[snapshot]
	if !ok {
		entry = &cacheEntry{}
		c.entries[snapshot] = entry
	}
	entry.lastUsed = c.clock()
	c.mu.Unlock()

	entry.once.Do(func() {
		entry.tree, entry.err = Build(ctx, r, snapshot, c.maxEntries)
	})

	if entry.err != nil {
		// A failed build is not cached: the next caller deserves a fresh
		// attempt rather than a permanent error from a context that was
		// cancelled once.
		c.Invalidate(snapshot)
		return nil, entry.err
	}
	return entry.tree, nil
}

// Invalidate drops a snapshot from the cache.
func (c *Cache) Invalidate(snapshot string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, snapshot)
}

// Len reports how many trees are held.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// evictLocked drops idle and surplus trees. Runs with the mutex held.
func (c *Cache) evictLocked() {
	now := c.clock()
	for id, entry := range c.entries {
		if now.Sub(entry.lastUsed) > c.idle {
			delete(c.entries, id)
		}
	}
	for len(c.entries) >= c.maxTrees {
		var oldestID string
		var oldest time.Time
		for id, entry := range c.entries {
			if oldestID == "" || entry.lastUsed.Before(oldest) {
				oldestID, oldest = id, entry.lastUsed
			}
		}
		if oldestID == "" {
			return
		}
		delete(c.entries, oldestID)
	}
}
