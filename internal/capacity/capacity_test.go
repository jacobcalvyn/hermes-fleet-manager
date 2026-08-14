package capacity

import (
	"errors"
	"testing"
)

func TestProbeReportsCapacityAndPolicy(t *testing.T) {
	status, err := Probe(t.TempDir(), Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if status.TotalBytes == 0 || status.FreeBytes == 0 || !status.OperationsSafe {
		t.Fatalf("unexpected capacity status: %+v", status)
	}
}

func TestRequireFailsClosedWhenThresholdCannotBeMet(t *testing.T) {
	_, err := Require(t.TempDir(), Policy{MinimumFreeBytes: ^uint64(0)})
	if !errors.Is(err, ErrInsufficient) {
		t.Fatalf("Require() error = %v, want ErrInsufficient", err)
	}
}
