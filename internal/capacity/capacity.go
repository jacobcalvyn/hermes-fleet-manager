package capacity

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

var ErrInsufficient = errors.New("Fleet data storage has insufficient free capacity")

type Policy struct {
	MinimumFreeBytes   uint64
	MinimumFreePercent float64
	MinimumFreeInodes  uint64
}

type Status struct {
	Path           string  `json:"-"`
	FreeBytes      uint64  `json:"free_bytes"`
	TotalBytes     uint64  `json:"total_bytes"`
	FreePercent    float64 `json:"free_percent"`
	FreeInodes     uint64  `json:"free_inodes"`
	MinimumBytes   uint64  `json:"minimum_free_bytes"`
	MinimumPercent float64 `json:"minimum_free_percent"`
	MinimumInodes  uint64  `json:"minimum_free_inodes"`
	OperationsSafe bool    `json:"operations_safe"`
	BlockingReason string  `json:"blocking_reason,omitempty"`
}

func Probe(path string, policy Policy) (Status, error) {
	var stats unix.Statfs_t
	if err := unix.Statfs(path, &stats); err != nil {
		return Status{}, fmt.Errorf("inspect Fleet data storage: %w", err)
	}
	blockSize := uint64(stats.Bsize)
	status := Status{
		Path: path, FreeBytes: stats.Bavail * blockSize, TotalBytes: stats.Blocks * blockSize,
		FreeInodes: stats.Ffree, MinimumBytes: policy.MinimumFreeBytes,
		MinimumPercent: policy.MinimumFreePercent, MinimumInodes: policy.MinimumFreeInodes,
		OperationsSafe: true,
	}
	if status.TotalBytes > 0 {
		status.FreePercent = float64(status.FreeBytes) * 100 / float64(status.TotalBytes)
	}
	switch {
	case status.FreeBytes < policy.MinimumFreeBytes:
		status.OperationsSafe = false
		status.BlockingReason = fmt.Sprintf("only %d bytes remain; at least %d bytes are required", status.FreeBytes, policy.MinimumFreeBytes)
	case status.FreePercent < policy.MinimumFreePercent:
		status.OperationsSafe = false
		status.BlockingReason = fmt.Sprintf("only %.1f%% storage remains; at least %.1f%% is required", status.FreePercent, policy.MinimumFreePercent)
	case stats.Files > 0 && status.FreeInodes < policy.MinimumFreeInodes:
		status.OperationsSafe = false
		status.BlockingReason = fmt.Sprintf("only %d inodes remain; at least %d are required", status.FreeInodes, policy.MinimumFreeInodes)
	}
	return status, nil
}

func Require(path string, policy Policy) (Status, error) {
	status, err := Probe(path, policy)
	if err != nil {
		return Status{}, err
	}
	if !status.OperationsSafe {
		return status, fmt.Errorf("%w: %s", ErrInsufficient, status.BlockingReason)
	}
	return status, nil
}
