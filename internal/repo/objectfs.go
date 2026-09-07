package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/compress"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/hash"
	"github.com/klauspost/compress/zstd"
)

// probeBytes is how much of a stream is buffered before deciding whether the
// object is worth compressing. It bounds memory for arbitrarily large files.
const probeBytes = 128 << 10

// FSObjectStore keeps objects as individual files under a two-level shard.
//
// Loose objects were chosen over pack files on purpose: at the ~230 KB mean
// file size of an AMP instance the per-file slack is under one percent, while
// garbage collection reduces to unlink(), integrity checking to a re-hash, and
// a single-file restore to one open(). Packing only starts to pay when object
// counts reach the millions or when the backend charges per request, and it
// can be added later behind ObjectStore without touching the backup path.
type FSObjectStore struct {
	root    string // <repo>/objects
	tmp     string // <repo>/tmp
	level   int
	fsyncOn bool
}

var _ ObjectStore = (*FSObjectStore)(nil)

// NewFSObjectStore prepares a store rooted at objectsDir, using tmpDir for
// in-flight writes. Both must live on the same filesystem so that the final
// rename is atomic.
func NewFSObjectStore(objectsDir, tmpDir string, level int) (*FSObjectStore, error) {
	for _, d := range []string{objectsDir, tmpDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("objectstore: create %s: %w", d, err)
		}
	}
	return &FSObjectStore{root: objectsDir, tmp: tmpDir, level: level, fsyncOn: true}, nil
}

// path returns the on-disk location of an object.
func (s *FSObjectStore) path(h hash.Hash) string {
	hex := h.String()
	return filepath.Join(s.root, hex[0:2], hex[2:4], hex)
}

func (s *FSObjectStore) Has(_ context.Context, h hash.Hash) (bool, error) {
	_, err := os.Stat(s.path(h))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("objectstore: stat %s: %w", h, err)
}

func (s *FSObjectStore) Put(ctx context.Context, r io.Reader, name string) (PutResult, error) {
	if err := ctx.Err(); err != nil {
		return PutResult{}, err
	}

	tmpFile, err := os.CreateTemp(s.tmp, "obj-*")
	if err != nil {
		return PutResult{}, fmt.Errorf("objectstore: temp file: %w", err)
	}
	tmpName := tmpFile.Name()
	// Until the rename succeeds the temp file is ours to clean up. After a
	// successful rename removing it would delete the stored object, so the
	// cleanup is disarmed by clearing tmpName.
	defer func() {
		if tmpName != "" {
			tmpFile.Close()
			os.Remove(tmpName)
		}
	}()

	res, err := s.writeObject(tmpFile, r, name)
	if err != nil {
		return PutResult{}, err
	}

	if s.fsyncOn {
		if err := tmpFile.Sync(); err != nil {
			return PutResult{}, fmt.Errorf("objectstore: sync: %w", err)
		}
	}
	if err := tmpFile.Close(); err != nil {
		return PutResult{}, fmt.Errorf("objectstore: close: %w", err)
	}

	final := s.path(res.Hash)
	if _, err := os.Stat(final); err == nil {
		// Already stored by an earlier snapshot. This is the common case and
		// the whole point of content addressing.
		res.New = false
		res.StoredSize = 0
		return res, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return PutResult{}, fmt.Errorf("objectstore: stat %s: %w", res.Hash, err)
	}

	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return PutResult{}, fmt.Errorf("objectstore: create shard: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return PutResult{}, fmt.Errorf("objectstore: commit %s: %w", res.Hash, err)
	}
	tmpName = "" // committed; do not remove
	res.New = true
	return res, nil
}

