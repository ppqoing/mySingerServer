package localdelete

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dedup/internal/proto"
	"dedup/internal/store"
)

// Break caught: Prepare accepts arbitrary paths or returns a reusable token
// without binding it to the committed review generation and file identities.
func TestDeletePrepareAndExecuteBindReviewIdentityAndConsumeToken(t *testing.T) {
	selection := deletionFixture(t, 2)
	backend := &fakeDeleteStore{selection: selection}
	helper := &fakeDeleteHelper{results: map[string]proto.DeleteResult{
		selection.Files[0].Path: {Path: selection.Files[0].Path, OK: true},
		selection.Files[1].Path: {Path: selection.Files[1].Path, OK: true, Uncertain: true, ErrCode: "helper_lost"},
	}}
	service := NewService("machine-a", backend, helper)

	preview, err := service.Prepare(context.Background(), DeleteSelection{RunID: "run-1", GroupID: "group-1"})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Count != 2 || preview.TotalSize != selection.Files[0].Size+selection.Files[1].Size ||
		preview.SelectionDigest == "" || preview.Token == "" || preview.BatchID == "" ||
		len(preview.Files) != 2 || preview.Files[0].Path != selection.Files[0].Path {
		t.Fatalf("preview=%#v", preview)
	}
	batch, err := service.Execute(context.Background(), DeleteExecution{
		BatchID: preview.BatchID, SelectionDigest: preview.SelectionDigest, Token: preview.Token,
	})
	if err != nil {
		t.Fatal(err)
	}
	if batch.Status != "uncertain" || batch.Succeeded != 1 || batch.Uncertain != 1 || helper.calls != 1 {
		t.Fatalf("batch=%#v helper calls=%d", batch, helper.calls)
	}
	if len(backend.committed) != 2 || !backend.committed[0].OK || backend.committed[0].Uncertain ||
		!backend.committed[1].Uncertain {
		t.Fatalf("committed=%#v", backend.committed)
	}
	if _, err := service.Execute(context.Background(), DeleteExecution{
		BatchID: preview.BatchID, SelectionDigest: preview.SelectionDigest, Token: preview.Token,
	}); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("replayed token error=%v", err)
	}
	newProcess := NewService("machine-a", backend, helper)
	if _, err := newProcess.Execute(context.Background(), DeleteExecution{
		BatchID: preview.BatchID, SelectionDigest: preview.SelectionDigest, Token: preview.Token,
	}); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("token survived service restart: %v", err)
	}
}

func TestDeleteExecuteRejectsChangedReviewOrFileBeforeHelper(t *testing.T) {
	selection := deletionFixture(t, 1)
	backend := &fakeDeleteStore{selection: selection}
	helper := &fakeDeleteHelper{}
	service := NewService("machine-a", backend, helper)
	preview, err := service.Prepare(context.Background(), DeleteSelection{RunID: "run-1", GroupID: "group-1"})
	if err != nil {
		t.Fatal(err)
	}
	backend.selection.Generation++
	if _, err := service.Execute(context.Background(), DeleteExecution{
		BatchID: preview.BatchID, SelectionDigest: preview.SelectionDigest, Token: preview.Token,
	}); !errors.Is(err, ErrSelectionChanged) {
		t.Fatalf("changed generation error=%v", err)
	}
	if helper.calls != 0 || len(backend.committed) != 0 {
		t.Fatalf("changed selection reached helper/store: %d/%d", helper.calls, len(backend.committed))
	}

	backend.selection = selection
	preview, err = service.Prepare(context.Background(), DeleteSelection{RunID: "run-1", GroupID: "group-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(selection.Files[0].Path, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Execute(context.Background(), DeleteExecution{
		BatchID: preview.BatchID, SelectionDigest: preview.SelectionDigest, Token: preview.Token,
	}); !errors.Is(err, ErrSelectionChanged) {
		t.Fatalf("changed file error=%v", err)
	}
	if helper.calls != 0 {
		t.Fatalf("changed file reached helper: %d", helper.calls)
	}
}

func deletionFixture(t *testing.T, count int) store.CommittedDeletion {
	t.Helper()
	selection := store.CommittedDeletion{
		MachineID: "machine-a", RunID: "run-1", GroupID: "group-1", Generation: 4,
		Category: "exact", Verdict: "duplicate",
	}
	for index := 0; index < count; index++ {
		path := filepath.Join(t.TempDir(), "delete.bin")
		contents := []byte{byte(index + 1), 2, 3, 4}
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha512.Sum512(contents)
		selection.Files = append(selection.Files, store.DeletionFile{
			FileID: int64(index + 1), MachineID: "machine-a", Path: path,
			SHA512: hex.EncodeToString(digest[:]), Size: info.Size(), MTime: info.ModTime().Unix(),
		})
	}
	return selection
}

type fakeDeleteStore struct {
	selection store.CommittedDeletion
	committed []store.DeletionResult
	batch     store.DeletionBatch
}

func (fake *fakeDeleteStore) LoadCommittedDeletion(context.Context, string, string, string) (store.CommittedDeletion, error) {
	return fake.selection, nil
}

func (fake *fakeDeleteStore) CommitDeletionResults(_ context.Context, _ string, results []store.DeletionResult) error {
	fake.committed = append([]store.DeletionResult(nil), results...)
	fake.batch = store.DeletionBatch{BatchID: results[0].BatchID, Requested: len(results)}
	for _, result := range results {
		switch {
		case result.OK && !result.Uncertain:
			fake.batch.Succeeded++
		case result.Uncertain:
			fake.batch.Uncertain++
		default:
			fake.batch.Failed++
		}
	}
	if fake.batch.Succeeded == len(results) {
		fake.batch.Status = "succeeded"
	} else if fake.batch.Uncertain > 0 {
		fake.batch.Status = "uncertain"
	} else {
		fake.batch.Status = "failed"
	}
	return nil
}

func (fake *fakeDeleteStore) LoadDeletionBatch(context.Context, string, string) (store.DeletionBatch, error) {
	return fake.batch, nil
}

type fakeDeleteHelper struct {
	results map[string]proto.DeleteResult
	err     error
	calls   int
}

func (fake *fakeDeleteHelper) Execute(_ context.Context, task proto.DeleteTask) ([]proto.DeleteReport, error) {
	fake.calls++
	entries := make([]proto.DeleteResult, 0, len(task.Entries))
	for _, path := range task.Entries {
		result, ok := fake.results[path]
		if !ok {
			result = proto.DeleteResult{Path: path, ErrCode: "helper_lost", Uncertain: true}
		}
		entries = append(entries, result)
	}
	return []proto.DeleteReport{{TaskID: task.TaskID, Entries: entries}}, fake.err
}

func TestDeletePreparedTokenExpires(t *testing.T) {
	selection := deletionFixture(t, 1)
	backend := &fakeDeleteStore{selection: selection}
	service := NewServiceWithOptions("machine-a", backend, &fakeDeleteHelper{}, Options{
		TokenTTL: time.Nanosecond,
	})
	preview, err := service.Prepare(context.Background(), DeleteSelection{RunID: "run-1", GroupID: "group-1"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if _, err := service.Execute(context.Background(), DeleteExecution{
		BatchID: preview.BatchID, SelectionDigest: preview.SelectionDigest, Token: preview.Token,
	}); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired token error=%v", err)
	}
}
