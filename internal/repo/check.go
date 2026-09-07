package repo

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/hash"
)

// CheckOptions configures an integrity check.
type CheckOptions struct {
	// ReadData re-reads and re-hashes every referenced object rather than only
	// confirming it exists. Slow, and the only way to catch bit rot before a
	// restore does.
	ReadData bool

	Progress func(stage string, done, total int)
}

// CheckReport summarises a check.
type CheckReport struct {
	Snapshots int
	// Objects counts distinct referenced objects.
	Objects  int
	Rehashed int
	Problems []string
}

// Check verifies the repository.
//
// It works outwards from the snapshots rather than inwards from the objects,
// because the question that matters is not "is this object intact" but "can
// every snapshot still be restored".
func (r *Repository) Check(ctx context.Context, opts CheckOptions) (*CheckReport, error) {
	if opts.Progress == nil {
		opts.Progress = func(string, int, int) {}
	}

	// Shared lock: a concurrent backup is fine, a concurrent prune is not.
	unlock, err := r.LockWrite(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()

	snapshots, err := r.ListSnapshots()
	if err != nil {
		return nil, err
	}

	rep := &CheckReport{Snapshots: len(snapshots)}
	referenced := make(map[hash.Hash]string, 4096)

	for i, m := range snapshots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		opts.Progress("snapshots", i, len(snapshots))

		if err := r.VerifyIndexHash(m); err != nil {
			rep.Problems = append(rep.Problems, err.Error())
			continue
		}

		ir, closeIdx, err := r.OpenIndex(m.ID)
		if err != nil {
			rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %v", m.ID, err))
			continue
		}
		for {
			e, err := ir.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %v", m.ID, err))
				break
			}
			if e.Type != TypeFile || e.Hash.IsZero() {
				continue
			}
			if _, seen := referenced[e.Hash]; !seen {
				referenced[e.Hash] = fmt.Sprintf("%s in %s", e.Path, m.ID)
			}
		}
		closeIdx()
	}
	opts.Progress("snapshots", len(snapshots), len(snapshots))
	rep.Objects = len(referenced)

	var done int
	for h, where := range referenced {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		done++
		opts.Progress("objects", done, len(referenced))

		if !opts.ReadData {
			ok, err := r.Objects().Has(ctx, h)
			if err != nil {
				rep.Problems = append(rep.Problems, fmt.Sprintf("%s (%s): %v", h, where, err))
				continue
			}
			if !ok {
				rep.Problems = append(rep.Problems,
					fmt.Sprintf("object %s is missing, needed by %s", h, where))
			}
			continue
		}

		rc, err := r.Objects().Open(ctx, h)
		if err != nil {
			rep.Problems = append(rep.Problems,
				fmt.Sprintf("object %s (%s) cannot be read: %v", h, where, err))
			continue
		}
		got, _, err := hash.OfReader(rc)
		rc.Close()
		if err != nil {
			rep.Problems = append(rep.Problems,
				fmt.Sprintf("object %s (%s): %v", h, where, err))
			continue
		}
		rep.Rehashed++
		if got != h {
			rep.Problems = append(rep.Problems, fmt.Sprintf(
				"object %s (%s) is corrupted: its content hashes to %s", h, where, got))
		}
	}
	return rep, nil
}