// writeObject streams r into w, hashing the plaintext and compressing when
// that pays off. The compression decision is made from a bounded prefix so
// that memory stays flat regardless of object size.
func (s *FSObjectStore) writeObject(w io.Writer, r io.Reader, name string) (PutResult, error) {
	probe := make([]byte, probeBytes)
	n, err := io.ReadFull(r, probe)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return PutResult{}, fmt.Errorf("objectstore: read: %w", err)
	}
	probe = probe[:n]
	atEOF := n < probeBytes

	algo := compress.Zstd
	if compress.SkipByName(name) {
		algo = compress.None
	} else if len(probe) > 0 {
		if _, chosen, cerr := compress.Encode(probe, name, s.level); cerr != nil {
			return PutResult{}, cerr
		} else if chosen == compress.None {
			algo = compress.None
		}
	}

	if _, err := w.Write(encodeObjectHeader(objectHeader{Version: objectVersion, Algo: algo})); err != nil {
		return PutResult{}, fmt.Errorf("objectstore: write header: %w", err)
	}

	counter := &countingWriter{w: w}
	hasher := hash.New()

	var sink io.Writer = counter
	var enc *zstd.Encoder
	if algo == compress.Zstd {
		enc, err = zstd.NewWriter(counter,
			zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(s.level)),
			zstd.WithEncoderConcurrency(1),
		)
		if err != nil {
			return PutResult{}, fmt.Errorf("objectstore: encoder: %w", err)
		}
		sink = enc
	}

	// Everything that reaches the sink must also reach the hasher, so that the
	// address always describes the plaintext.
	var plainSize int64
	src := io.Reader(bytes.NewReader(probe))
	if !atEOF {
		src = io.MultiReader(src, r)
	}
	plainSize, err = io.Copy(io.MultiWriter(sink, hasher), src)
	if err != nil {
		if enc != nil {
			enc.Close()
		}
		return PutResult{}, fmt.Errorf("objectstore: copy: %w", err)
	}
	if enc != nil {
		if err := enc.Close(); err != nil {
			return PutResult{}, fmt.Errorf("objectstore: finish encoder: %w", err)
		}
	}

	return PutResult{
		Hash:       hasher.Sum(),
		Size:       plainSize,
		StoredSize: objectHeaderSize + counter.n,
		Algo:       algo,
	}, nil
}

func (s *FSObjectStore) Open(_ context.Context, h hash.Hash) (io.ReadCloser, error) {
	f, err := os.Open(s.path(h))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("objectstore: object %s: %w", h, os.ErrNotExist)
		}
		return nil, fmt.Errorf("objectstore: open %s: %w", h, err)
	}

	header := make([]byte, objectHeaderSize)
	if _, err := io.ReadFull(f, header); err != nil {
		f.Close()
		return nil, fmt.Errorf("objectstore: read header of %s: %w", h, err)
	}
	hdr, err := decodeObjectHeader(header)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("objectstore: %s: %w", h, err)
	}

	switch hdr.Algo {
	case compress.None:
		return f, nil
	case compress.Zstd:
		dec, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1))
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("objectstore: decoder for %s: %w", h, err)
		}
		return &zstdReadCloser{dec: dec, file: f}, nil
	default:
		f.Close()
		return nil, fmt.Errorf("objectstore: %s: unknown algorithm %s", h, hdr.Algo)
	}
}

func (s *FSObjectStore) List(ctx context.Context, fn func(ObjectInfo) error) error {
	return filepath.WalkDir(s.root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		h, perr := hash.Parse(d.Name())
		if perr != nil {
			// Not an object file. Leave it alone rather than guessing; prune
			// must never delete something it does not understand.
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil // raced with a concurrent delete
			}
			return fmt.Errorf("objectstore: stat %s: %w", p, err)
		}
		return fn(ObjectInfo{Hash: h, StoredSize: info.Size(), ModTime: info.ModTime().UnixNano()})
	})
}

func (s *FSObjectStore) Delete(_ context.Context, h hash.Hash) error {
	err := os.Remove(s.path(h))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("objectstore: delete %s: %w", h, err)
	}
	return nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

type zstdReadCloser struct {
	dec  *zstd.Decoder
	file *os.File
}

func (z *zstdReadCloser) Read(p []byte) (int, error) { return z.dec.Read(p) }

func (z *zstdReadCloser) Close() error {
	z.dec.Close()
	return z.file.Close()
}
