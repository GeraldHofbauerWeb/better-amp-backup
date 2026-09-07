package repo

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/hash"
	"github.com/klauspost/compress/zstd"
)

// EntryType distinguishes the three things a snapshot can record.
type EntryType string

const (
	TypeFile    EntryType = "f"
	TypeDir     EntryType = "d"
	TypeSymlink EntryType = "l"
)

// Entry is one line of a snapshot index.
//
// The index is newline-delimited JSON rather than a binary format on purpose:
// `zstdcat foo.idx.zst | jq` is a debugging tool everyone already has, and at
// ~180 bytes per entry a full AMP instance still compresses to a couple of
// megabytes. Field names are terse because they repeat 58,000 times.
type Entry struct {
	// Path is slash-separated and relative to the snapshot root. It never
	// starts with a slash and never contains "..".
	Path string    `json:"p"`
	Type EntryType `json:"t"`

	Mode fs.FileMode `json:"m,omitzero"`
	UID  int         `json:"u,omitzero"`
	GID  int         `json:"g,omitzero"`

	// Size and ModTime describe the plaintext. ModTime is Unix nanoseconds and
	// is what the next snapshot's stat-diff compares against.
	Size    int64 `json:"s,omitzero"`
	ModTime int64 `json:"mt,omitzero"`

	// Inode is recorded so that a file swapped for another of identical size
	// and timestamp is still noticed. It deliberately errs towards reporting a
	// change: after a restore every inode differs, which costs one full re-read
	// but can never cause a modification to go unbacked-up.
	Inode uint64 `json:"i,omitzero"`

	// Hash addresses the content. Files only.
	Hash hash.Hash `json:"h,omitzero"`

	// Target is the link destination. Symlinks only.
	Target string `json:"tgt,omitzero"`
}

// Validate reports whether an entry is internally consistent. Reading a
// snapshot written by a future or broken version should fail here rather than
// halfway through a restore.
func (e Entry) Validate() error {
	if e.Path == "" {
		return fmt.Errorf("index: entry with empty path")
	}
	// Path safety is enforced here rather than only at restore time, so that a
	// crafted or corrupted index cannot be written in the first place and no
	// consumer has to remember to check.
	if strings.HasPrefix(e.Path, "/") {
		return fmt.Errorf("index: entry path %q is absolute", e.Path)
	}
	if e.Path != path.Clean(e.Path) {
		return fmt.Errorf("index: entry path %q is not in canonical form", e.Path)
	}
	for _, seg := range strings.Split(e.Path, "/") {
		if seg == ".." {
			return fmt.Errorf("index: entry path %q escapes the snapshot root", e.Path)
		}
	}
	switch e.Type {
	case TypeFile:
		if e.Hash.IsZero() && e.Size != 0 {
			return fmt.Errorf("index: file %q has no hash", e.Path)
		}
	case TypeDir, TypeSymlink:
	default:
		return fmt.Errorf("index: entry %q has unknown type %q", e.Path, e.Type)
	}
	if e.Type == TypeSymlink && e.Target == "" {
		return fmt.Errorf("index: symlink %q has no target", e.Path)
	}
	return nil
}

// indexHeader is the first line of every index, so that a reader can reject a
// format it does not understand before parsing 58,000 entries.
type indexHeader struct {
	Version  int    `json:"v"`
	Snapshot string `json:"snapshot"`
	Instance string `json:"instance"`
	Root     string `json:"root"`
}

const indexVersion = 1

// IndexWriter streams entries into a zstd-compressed JSONL file while hashing
// the compressed bytes, so the manifest can record what it committed to.
type IndexWriter struct {
	enc     *zstd.Encoder
	hasher  *hash.Hasher
	buf     *bufio.Writer
	json    *json.Encoder
	entries int64
	closed  bool
}

