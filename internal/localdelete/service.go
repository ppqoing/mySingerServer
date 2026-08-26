package localdelete

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"

	"dedup/internal/proto"
	"dedup/internal/store"
)

var (
	ErrInvalidToken     = errors.New("invalid_delete_token")
	ErrSelectionChanged = errors.New("delete_selection_changed")
)

const defaultTokenTTL = 10 * time.Minute

type DeleteSelection struct {
	RunID   string
	GroupID string
}

type DeletePreview = proto.LocalDeletePreview

type DeleteExecution struct {
	BatchID         string
	SelectionDigest string
	Token           string
}

type DeleteBatch = proto.LocalDeleteBatch

type Service interface {
	Prepare(context.Context, DeleteSelection) (DeletePreview, error)
	Execute(context.Context, DeleteExecution) (DeleteBatch, error)
	Status(context.Context, string) (DeleteBatch, error)
}

type Store interface {
	LoadCommittedDeletion(context.Context, string, string, string) (store.CommittedDeletion, error)
	BeginDeletionBatch(context.Context, string, store.CommittedDeletion, string) error
	CommitDeletionResults(context.Context, string, []store.DeletionResult) error
	LoadDeletionBatch(context.Context, string, string) (store.DeletionBatch, error)
}

type Helper interface {
	Execute(context.Context, proto.DeleteTask) ([]proto.DeleteReport, error)
}

type Options struct {
	TokenTTL time.Duration
	Now      func() time.Time
}

type preparedDelete struct {
	batchID   string
	digest    string
	token     string
	expires   time.Time
	selection store.CommittedDeletion
}

type service struct {
	machineID string
	store     Store
	helper    Helper
	tokenTTL  time.Duration
	now       func() time.Time

	mu      sync.Mutex
	pending map[string]preparedDelete
}

func NewService(machineID string, backend Store, helper Helper) Service {
	return NewServiceWithOptions(machineID, backend, helper, Options{})
}

