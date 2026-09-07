package repo

import (
	"context"
	"io"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/compress"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/hash"
)

// PutResult describes what happened to a single stored object.
type PutResult struct {
	// Hash addresses the object; it is the BLAKE3-256 of the plaintext.
	Hash hash.Hash
	// Size is the plaintext length in bytes.
	Size int64
	// StoredSize is what the object occupies on disk, header included. It is
	// zero when the object already existed, since nothing new was written.
	StoredSize int64
	// Algo records how the payload was encoded.
	Algo compress.Algo
	// New reports whether this call actually added the object. A false value
	// is the dedup hit that makes incremental snapshots cheap.
	New bool
}

// ObjectInfo is what List reports for each stored object.
type ObjectInfo struct {
	Hash       hash.Hash
	StoredSize int64
	// ModTime is used by prune's grace period to avoid racing a concurrent
	// backup that has written an object but not yet committed its index.
	ModTime int64
}

// ObjectStore is the content-addressed blob layer.
//
// Implementations must be safe for concurrent use, and Put must be atomic:
// an object is either absent or complete and readable, never half-written.
// Everything above this interface is storage-agnostic, which is what lets an
// S3 backend arrive later without reshaping the backup path.
type ObjectStore interface {
	// Put stores the contents of r. name is only a hint for the compression
	// heuristic and may be empty.
	Put(ctx context.Context, r io.Reader, name string) (PutResult, error)
	// Open returns the plaintext of an object as a stream.
	Open(ctx context.Context, h hash.Hash) (io.ReadCloser, error)
	// Has reports whether an object is present.
	Has(ctx context.Context, h hash.Hash) (bool, error)
	// List calls fn for every stored object. Returning an error from fn stops
	// the walk and is propagated.
	List(ctx context.Context, fn func(ObjectInfo) error) error
	// Delete removes an object. Deleting an absent object is not an error.
	Delete(ctx context.Context, h hash.Hash) error
}
