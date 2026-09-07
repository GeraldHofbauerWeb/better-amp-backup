package repo

import (
	"fmt"
	"syscall"
)

// SpaceInfo describes the filesystem a repository lives on.
type SpaceInfo struct {
	// AvailableBytes is what an unprivileged process may still use, which is
	// what matters here — not the root-reserved total.
	AvailableBytes uint64
	TotalBytes     uint64
}

// UsedPercent reports how full the filesystem is.
func (s SpaceInfo) UsedPercent() float64 {
	if s.TotalBytes == 0 {
		return 0
	}
	return 100 * (1 - float64(s.AvailableBytes)/float64(s.TotalBytes))
}

// Space reports free space on the filesystem holding path.
func Space(path string) (SpaceInfo, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return SpaceInfo{}, fmt.Errorf("repo: statfs %s: %w", path, err)
	}
	bsize := uint64(st.Bsize)
	return SpaceInfo{
		AvailableBytes: st.Bavail * bsize,
		TotalBytes:     st.Blocks * bsize,
	}, nil
}

// Space reports free space where the repository is stored.
func (r *Repository) Space() (SpaceInfo, error) { return Space(r.root) }