// NewIndexWriter wraps w. The caller keeps ownership of w and must close it
// after Close returns.
func NewIndexWriter(w io.Writer, snapshotID, instance, root string) (*IndexWriter, error) {
	hasher := hash.New()
	enc, err := zstd.NewWriter(io.MultiWriter(w, hasher),
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderConcurrency(1),
	)
	if err != nil {
		return nil, fmt.Errorf("index: encoder: %w", err)
	}

	buf := bufio.NewWriterSize(enc, 128<<10)
	iw := &IndexWriter{enc: enc, hasher: hasher, buf: buf, json: json.NewEncoder(buf)}

	if err := iw.json.Encode(indexHeader{
		Version: indexVersion, Snapshot: snapshotID, Instance: instance, Root: root,
	}); err != nil {
		return nil, fmt.Errorf("index: write header: %w", err)
	}
	return iw, nil
}

// Add appends one entry.
func (w *IndexWriter) Add(e Entry) error {
	if err := e.Validate(); err != nil {
		return err
	}
	if err := w.json.Encode(e); err != nil {
		return fmt.Errorf("index: write entry %q: %w", e.Path, err)
	}
	w.entries++
	return nil
}

// Entries reports how many entries have been written.
func (w *IndexWriter) Entries() int64 { return w.entries }

// Close flushes the index and returns the hash of the compressed bytes.
func (w *IndexWriter) Close() (hash.Hash, error) {
	if w.closed {
		return hash.Zero, fmt.Errorf("index: already closed")
	}
	w.closed = true
	if err := w.buf.Flush(); err != nil {
		w.enc.Close()
		return hash.Zero, fmt.Errorf("index: flush: %w", err)
	}
	if err := w.enc.Close(); err != nil {
		return hash.Zero, fmt.Errorf("index: close encoder: %w", err)
	}
	return w.hasher.Sum(), nil
}

// IndexReader streams entries back out of an index.
type IndexReader struct {
	dec    *zstd.Decoder
	json   *json.Decoder
	header indexHeader
}

// NewIndexReader reads the header and prepares to stream entries.
func NewIndexReader(r io.Reader) (*IndexReader, error) {
	dec, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, fmt.Errorf("index: decoder: %w", err)
	}
	ir := &IndexReader{dec: dec, json: json.NewDecoder(bufio.NewReaderSize(dec, 128<<10))}
	if err := ir.json.Decode(&ir.header); err != nil {
		dec.Close()
		return nil, fmt.Errorf("index: read header: %w", err)
	}
	if ir.header.Version != indexVersion {
		dec.Close()
		return nil, fmt.Errorf("index: unsupported version %d (this build reads %d)",
			ir.header.Version, indexVersion)
	}
	return ir, nil
}

// SnapshotID returns the snapshot this index belongs to.
func (r *IndexReader) SnapshotID() string { return r.header.Snapshot }

// Instance returns the AMP instance name recorded in the header.
func (r *IndexReader) Instance() string { return r.header.Instance }

// Root returns the absolute directory the paths are relative to.
func (r *IndexReader) Root() string { return r.header.Root }

// Next returns the next entry, or io.EOF when the index is exhausted.
func (r *IndexReader) Next() (Entry, error) {
	var e Entry
	if err := r.json.Decode(&e); err != nil {
		if err == io.EOF {
			return Entry{}, io.EOF
		}
		return Entry{}, fmt.Errorf("index: read entry: %w", err)
	}
	if err := e.Validate(); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// Close releases the decoder.
func (r *IndexReader) Close() error {
	r.dec.Close()
	return nil
}

// ReadAll loads an entire index into a map keyed by path. This is what the
// stat-diff compares the live filesystem against, so it is worth the memory:
// 58,000 entries cost roughly 15 MB.
func ReadAll(r io.Reader) (map[string]Entry, *IndexReader, error) {
	ir, err := NewIndexReader(r)
	if err != nil {
		return nil, nil, err
	}
	out := make(map[string]Entry, 1024)
	for {
		e, err := ir.Next()
		if err == io.EOF {
			return out, ir, nil
		}
		if err != nil {
			ir.Close()
			return nil, nil, err
		}
		out[e.Path] = e
	}
}
