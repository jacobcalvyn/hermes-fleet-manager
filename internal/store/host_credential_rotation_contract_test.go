package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

func TestRotateHostCredentialJobStatusContract(t *testing.T) {
	tests := []struct {
		status  string
		allowed bool
	}{
		{status: domain.JobPending},
		{status: domain.JobLeased},
		{status: domain.JobRunning},
		{status: domain.JobSucceeded, allowed: true},
		{status: domain.JobFailed, allowed: true},
	}

	for _, test := range tests {
		t.Run(strings.ToLower(test.status), func(t *testing.T) {
			ctx, dataStore, host, _ := newFleetFixture(t, "credential-status-"+strings.ToLower(test.status))
			if _, err := dataStore.db.ExecContext(
				ctx,
				`UPDATE jobs SET status=? WHERE host_id=?`,
				test.status,
				host.ID,
			); err != nil {
				t.Fatal(err)
			}

			before, err := dataStore.GetHost(ctx, host.ID)
			if err != nil {
				t.Fatal(err)
			}
			err = dataStore.RotateHostCredential(
				ctx,
				host.ID,
				host.Name,
				host.Hostname,
				host.OS,
				host.Arch,
				"rotated-token-hash",
			)
			if test.allowed {
				if err != nil {
					t.Fatalf("RotateHostCredential() error=%v, want success", err)
				}
			} else if !errors.Is(err, ErrHostBusy) {
				t.Fatalf("RotateHostCredential() error=%v, want ErrHostBusy", err)
			}

			after, err := dataStore.GetHost(ctx, host.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Fatalf("rotation changed host metadata for %s job: before=%+v after=%+v", test.status, before, after)
			}
			hash, err := dataStore.HostTokenHash(ctx, host.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantHash := "token-hash"
			if test.allowed {
				wantHash = "rotated-token-hash"
			}
			if hash != wantHash {
				t.Fatalf("token hash=%q, want %q for %s job", hash, wantHash, test.status)
			}
		})
	}
}

func TestRotateHostCredentialRejectedConfirmationPreservesEntireHost(t *testing.T) {
	tests := []struct {
		name        string
		hostID      string
		confirmName string
		hostname    string
		osName      string
		arch        string
		wantErr     error
	}{
		{
			name: "unknown host", hostID: "missing",
			confirmName: "host-rotate-contract", hostname: "host", osName: "test", arch: "test",
			wantErr: ErrNotFound,
		},
		{
			name: "identity mismatch", hostID: "host-rotate-contract",
			confirmName: "other-host", hostname: "host", osName: "test", arch: "test",
			wantErr: ErrHostIdentityMismatch,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, dataStore, host, _ := newFleetFixture(t, "rotate-contract")
			before, err := dataStore.GetHost(ctx, host.ID)
			if err != nil {
				t.Fatal(err)
			}
			beforeHash, err := dataStore.HostTokenHash(ctx, before.ID)
			if err != nil {
				t.Fatal(err)
			}
			err = dataStore.RotateHostCredential(
				ctx,
				test.hostID,
				test.confirmName,
				test.hostname,
				test.osName,
				test.arch,
				"rotated-token-hash",
			)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("RotateHostCredential() error=%v, want %v", err, test.wantErr)
			}

			after, err := dataStore.GetHost(ctx, before.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Fatalf("rejected rotation changed host metadata: before=%+v after=%+v", before, after)
			}
			afterHash, err := dataStore.HostTokenHash(ctx, before.ID)
			if err != nil {
				t.Fatal(err)
			}
			if afterHash != beforeHash {
				t.Fatalf("rejected rotation changed token hash from %q to %q", beforeHash, afterHash)
			}
		})
	}
}
