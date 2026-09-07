// Package compress handles the optional zstd layer applied to stored objects.
//
// Much of what a Minecraft instance holds is already compressed: .jar and .zip
// archives, PNG textures, and the per-chunk zlib streams inside region files.
// Spending CPU to grow those by a few bytes is pure waste, so every object is
// trial-compressed and stored raw whenever compression fails to pay for itself.
package compress

import (
	"fmt"
	"path"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Algo identifies how an object's payload is encoded on disk.
type Algo uint8

const (
	// None means the payload is stored verbatim.
	None Algo = 0
	// Zstd means the payload is zstd-compressed.
	Zstd Algo = 1
)

func (a Algo) String() string {
	switch a {
	case None:
		return "none"
	case Zstd:
		return "zstd"
	default:
		return fmt.Sprintf("algo(%d)", uint8(a))
	}
}

// Valid reports whether a is an encoding this build understands.
func (a Algo) Valid() bool { return a == None || a == Zstd }

// worthwhileRatio is the compressed/original ratio below which compression is
// kept. At 0.95 an object has to shrink by at least 5% to justify the CPU cost
// of decompressing it on every restore.
const worthwhileRatio = 0.95

// probeSize is how much of a large payload is sampled to decide whether the
// whole thing is worth compressing.
const probeSize = 128 << 10

// alreadyCompressed lists extensions whose contents are compressed by design.
// Region files (.mca) are deliberately absent: their chunk payloads are zlib
// streams, but the surrounding sector padding still yields 5-15%.
var alreadyCompressed = map[string]bool{
	".zip": true, ".jar": true, ".gz": true, ".xz": true, ".zst": true,
	".bz2": true, ".7z": true, ".rar": true,
	".png": true, ".jpg": true, ".jpeg": true, ".webp": true,
	".ogg": true, ".mp3": true, ".mp4": true, ".webm": true,
}

// SkipByName reports whether a path's extension marks it as already compressed.
func SkipByName(name string) bool {
	return alreadyCompressed[strings.ToLower(path.Ext(name))]
}

var (
	decoderOnce sync.Once
	decoder     *zstd.Decoder
	encoders    sync.Map // level int -> *zstd.Encoder
)

func encoderFor(level int) (*zstd.Encoder, error) {
	if e, ok := encoders.Load(level); ok {
		return e.(*zstd.Encoder), nil
	}
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)),
		zstd.WithEncoderConcurrency(1),
	)
	if err != nil {
		return nil, fmt.Errorf("compress: new encoder: %w", err)
	}
	actual, loaded := encoders.LoadOrStore(level, enc)
	if loaded {
		enc.Close()
	}
	return actual.(*zstd.Encoder), nil
}

func sharedDecoder() (*zstd.Decoder, error) {
	var err error
	decoderOnce.Do(func() {
		decoder, err = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	})
	if err != nil {
		return nil, fmt.Errorf("compress: new decoder: %w", err)
	}
	if decoder == nil {
		return nil, fmt.Errorf("compress: decoder unavailable")
	}
	return decoder, nil
}

// Encode compresses src if that is worthwhile and reports which encoding was
// chosen. The name is only used for the extension heuristic and may be empty.
//
// The returned slice may alias src when the chosen algorithm is None.
func Encode(src []byte, name string, level int) ([]byte, Algo, error) {
	if len(src) == 0 || SkipByName(name) {
		return src, None, nil
	}

	enc, err := encoderFor(level)
	if err != nil {
		return nil, None, err
	}

	// For large payloads, decide on a sample first so an incompressible 600 KB
	// region file does not get compressed in full just to be thrown away.
	if len(src) > probeSize {
		probe := enc.EncodeAll(src[:probeSize], nil)
		if float64(len(probe))/float64(probeSize) > worthwhileRatio {
			return src, None, nil
		}
	}

	out := enc.EncodeAll(src, nil)
	if float64(len(out))/float64(len(src)) > worthwhileRatio {
		return src, None, nil
	}
	return out, Zstd, nil
}

// Decode reverses Encode. The returned slice may alias src when algo is None.
func Decode(src []byte, algo Algo) ([]byte, error) {
	switch algo {
	case None:
		return src, nil
	case Zstd:
		dec, err := sharedDecoder()
		if err != nil {
			return nil, err
		}
		out, err := dec.DecodeAll(src, nil)
		if err != nil {
			return nil, fmt.Errorf("compress: decode: %w", err)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("compress: unknown algorithm %d", uint8(algo))
	}
}