func NewServiceWithOptions(machineID string, backend Store, helper Helper, options Options) Service {
	if options.TokenTTL <= 0 {
		options.TokenTTL = defaultTokenTTL
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &service{
		machineID: machineID, store: backend, helper: helper,
		tokenTTL: options.TokenTTL, now: options.Now,
		pending: make(map[string]preparedDelete),
	}
}

func (service *service) Prepare(ctx context.Context, request DeleteSelection) (DeletePreview, error) {
	if service == nil || service.store == nil || service.helper == nil || service.machineID == "" ||
		ctx == nil || request.RunID == "" || request.GroupID == "" {
		return DeletePreview{}, fmt.Errorf("localdelete: unavailable")
	}
	selection, err := service.store.LoadCommittedDeletion(ctx, service.machineID, request.RunID, request.GroupID)
	if err != nil {
		return DeletePreview{}, err
	}
	if err := validateSelection(service.machineID, request, selection); err != nil {
		return DeletePreview{}, err
	}
	digest := selectionDigest(selection)
	batchID, err := randomHex(16)
	if err != nil {
		return DeletePreview{}, err
	}
	token, err := randomHex(32)
	if err != nil {
		return DeletePreview{}, err
	}
	expires := service.now().Add(service.tokenTTL)
	prepared := preparedDelete{
		batchID: batchID, digest: digest, token: token, expires: expires,
		selection: cloneSelection(selection),
	}
	service.mu.Lock()
	service.removeExpiredLocked(service.now())
	service.pending[batchID] = prepared
	service.mu.Unlock()

	preview := DeletePreview{
		BatchID: batchID, RunID: selection.RunID, GroupID: selection.GroupID,
		Generation: selection.Generation, Count: len(selection.Files),
		SelectionDigest: digest, Token: token, ExpiresAt: expires.UnixMilli(),
		Files: make([]proto.LocalDeleteFile, 0, len(selection.Files)),
	}
	for _, file := range selection.Files {
		preview.TotalSize += file.Size
		preview.Files = append(preview.Files, proto.LocalDeleteFile{
			FileID: file.FileID, Path: file.Path, Size: file.Size, SHA512: file.SHA512,
		})
	}
	return preview, nil
}

func (service *service) Execute(ctx context.Context, request DeleteExecution) (DeleteBatch, error) {
	if service == nil || service.store == nil || service.helper == nil || ctx == nil ||
		request.BatchID == "" || request.SelectionDigest == "" || request.Token == "" {
		return DeleteBatch{}, ErrInvalidToken
	}
	prepared, err := service.consume(request)
	if err != nil {
		return DeleteBatch{}, err
	}
	current, err := service.store.LoadCommittedDeletion(
		ctx, service.machineID, prepared.selection.RunID, prepared.selection.GroupID,
	)
	if err != nil || current.Generation != prepared.selection.Generation ||
		selectionDigest(current) != prepared.digest {
		return DeleteBatch{}, ErrSelectionChanged
	}
	for _, file := range current.Files {
		if err := verifyFileIdentity(ctx, file); err != nil {
			return DeleteBatch{}, ErrSelectionChanged
		}
	}
	if err := service.store.BeginDeletionBatch(ctx, prepared.batchID, current, prepared.digest); err != nil {
		return DeleteBatch{}, err
	}

	paths := make([]string, len(current.Files))
	for index := range current.Files {
		paths[index] = current.Files[index].Path
	}
	reports, helperErr := service.helper.Execute(ctx, proto.DeleteTask{
		TaskID: prepared.batchID, Mode: proto.ModeSoft, Confirmed: true, Entries: paths,
	})
	collector := newReportCollector(paths)
	for index := range reports {
		if err := collector.add(&reports[index]); err != nil {
			helperErr = errors.Join(helperErr, err)
		}
	}
	results := collector.results(current, prepared, helperErr)
	if err := service.store.CommitDeletionResults(ctx, prepared.batchID, results); err != nil {
		return DeleteBatch{}, err
	}
	return service.Status(ctx, prepared.batchID)
}

func (service *service) Status(ctx context.Context, batchID string) (DeleteBatch, error) {
	if service == nil || service.store == nil || service.machineID == "" || ctx == nil || batchID == "" {
		return DeleteBatch{}, fmt.Errorf("localdelete: invalid batch")
	}
	batch, err := service.store.LoadDeletionBatch(ctx, service.machineID, batchID)
	if err != nil {
		return DeleteBatch{}, err
	}
	response := DeleteBatch{
		BatchID: batch.BatchID, Status: batch.Status, Requested: batch.Requested,
		Succeeded: batch.Succeeded, Failed: batch.Failed, Uncertain: batch.Uncertain,
		Items: make([]proto.LocalDeleteItem, 0, len(batch.Items)),
	}
	for _, item := range batch.Items {
		response.Items = append(response.Items, proto.LocalDeleteItem{
			FileID: item.FileID, Result: item.Result, ErrorCode: item.ErrorCode, Uncertain: item.Uncertain,
		})
	}
	return response, nil
}

func (service *service) consume(request DeleteExecution) (preparedDelete, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	now := service.now()
	service.removeExpiredLocked(now)
	prepared, exists := service.pending[request.BatchID]
	if !exists {
		return preparedDelete{}, ErrInvalidToken
	}
	delete(service.pending, request.BatchID)
	if now.After(prepared.expires) || !equalSecret(request.Token, prepared.token) ||
		!equalSecret(request.SelectionDigest, prepared.digest) {
		return preparedDelete{}, ErrInvalidToken
	}
	return prepared, nil
}

func (service *service) removeExpiredLocked(now time.Time) {
	for batchID, prepared := range service.pending {
		if !now.Before(prepared.expires) {
			delete(service.pending, batchID)
		}
	}
}

func validateSelection(machineID string, request DeleteSelection, selection store.CommittedDeletion) error {
	if selection.MachineID != machineID || selection.RunID != request.RunID ||
		selection.GroupID != request.GroupID || selection.Generation <= 0 || len(selection.Files) == 0 ||
		(selection.Category != "exact" && selection.Verdict != "duplicate") {
		return ErrSelectionChanged
	}
	for _, file := range selection.Files {
		if file.FileID <= 0 || file.MachineID != machineID || file.Path == "" ||
			file.Size < 0 || file.MTime <= 0 || len(file.SHA512) != sha512.Size*2 {
			return ErrSelectionChanged
		}
	}
	return nil
}

func selectionDigest(selection store.CommittedDeletion) string {
	files := append([]store.DeletionFile(nil), selection.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].FileID < files[j].FileID })
	hash := sha256.New()
	writeDigestString(hash, selection.MachineID)
	writeDigestString(hash, selection.RunID)
	writeDigestString(hash, selection.GroupID)
	writeDigestInt64(hash, selection.Generation)
	for _, file := range files {
		writeDigestInt64(hash, file.FileID)
		writeDigestString(hash, file.Path)
		writeDigestString(hash, file.SHA512)
		writeDigestInt64(hash, file.Size)
		writeDigestInt64(hash, file.MTime)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func writeDigestString(writer io.Writer, value string) {
	writeDigestInt64(writer, int64(len(value)))
	_, _ = io.WriteString(writer, value)
}

func writeDigestInt64(writer io.Writer, value int64) {
	var buffer [8]byte
	binary.BigEndian.PutUint64(buffer[:], uint64(value))
	_, _ = writer.Write(buffer[:])
}

func verifyFileIdentity(ctx context.Context, file store.DeletionFile) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	handle, err := os.Open(file.Path)
	if err != nil {
		return err
	}
	defer handle.Close()
	before, err := handle.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() != file.Size ||
		before.ModTime().Unix() != file.MTime {
		return ErrSelectionChanged
	}
	hasher := sha512.New()
	if _, err := io.Copy(hasher, &contextReader{ctx: ctx, reader: handle}); err != nil {
		return err
	}
	after, err := handle.Stat()
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() ||
		before.ModTime().UnixNano() != after.ModTime().UnixNano() ||
		hex.EncodeToString(hasher.Sum(nil)) != file.SHA512 {
		return ErrSelectionChanged
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}

type reportCollector struct {
	want map[string]struct{}
	got  map[string]proto.DeleteResult
}

func newReportCollector(paths []string) *reportCollector {
	want := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		want[path] = struct{}{}
	}
	return &reportCollector{want: want, got: make(map[string]proto.DeleteResult, len(paths))}
}

