package repo

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/hash"
	"github.com/klauspost/compress/zstd"
)

func newTestRepo(t *testing.T) *Repository {
	t.Helper()
	r, err := Init(t.TempDir(), 3)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	return r
}

func sampleEntries() []Entry {
	return []Entry{
		{Path: "Minecraft", Type: TypeDir, Mode: 0o755, UID: 1000, GID: 1000, ModTime: 1},
		{Path: "Minecraft/server.properties", Type: TypeFile, Mode: 0o644,
			Size: 1527, ModTime: 1757260800123456789, Hash: hash.Sum([]byte("props"))},
		{Path: "Minecraft/world/region/r.0.0.mca", Type: TypeFile, Mode: 0o644,
			Size: 614400, ModTime: 1757260800987654321, Hash: hash.Sum([]byte("region"))},
		{Path: "Minecraft/logs/latest.log", Type: TypeSymlink, Target: "2026-09-07-1.log"},
		{Path: "Minecraft/empty.dat", Type: TypeFile, Mode: 0o600, Size: 0},
	}
}

func TestIndexRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := sampleEntries()

	iw, err := NewIndexWriter(&buf, "20260907T170000Z-3f2a1b", "SebsModpackv401", "/AMP")
	if err != nil {
		t.Fatalf("NewIndexWriter: %v", err)
	}
	for _, e := range want {
		if err := iw.Add(e); err != nil {
			t.Fatalf("Add %q: %v", e.Path, err)
		}
	}
	if got := iw.Entries(); got != int64(len(want)) {
		t.Errorf("Entries = %d, want %d", got, len(want))
	}
	if _, err := iw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ir, err := NewIndexReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("NewIndexReader: %v", err)
	}
	defer ir.Close()

	if got := ir.SnapshotID(); got != "20260907T170000Z-3f2a1b" {
		t.Errorf("SnapshotID = %q", got)
	}
	if got := ir.Instance(); got != "SebsModpackv401" {
		t.Errorf("Instance = %q", got)
	}
	if got := ir.Root(); got != "/AMP" {
		t.Errorf("Root = %q", got)
	}

	var got []Entry
	for {
		e, err := ir.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		got = append(got, e)
	}

	if len(got) != len(want) {
		t.Fatalf("read %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}
}

func TestIndexRejectsInvalidEntries(t *testing.T) {
	cases := []struct {
		name string
		e    Entry
	}{
		{"empty path", Entry{Type: TypeFile, Hash: hash.Sum([]byte("x")), Size: 1}},
		{"unknown type", Entry{Path: "a", Type: EntryType("q")}},
		{"symlink without target", Entry{Path: "a", Type: TypeSymlink}},
		{"file with size but no hash", Entry{Path: "a", Type: TypeFile, Size: 10}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			iw, err := NewIndexWriter(&bytes.Buffer{}, "20260907T170000Z-000000", "i", "/r")
			if err != nil {
				t.Fatalf("NewIndexWriter: %v", err)
			}
			if err := iw.Add(tc.e); err == nil {
				t.Error("Add accepted an invalid entry")
			}
		})
	}
}

func TestIndexRejectsFutureVersion(t *testing.T) {
	// Build an index whose header claims a format this build does not know.
	// Reading it must fail up front rather than mis-parsing 58,000 entries.
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	if _, err := enc.Write([]byte(`{"v":9,"snapshot":"x","instance":"i","root":"/r"}` + "\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := NewIndexReader(bytes.NewReader(buf.Bytes())); err == nil {
		t.Error("reader accepted an unsupported index version")
	}
}

func TestSnapshotCommitPublishesBoth(t *testing.T) {
	r := newTestRepo(t)
	id := NewSnapshotID(time.Date(2026, 9, 7, 17, 0, 0, 0, time.UTC), "3f2a1b")

	w, err := r.NewSnapshotWriter(id, "SebsModpackv401", "/AMP")
	if err != nil {
		t.Fatalf("NewSnapshotWriter: %v", err)
	}
	for _, e := range sampleEntries() {
		if err := w.Add(e); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	m := &Manifest{
		Root:          "/AMP",
		StartedAt:     time.Now().UTC().Add(-time.Minute),
		FinishedAt:    time.Now().UTC(),
		QuiesceMillis: 240,
		Stats:         Stats{Files: 3, Dirs: 1, Symlinks: 1},
		ToolVersion:   "test",
	}
	if err := w.Commit(m); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	got, err := r.LoadManifest(id)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if got.State != StateComplete {
		t.Errorf("state = %q, want complete", got.State)
	}
	if got.IndexHash.IsZero() {
		t.Error("manifest has no index hash")
	}
	if err := r.VerifyIndexHash(got); err != nil {
		t.Errorf("VerifyIndexHash: %v", err)
	}

	ir, closeIdx, err := r.OpenIndex(id)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer closeIdx()
	var n int
	for {
		if _, err := ir.Next(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("Next: %v", err)
		}
		n++
	}
	if n != len(sampleEntries()) {
		t.Errorf("index holds %d entries, want %d", n, len(sampleEntries()))
	}
}

func TestSnapshotAbortLeavesNothing(t *testing.T) {
	r := newTestRepo(t)
	id := NewSnapshotID(time.Now(), "abcdef")

	w, err := r.NewSnapshotWriter(id, "inst", "/AMP")
	if err != nil {
		t.Fatalf("NewSnapshotWriter: %v", err)
	}
	if err := w.Add(sampleEntries()[0]); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := w.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}

	if _, err := r.LoadManifest(id); err == nil {
		t.Error("aborted snapshot left a manifest behind")
	}
	tmp, err := os.ReadDir(filepath.Join(r.Root(), dirTmp))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(tmp) != 0 {
		t.Errorf("aborted snapshot left %d temp files", len(tmp))
	}
	snaps, err := r.ListSnapshots()
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snaps) != 0 {
		t.Errorf("ListSnapshots returned %d snapshots after abort", len(snaps))
	}
}

