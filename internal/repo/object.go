package repo

import (
	"fmt"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/compress"
)

// Object files carry a small fixed header so that a stored blob is
// self-describing: given only the file, a reader knows how to turn it back
// into plaintext without consulting an index. That matters for recovery — a
// repository whose indexes are lost is still readable with this package alone.
const (
	objectHeaderSize = 8
	objectVersion    = 1
)

var objectMagic = [4]byte{'A', 'B', 'B', 'O'}

type objectHeader struct {
	Version uint8
	Algo    compress.Algo
}

func encodeObjectHeader(h objectHeader) []byte {
	buf := make([]byte, objectHeaderSize)
	copy(buf[0:4], objectMagic[:])
	buf[4] = h.Version
	buf[5] = uint8(h.Algo)
	// buf[6:8] stay zero: reserved for flags, keeps the header 8-byte aligned.
	return buf
}

func decodeObjectHeader(buf []byte) (objectHeader, error) {
	if len(buf) < objectHeaderSize {
		return objectHeader{}, fmt.Errorf("object: truncated header (%d bytes)", len(buf))
	}
	if [4]byte(buf[0:4]) != objectMagic {
		return objectHeader{}, fmt.Errorf("object: bad magic %q", buf[0:4])
	}
	h := objectHeader{Version: buf[4], Algo: compress.Algo(buf[5])}
	if h.Version != objectVersion {
		return objectHeader{}, fmt.Errorf("object: unsupported version %d", h.Version)
	}
	if !h.Algo.Valid() {
		return objectHeader{}, fmt.Errorf("object: unknown compression algorithm %d", buf[5])
	}
	return h, nil
}
