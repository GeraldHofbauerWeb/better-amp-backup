package repo

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/compress"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/hash"
)

func newTestStore(t *testing.T) *FSObjectStore {
	t.Helper()
	dir := t.TempDir()
	s, err := NewFSObjectStore(filepath.Join(dir, "objects"), filepath.Join(dir, "tmp"), 3)
	if err != nil {
		t.Fatalf("NewFSObjectStore: %v", err)
	}
	return s
}

// compressible produces data that zstd shrinks well, standing in for text
// configs and NBT.
func compressible(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte('a' + i%16)
	}
	return out
}

// incompressible produces high-entropy data, standing in for the zlib streams
// inside a region file.
func incompressible(n int, seed int64) []byte {
	out := make([]byte, n)
	rng := rand.New(rand.NewSource(seed))
	rng.Read(out)
	return out
}

func TestPutOpenRoundTrip(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		data []byte
		file string
	}{
		{"empty", nil, "empty.dat"},
		{"tiny", []byte("x"), "tiny.dat"},
		{"compressible small", compressible(1000), "server.properties"},
		{"compressible large", compressible(5 << 20), "level.dat"},
		{"incompressible small", incompressible(1000, 1), "r.0.0.mca"},
		{"incompressible large", incompressible(3<<20, 2), "r.1.1.mca"},
		{"skipped by extension", compressible(2 << 20), "modpack.zip"},
		{"exactly probe size", compressible(probeBytes), "probe.dat"},
		{"one over probe size", compressible(probeBytes + 1), "probe1.dat"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)

			res, err := s.Put(ctx, bytes.NewReader(tc.data), tc.file)
			if err != nil {
				t.Fatalf("Put: %v", err)
			}
			if got, want := res.Size, int64(len(tc.data)); got != want {
				t.Errorf("plaintext size = %d, want %d", got, want)
			}
			if !res.New {
				t.Error("first Put reported New = false")
			}
			if res.Hash != hash.Sum(tc.data) {
				t.Errorf("hash = %s, want %s", res.Hash, hash.Sum(tc.data))
			}

			rc, err := s.Open(ctx, res.Hash)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer rc.Close()
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if !bytes.Equal(got, tc.data) {
				t.Errorf("round trip mismatch: got %d bytes, want %d", len(got), len(tc.data))
			}
		})
	}
}

func TestPutIsDeduplicating(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	data := compressible(200 << 10)

	first, err := s.Put(ctx, bytes.NewReader(data), "a.dat")
	if err != nil {
		t.Fatalf("first Put: %v", err)
	}
	if !first.New {
		t.Fatal("first Put should be new")
	}

	// Same content under a different name must resolve to the same object.
	second, err := s.Put(ctx, bytes.NewReader(data), "b.dat")
	if err != nil {
		t.Fatalf("second Put: %v", err)
	}
	if second.New {
		t.Error("second Put reported New = true; content was not deduplicated")
	}
	if second.Hash != first.Hash {
		t.Errorf("hash mismatch: %s vs %s", second.Hash, first.Hash)
	}
	if second.StoredSize != 0 {
		t.Errorf("deduplicated Put reported StoredSize = %d, want 0", second.StoredSize)
	}

	var count int
	if err := s.List(ctx, func(ObjectInfo) error { count++; return nil }); err != nil {
		t.Fatalf("List: %v", err)
	}
	if count != 1 {
		t.Errorf("store holds %d objects, want 1", count)
	}
}

func TestIncompressibleDataIsStoredRaw(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	data := incompressible(1<<20, 42)

	res, err := s.Put(ctx, bytes.NewReader(data), "r.0.0.mca")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if res.Algo != compress.None {
		t.Errorf("algo = %s, want none: random data must not be compressed", res.Algo)
	}
	if res.StoredSize > int64(len(data))+objectHeaderSize {
		t.Errorf("stored %d bytes for %d bytes of input; compression made it bigger",
			res.StoredSize, len(data))
	}
}

func TestPutLeavesNoTempFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tmp := filepath.Join(dir, "tmp")
	s, err := NewFSObjectStore(filepath.Join(dir, "objects"), tmp, 3)
	if err != nil {
		t.Fatalf("NewFSObjectStore: %v", err)
	}

	data := compressible(64 << 10)
	for i := 0; i < 3; i++ {
		if _, err := s.Put(ctx, bytes.NewReader(data), "x.dat"); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}

	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("tmp dir holds %d leftover files, want 0", len(entries))
	}
}

func TestHasAndDelete(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	res, err := s.Put(ctx, bytes.NewReader(compressible(4096)), "a.dat")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	if ok, err := s.Has(ctx, res.Hash); err != nil || !ok {
		t.Fatalf("Has after Put = %v, %v; want true, nil", ok, err)
	}
	if err := s.Delete(ctx, res.Hash); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ok, err := s.Has(ctx, res.Hash); err != nil || ok {
		t.Fatalf("Has after Delete = %v, %v; want false, nil", ok, err)
	}
	// Deleting an absent object is a no-op, so that prune can be re-run safely.
	if err := s.Delete(ctx, res.Hash); err != nil {
		t.Errorf("second Delete: %v", err)
	}
	if _, err := s.Open(ctx, res.Hash); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Open after Delete = %v, want ErrNotExist", err)
	}
}

func TestOpenRejectsCorruptedObject(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	res, err := s.Put(ctx, bytes.NewReader(compressible(8192)), "a.dat")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Wreck the header. A restore must fail loudly rather than write garbage.
	p := s.path(res.Hash)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	raw[0] = 'X'
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := s.Open(ctx, res.Hash); err == nil {
		t.Error("Open accepted an object with a corrupted header")
	}
}

func TestListReportsEveryObject(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	want := map[hash.Hash]bool{}
	for i := 0; i < 25; i++ {
		res, err := s.Put(ctx, bytes.NewReader(incompressible(1024, int64(i))), "x.mca")
		if err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
		want[res.Hash] = true
	}

	got := map[hash.Hash]bool{}
	if err := s.List(ctx, func(info ObjectInfo) error {
		if info.StoredSize <= 0 {
			t.Errorf("object %s reported StoredSize %d", info.Hash, info.StoredSize)
		}
		got[info.Hash] = true
		return nil
	}); err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(got) != len(want) {
		t.Fatalf("List returned %d objects, want %d", len(got), len(want))
	}
	for h := range want {
		if !got[h] {
			t.Errorf("List omitted %s", h)
		}
	}
}

func TestListIgnoresForeignFiles(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.Put(ctx, bytes.NewReader(compressible(512)), "a.dat"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Something that is not an object address must be left strictly alone:
	// prune walks this same listing and must never delete what it cannot name.
	stray := filepath.Join(s.root, "README.txt")
	if err := os.WriteFile(stray, []byte("not an object"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var count int
	if err := s.List(ctx, func(ObjectInfo) error { count++; return nil }); err != nil {
		t.Fatalf("List: %v", err)
	}
	if count != 1 {
		t.Errorf("List reported %d objects, want 1 (stray file must be ignored)", count)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Errorf("stray file disappeared: %v", err)
	}
}
