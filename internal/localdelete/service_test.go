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
	begun     bool
	beginErr  error
	commitErr error
	events    *[]string
}

func (fake *fakeDeleteStore) LoadCommittedDeletion(context.Context, string, string, string) (store.CommittedDeletion, error) {
	return fake.selection, nil
}

func (fake *fakeDeleteStore) BeginDeletionBatch(_ context.Context, _ string, _ store.CommittedDeletion, _ string) error {
	if fake.events != nil {
		*fake.events = append(*fake.events, "begin")
	}
	if fake.beginErr != nil {
		return fake.beginErr
	}
	fake.begun = true
	return nil
}

func (fake *fakeDeleteStore) CommitDeletionResults(_ context.Context, _ string, results []store.DeletionResult) error {
	if fake.events != nil {
		*fake.events = append(*fake.events, "commit")
	}
	if fake.commitErr != nil {
		return fake.commitErr
	}
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
	events  *[]string
}

func (fake *fakeDeleteHelper) Execute(_ context.Context, task proto.DeleteTask) ([]proto.DeleteReport, error) {
	fake.calls++
	if fake.events != nil {
		*fake.events = append(*fake.events, "helper")
	}
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

func TestDeleteExecutePersistsIntentBeforeHelper(t *testing.T) {
	selection := deletionFixture(t, 1)
	events := []string{}
	backend := &fakeDeleteStore{selection: selection, events: &events}
	helper := &fakeDeleteHelper{events: &events}
	service := NewService("machine-a", backend, helper)
	preview, err := service.Prepare(context.Background(), DeleteSelection{RunID: "run-1", GroupID: "group-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Execute(context.Background(), DeleteExecution{BatchID: preview.BatchID, SelectionDigest: preview.SelectionDigest, Token: preview.Token}); err != nil {
		t.Fatal(err)
	}
	want := []string{"begin", "helper", "commit"}
	if len(events) != len(want) || events[0] != want[0] || events[1] != want[1] || events[2] != want[2] {
		t.Fatalf("delete events=%v, want %v", events, want)
	}
}

func TestDeleteExecuteDoesNotCallHelperWhenIntentFails(t *testing.T) {
	selection := deletionFixture(t, 1)
	beginErr := errors.New("begin failed")
	backend := &fakeDeleteStore{selection: selection, beginErr: beginErr}
	helper := &fakeDeleteHelper{}
	service := NewService("machine-a", backend, helper)
	preview, err := service.Prepare(context.Background(), DeleteSelection{RunID: "run-1", GroupID: "group-1"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Execute(context.Background(), DeleteExecution{BatchID: preview.BatchID, SelectionDigest: preview.SelectionDigest, Token: preview.Token})
	if !errors.Is(err, beginErr) || helper.calls != 0 {
		t.Fatalf("execute error=%v helper calls=%d", err, helper.calls)
	}
}

func TestDeleteExecuteKeepsIntentWhenResultCommitFails(t *testing.T) {
	selection := deletionFixture(t, 1)
	commitErr := errors.New("commit failed")
	backend := &fakeDeleteStore{selection: selection, commitErr: commitErr}
	helper := &fakeDeleteHelper{}
	service := NewService("machine-a", backend, helper)
	preview, err := service.Prepare(context.Background(), DeleteSelection{RunID: "run-1", GroupID: "group-1"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Execute(context.Background(), DeleteExecution{BatchID: preview.BatchID, SelectionDigest: preview.SelectionDigest, Token: preview.Token})
	if !errors.Is(err, commitErr) || !backend.begun || helper.calls != 1 {
		t.Fatalf("execute error=%v begun=%t helper calls=%d", err, backend.begun, helper.calls)
	}
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