func (collector *reportCollector) add(report *proto.DeleteReport) error {
	if report == nil {
		return fmt.Errorf("localdelete: invalid helper report")
	}
	for _, result := range report.Entries {
		if _, wanted := collector.want[result.Path]; !wanted {
			return fmt.Errorf("localdelete: foreign helper result")
		}
		if _, duplicate := collector.got[result.Path]; duplicate {
			return fmt.Errorf("localdelete: duplicate helper result")
		}
		collector.got[result.Path] = result
	}
	return nil
}

func (collector *reportCollector) results(
	selection store.CommittedDeletion,
	prepared preparedDelete,
	helperErr error,
) []store.DeletionResult {
	results := make([]store.DeletionResult, 0, len(selection.Files))
	for _, file := range selection.Files {
		physical, exists := collector.got[file.Path]
		if !exists {
			physical = proto.DeleteResult{
				Path: file.Path, ErrCode: proto.DeleteErrHelperLost, Uncertain: true,
			}
			if helperErr != nil {
				physical.Err = "delete helper unavailable"
			}
		}
		results = append(results, store.DeletionResult{
			FileID: file.FileID, MachineID: file.MachineID, Path: file.Path,
			SHA512: file.SHA512, Size: file.Size, MTime: file.MTime,
			BatchID: prepared.batchID, RunID: selection.RunID, GroupID: selection.GroupID,
			Generation: selection.Generation, ConfirmationDigest: prepared.digest,
			OK: physical.OK && !physical.Uncertain, Uncertain: physical.Uncertain,
			ErrorCode: physical.ErrCode, ErrorMessage: physical.Err,
		})
	}
	return results
}

func cloneSelection(selection store.CommittedDeletion) store.CommittedDeletion {
	selection.Files = append([]store.DeletionFile(nil), selection.Files...)
	return selection
}

func randomHex(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

func equalSecret(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}