func TestListSnapshotsIsChronological(t *testing.T) {
	r := newTestRepo(t)
	base := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	// Commit out of order to prove the sort is by ID, not by directory order.
	for _, off := range []int{2, 0, 1} {
		id := NewSnapshotID(base.Add(time.Duration(off)*time.Hour), "00000"+string(rune('a'+off)))
		w, err := r.NewSnapshotWriter(id, "inst", "/AMP")
		if err != nil {
			t.Fatalf("NewSnapshotWriter: %v", err)
		}
		if err := w.Commit(&Manifest{Root: "/AMP", StartedAt: base, FinishedAt: base}); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}

	got, err := r.ListSnapshots()
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d snapshots, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].ID >= got[i].ID {
			t.Errorf("snapshots out of order: %s before %s", got[i-1].ID, got[i].ID)
		}
	}
}

func TestLatestForSkipsIncompleteSnapshots(t *testing.T) {
	r := newTestRepo(t)
	base := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

	commit := func(off int, suffix string, state SnapshotState, instance string) string {
		id := NewSnapshotID(base.Add(time.Duration(off)*time.Hour), suffix)
		w, err := r.NewSnapshotWriter(id, instance, "/AMP")
		if err != nil {
			t.Fatalf("NewSnapshotWriter: %v", err)
		}
		if err := w.Commit(&Manifest{Root: "/AMP", State: state,
			StartedAt: base, FinishedAt: base}); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		return id
	}

	good := commit(0, "aaaaaa", StateComplete, "inst")
	commit(1, "bbbbbb", StatePartial, "inst")
	commit(2, "cccccc", StateComplete, "other") // different instance

	got, err := r.LatestFor("inst")
	if err != nil {
		t.Fatalf("LatestFor: %v", err)
	}
	if got != good {
		t.Errorf("LatestFor = %q, want %q (partial and foreign snapshots must be skipped)", got, good)
	}

	if got, err := r.LatestFor("nobody"); err != nil || got != "" {
		t.Errorf("LatestFor(unknown) = %q, %v; want \"\", nil", got, err)
	}
}

func TestMalformedSnapshotIDIsRejected(t *testing.T) {
	r := newTestRepo(t)
	// A path traversal attempt must never reach the filesystem.
	for _, bad := range []string{"../../etc/passwd", "20260907T170000Z", "", "20260907T170000Z-XXXXXX"} {
		if _, err := r.NewSnapshotWriter(bad, "inst", "/AMP"); err == nil {
			t.Errorf("NewSnapshotWriter accepted malformed id %q", bad)
		}
		if _, err := r.LoadManifest(bad); err == nil {
			t.Errorf("LoadManifest accepted malformed id %q", bad)
		}
	}
}

func TestOpenRejectsNonRepository(t *testing.T) {
	if _, err := Open(t.TempDir()); err == nil {
		t.Error("Open accepted a directory that holds no repository")
	}
}

func TestInitRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	if _, err := Init(dir, 3); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := Init(dir, 3); err == nil {
		t.Error("Init overwrote an existing repository")
	}
}

func TestEntryModePreservedThroughIndex(t *testing.T) {
	var buf bytes.Buffer
	want := Entry{Path: "run.sh", Type: TypeFile, Mode: fs.FileMode(0o755),
		Size: 358, ModTime: 5, Hash: hash.Sum([]byte("run"))}

	iw, err := NewIndexWriter(&buf, "20260907T170000Z-000000", "i", "/r")
	if err != nil {
		t.Fatalf("NewIndexWriter: %v", err)
	}
	if err := iw.Add(want); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := iw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	all, ir, err := ReadAll(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	defer ir.Close()

	got, ok := all["run.sh"]
	if !ok {
		t.Fatal("ReadAll lost the entry")
	}
	if got.Mode != want.Mode {
		t.Errorf("mode = %o, want %o", got.Mode, want.Mode)
	}
	if got != want {
		t.Errorf("entry mismatch:\n got %+v\nwant %+v", got, want)
	}
}
