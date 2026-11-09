package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "test.db"), filepath.Join(dir, "raw"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestRecoverRunningJobs(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	for _, j := range []*Job{
		{ID: "sync", Kind: "retry_sync", RunAt: time.Now().Add(-time.Minute)},
		{ID: "send", Kind: "retry_send", RunAt: time.Now().Add(-time.Minute)},
		{ID: "review", Kind: "send_review", RunAt: time.Now().Add(-time.Minute)},
	} {
		if err := st.EnqueueJob(ctx, j); err != nil {
			t.Fatal(err)
		}
		if err := st.ClaimJob(ctx, j.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.RecoverRunningJobs(ctx, 0); err != nil {
		t.Fatal(err)
	}
	jobs, err := st.DueJobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != "sync" {
		t.Fatalf("unexpected due jobs: %#v", jobs)
	}
	var status, message string
	if err := st.DB().QueryRowContext(ctx, "SELECT status,last_error FROM jobs WHERE id='send'").Scan(&status, &message); err != nil {
		t.Fatal(err)
	}
	if status != JobReview || message == "" {
		t.Fatalf("status=%q message=%q", status, message)
	}
	if err := st.ClaimJob(ctx, "sync"); err != nil {
		t.Fatal(err)
	}
	if err := st.HoldJobForReview(ctx, "sync", "uncertain"); err != nil {
		t.Fatal(err)
	}
	if err := st.ClaimJob(ctx, "sync"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("second claim error=%v", err)
	}
}

func TestFinishJobClearsLease(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	j := &Job{ID: "done", Kind: "retry_sync"}
	if err := st.EnqueueJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := st.ClaimJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishJob(ctx, j.ID, nil); err != nil {
		t.Fatal(err)
	}
	var status string
	var claimed any
	if err := st.DB().QueryRowContext(ctx, "SELECT status,claimed_at FROM jobs WHERE id=?", j.ID).Scan(&status, &claimed); err != nil {
		t.Fatal(err)
	}
	if status != JobDone || claimed != nil {
		t.Fatalf("status=%q claimed=%v", status, claimed)
	}
}
