package gui

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"dedup/internal/proto"
)

func TestConfirmStoreCreates128BitBase64URLTokenAndConsumesUntilTTL(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	store := NewConfirmStore(time.Minute, func() time.Time { return now })
	members := []DeleteMember{{
		FileID:    1,
		MachineID: "machine-a",
		Path:      `X:\synthetic\song.flac`,
		Size:      42,
	}}

	token, _, err := store.Create(members)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("token is not raw base64url: %v", err)
	}
	if len(token) != 22 || len(decoded) != 16 {
		t.Fatalf("token length = %d chars/%d bytes, want 22 chars/16 bytes", len(token), len(decoded))
	}

	got, err := store.Consume(token)
	if err != nil {
		t.Fatalf("Consume() error = %v", err)
	}
	if len(got) != 1 || got[0] != members[0] {
		t.Fatalf("Consume() = %#v, want %#v", got, members)
	}

	expiringToken, _, err := store.Create(members)
	if err != nil {
		t.Fatalf("second Create() error = %v", err)
	}
	now = now.Add(time.Minute)
	if _, err := store.Consume(expiringToken); !errors.Is(err, ErrConfirmationExpired) {
		t.Fatalf("Consume() at TTL boundary error = %v, want ErrConfirmationExpired", err)
	}
}

func TestConfirmStoreCreatesUniqueTokens(t *testing.T) {
	store := NewConfirmStore(time.Minute, time.Now)
	seen := make(map[string]struct{})
	for index := 0; index < 256; index++ {
		token, _, err := store.Create([]DeleteMember{{
			FileID: int64(index + 1), MachineID: "machine",
			Path: fmt.Sprintf(`Z:\unique-%d`, index),
		}})
		if err != nil {
			t.Fatal(err)
		}
		if _, duplicate := seen[token]; duplicate {
			t.Fatalf("Create() repeated token %q", token)
		}
		seen[token] = struct{}{}
	}
}

func TestConfirmStoreCanonicalizesAndDeepCopiesMembersAndSummary(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	store := NewConfirmStore(time.Minute, func() time.Time { return now })
	input := []DeleteMember{
		{FileID: 3, MachineID: "machine-b", Path: `Z:\three`, Size: 30},
		{FileID: 2, MachineID: "machine-a", Path: `Z:\two`, Size: 20},
		{FileID: 1, MachineID: "machine-a", Path: `A:\one`, Size: 10},
		{FileID: 1, MachineID: "machine-a", Path: `A:\one`, Size: 10},
	}

	token, summary, err := store.Create(input)
	if err != nil {
		t.Fatal(err)
	}
	wantSummary := DeleteSummary{
		TotalFiles: 3,
		TotalBytes: 60,
		ByMachine:  map[string]int64{"machine-a": 2, "machine-b": 1},
		Samples:    []string{`A:\one`, `Z:\two`, `Z:\three`},
	}
	if !reflect.DeepEqual(summary, wantSummary) {
		t.Fatalf("summary = %#v, want %#v", summary, wantSummary)
	}
	input[0].Path = `Z:\mutated-input`
	summary.ByMachine["machine-a"] = 99
	summary.Samples[0] = `Z:\mutated-summary`

	got, err := store.Consume(token)
	if err != nil {
		t.Fatal(err)
	}
	want := []DeleteMember{
		{FileID: 1, MachineID: "machine-a", Path: `A:\one`, Size: 10},
		{FileID: 2, MachineID: "machine-a", Path: `Z:\two`, Size: 20},
		{FileID: 3, MachineID: "machine-b", Path: `Z:\three`, Size: 30},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Consume() = %#v, want %#v", got, want)
	}
	got[0].Path = `Z:\mutated-consume`
	if _, err := store.Consume(token); !errors.Is(err, ErrConfirmationConsumed) {
		t.Fatalf("second Consume() error = %v, want consumed", err)
	}
}

func TestConfirmStoreReturnsAtMostTwentyCanonicalSamples(t *testing.T) {
	store := NewConfirmStore(time.Minute, time.Now)
	members := make([]DeleteMember, 25)
	for index := range members {
		members[index] = DeleteMember{
			FileID:    int64(index + 1),
			MachineID: "machine-a",
			Path:      fmt.Sprintf(`Z:\%02d`, 24-index),
			Size:      1,
		}
	}
	_, summary, err := store.Create(members)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Samples) != 20 || summary.Samples[0] != `Z:\00` ||
		summary.Samples[19] != `Z:\19` {
		t.Fatalf("Samples = %#v, want canonical first 20", summary.Samples)
	}
}

func TestConfirmStoreRejectsInvalidAndConflictingMembersAndOverflow(t *testing.T) {
	valid := DeleteMember{FileID: 1, MachineID: "machine", Path: `Z:\one`, Size: 1}
	tests := []struct {
		name    string
		members []DeleteMember
	}{
		{name: "empty"},
		{name: "non-positive file id", members: []DeleteMember{{FileID: 0, MachineID: "machine", Path: `Z:\one`}}},
		{name: "empty machine", members: []DeleteMember{{FileID: 1, Path: `Z:\one`}}},
		{name: "empty path", members: []DeleteMember{{FileID: 1, MachineID: "machine"}}},
		{name: "negative size", members: []DeleteMember{{FileID: 1, MachineID: "machine", Path: `Z:\one`, Size: -1}}},
		{name: "conflicting id", members: []DeleteMember{valid, {FileID: 1, MachineID: "machine", Path: `Z:\other`, Size: 1}}},
		{name: "conflicting machine path", members: []DeleteMember{valid, {FileID: 2, MachineID: "machine", Path: `Z:\one`, Size: 1}}},
		{name: "byte overflow", members: []DeleteMember{
			{FileID: 1, MachineID: "machine", Path: `Z:\one`, Size: math.MaxInt64},
			{FileID: 2, MachineID: "machine", Path: `Z:\two`, Size: 1},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := NewConfirmStore(time.Minute, time.Now)
			if _, _, err := store.Create(test.members); !errors.Is(err, ErrDeleteSelection) {
				t.Fatalf("Create() error = %v, want ErrDeleteSelection", err)
			}
		})
	}
}

func TestConfirmStoreTreatsMachineAndPathAsAnExactTuple(t *testing.T) {
	store := NewConfirmStore(time.Minute, time.Now)
	_, summary, err := store.Create([]DeleteMember{
		{FileID: 1, MachineID: "a\x00b", Path: "c", Size: 1},
		{FileID: 2, MachineID: "a", Path: "b\x00c", Size: 1},
	})
	if err != nil {
		t.Fatalf("distinct (machine,path) tuples were rejected: %v", err)
	}
	if summary.TotalFiles != 2 {
		t.Fatalf("total files = %d, want 2", summary.TotalFiles)
	}
}

func TestConfirmStoreConcurrentConsumeHasOneWinner(t *testing.T) {
	store := NewConfirmStore(time.Minute, time.Now)
	token, _, err := store.Create([]DeleteMember{{
		FileID: 1, MachineID: "machine", Path: `Z:\one`, Size: 1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	const callers = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan error, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := store.Consume(token)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var successes, consumed int
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrConfirmationConsumed):
			consumed++
		default:
			t.Fatalf("Consume() error = %v", err)
		}
	}
	if successes != 1 || consumed != callers-1 {
		t.Fatalf("successes/consumed = %d/%d, want 1/%d", successes, consumed, callers-1)
	}
}

func TestConfirmStoreFailsClosedAndBoundsTombstones(t *testing.T) {
	var nilStore *ConfirmStore
	if _, _, err := nilStore.Create(nil); !errors.Is(err, ErrDeleteUnavailable) {
		t.Fatalf("nil Create() error = %v", err)
	}
	for _, store := range []*ConfirmStore{
		NewConfirmStore(0, time.Now),
		NewConfirmStore(time.Minute, nil),
	} {
		if _, _, err := store.Create([]DeleteMember{{
			FileID: 1, MachineID: "machine", Path: `Z:\one`,
		}}); !errors.Is(err, ErrDeleteUnavailable) {
			t.Fatalf("misconfigured Create() error = %v", err)
		}
	}

	store := NewConfirmStore(time.Minute, time.Now)
	for index := 0; index < confirmTombstoneLimit+10; index++ {
		token, _, err := store.Create([]DeleteMember{{
			FileID: int64(index + 1), MachineID: "machine",
			Path: fmt.Sprintf(`Z:\%d`, index),
		}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Consume(token); err != nil {
			t.Fatal(err)
		}
	}
	store.mu.Lock()
	usedCount := len(store.used)
	store.mu.Unlock()
	if usedCount > confirmTombstoneLimit {
		t.Fatalf("used tombstones = %d, limit %d", usedCount, confirmTombstoneLimit)
	}
	if _, err := store.Consume("not-a-token"); !errors.Is(err, ErrConfirmationInvalid) {
		t.Fatalf("invalid token error = %v", err)
	}
}

func TestConfirmStoreLazilyPrunesAbandonedExpiredRecords(t *testing.T) {
	const ttl = time.Minute
	unknownToken := base64.RawURLEncoding.EncodeToString(make([]byte, 16))
	for _, operation := range []string{"create", "consume"} {
		t.Run(operation, func(t *testing.T) {
			now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
			store := NewConfirmStore(ttl, func() time.Time { return now })
			var tokens []string
			for index := 0; index < 3; index++ {
				token, _, err := store.Create([]DeleteMember{{
					FileID: int64(index + 1), MachineID: "machine",
					Path: fmt.Sprintf(`Z:\abandoned-%d`, index),
				}})
				if err != nil {
					t.Fatal(err)
				}
				tokens = append(tokens, token)
			}
			now = now.Add(ttl)
			switch operation {
			case "create":
				if _, _, err := store.Create([]DeleteMember{{
					FileID: 99, MachineID: "machine", Path: `Z:\live`,
				}}); err != nil {
					t.Fatal(err)
				}
			case "consume":
				if _, err := store.Consume(unknownToken); !errors.Is(err, ErrConfirmationInvalid) {
					t.Fatalf("unrelated Consume() error = %v", err)
				}
			}

			store.mu.Lock()
			active := len(store.records)
			expired := len(store.expired)
			_, abandonedRetained := store.records[tokens[len(tokens)-1]]
			store.mu.Unlock()
			wantActive := 0
			if operation == "create" {
				wantActive = 1
			}
			if active != wantActive || expired != 3 || abandonedRetained {
				t.Fatalf("active/expired/retained = %d/%d/%v, want %d/3/false",
					active, expired, abandonedRetained, wantActive)
			}
			if _, err := store.Consume(tokens[len(tokens)-1]); !errors.Is(err, ErrConfirmationExpired) {
				t.Fatalf("recent expired token error = %v, want ErrConfirmationExpired", err)
			}
		})
	}

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	store := NewConfirmStore(ttl, func() time.Time { return now })
	for index := 0; index < confirmTombstoneLimit+10; index++ {
		if _, _, err := store.Create([]DeleteMember{{
			FileID: int64(index + 1), MachineID: "machine",
			Path: fmt.Sprintf(`Z:\abandoned-bounded-%d`, index),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(ttl)
	if _, err := store.Consume(unknownToken); !errors.Is(err, ErrConfirmationInvalid) {
		t.Fatalf("unrelated Consume() error = %v", err)
	}
	store.mu.Lock()
	active := len(store.records)
	tombstones := len(store.used) + len(store.expired)
	store.mu.Unlock()
	if active != 0 || tombstones > confirmTombstoneLimit {
		t.Fatalf("active/tombstones = %d/%d, want 0/<=%d",
			active, tombstones, confirmTombstoneLimit)
	}
}

func TestConfirmStorePrunesExpiredRecordsBeforeInvalidCreateReturns(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	store := NewConfirmStore(time.Minute, func() time.Time { return now })
	token, _, err := store.Create([]DeleteMember{{
		FileID: 1, MachineID: "machine", Path: `Z:\abandoned`,
	}})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)

	if _, _, err := store.Create(nil); !errors.Is(err, ErrDeleteSelection) {
		t.Fatalf("Create(nil) error = %v, want ErrDeleteSelection", err)
	}
	store.mu.Lock()
	active := len(store.records)
	_, retained := store.records[token]
	_, expired := store.expired[token]
	tombstones := len(store.used) + len(store.expired)
	store.mu.Unlock()
	if active != 0 || retained || !expired || tombstones > confirmTombstoneLimit {
		t.Fatalf("active/retained/expired/tombstones = %d/%v/%v/%d",
			active, retained, expired, tombstones)
	}
	if _, err := store.Consume(token); !errors.Is(err, ErrConfirmationExpired) {
		t.Fatalf("Consume(expired token) error = %v, want ErrConfirmationExpired", err)
	}
}

type deleteTestSend struct {
	machineID string
	msgType   uint8
	task      proto.DeleteTask
}

type deleteTestTransport struct {
	mu          sync.Mutex
	online      map[string]bool
	onlineCalls map[string]int
	sendErrors  map[string]error
	sends       []deleteTestSend
	onSend      func(string, proto.DeleteTask)
}

func (transport *deleteTestTransport) IsOnline(machineID string) bool {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	transport.onlineCalls[machineID]++
	return transport.online[machineID]
}

func (transport *deleteTestTransport) Send(machineID string, msgType uint8, value any) error {
	task := *(value.(*proto.DeleteTask))
	task.Entries = append([]string(nil), task.Entries...)
	transport.mu.Lock()
	transport.sends = append(transport.sends, deleteTestSend{
		machineID: machineID, msgType: msgType, task: task,
	})
	err := transport.sendErrors[machineID]
	hook := transport.onSend
	transport.mu.Unlock()
	if hook != nil {
		hook(machineID, task)
	}
	return err
}

func newDeleteTestService(
	t *testing.T,
	members []DeleteMember,
	transport *deleteTestTransport,
) (*DeleteService, string) {
	t.Helper()
	if transport.onlineCalls == nil {
		transport.onlineCalls = make(map[string]int)
	}
	if transport.sendErrors == nil {
		transport.sendErrors = make(map[string]error)
	}
	confirms := NewConfirmStore(time.Minute, time.Now)
	token, _, err := confirms.Create(members)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewDeleteService(nil, transport, confirms, logger), token
}

func TestDeleteExecuteValidatesModeBeforeConsumeAndDispatchesCanonicalUnsplitTasks(t *testing.T) {
	transport := &deleteTestTransport{
		online: map[string]bool{
			"machine-a": true,
			"machine-b": true,
			"machine-c": false,
		},
		onlineCalls: make(map[string]int),
		sendErrors:  map[string]error{"machine-b": errors.New("private transport detail")},
	}
	service, token := newDeleteTestService(t, []DeleteMember{
		{FileID: 4, MachineID: "machine-c", Path: `Z:\offline`, Size: 4},
		{FileID: 3, MachineID: "machine-b", Path: `Z:\send-error`, Size: 3},
		{FileID: 2, MachineID: "machine-a", Path: `Z:\two`, Size: 2},
		{FileID: 1, MachineID: "machine-a", Path: `A:\one`, Size: 1},
	}, transport)

	if _, err := service.Execute(context.Background(), token, "SOFT"); !errors.Is(err, ErrDeleteMode) {
		t.Fatalf("invalid mode error = %v, want ErrDeleteMode", err)
	}
	taskID, err := service.Execute(context.Background(), token, "hard")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uuid.Parse(taskID); err != nil {
		t.Fatalf("task ID %q is not a canonical UUID: %v", taskID, err)
	}
	if len(transport.sends) != 2 {
		t.Fatalf("sends = %#v, want machine-a and machine-b", transport.sends)
	}
	for _, machineID := range []string{"machine-a", "machine-b", "machine-c"} {
		if transport.onlineCalls[machineID] != 1 {
			t.Fatalf("IsOnline(%q) calls = %d, want 1", machineID, transport.onlineCalls[machineID])
		}
	}
	first := transport.sends[0]
	if first.machineID != "machine-a" || first.msgType != proto.MsgDeleteTask ||
		first.task.TaskID != taskID || first.task.Seq != 0 || first.task.LastSeq != 0 ||
		first.task.Mode != "hard" || !first.task.Confirmed ||
		!reflect.DeepEqual(first.task.Entries, []string{`A:\one`, `Z:\two`}) {
		t.Fatalf("first send = %#v", first)
	}
	second := transport.sends[1]
	if second.machineID != "machine-b" || second.task.TaskID != taskID ||
		!reflect.DeepEqual(second.task.Entries, []string{`Z:\send-error`}) {
		t.Fatalf("second send = %#v", second)
	}

	status, ok := service.Status(taskID)
	if !ok {
		t.Fatal("task status not found")
	}
	if status.Total != 4 || status.Pending != 2 || status.Failed != 2 ||
		status.Uncertain != 0 || status.Complete {
		t.Fatalf("status = %#v", status)
	}
	if status.ErrorCodes[proto.DeleteErrHelperLost] != 2 {
		t.Fatalf("error codes = %#v", status.ErrorCodes)
	}
	for _, problem := range status.Problems {
		if problem.ErrorCode != proto.DeleteErrHelperLost || problem.Uncertain ||
			problem.ErrorMessage == "private transport detail" {
			t.Fatalf("dispatch problem = %#v", problem)
		}
	}
}

type deleteErrorDB struct {
	queryErr error
	querySQL string
	rows     pgx.Rows
}

func (db *deleteErrorDB) Query(
	_ context.Context,
	query string,
	_ ...any,
) (pgx.Rows, error) {
	db.querySQL = query
	return db.rows, db.queryErr
}

func (*deleteErrorDB) QueryRow(context.Context, string, ...any) pgx.Row {
	return nil
}

type deleteErrorRows struct {
	scanErr error
	rowsErr error
	next    bool
}

func (*deleteErrorRows) Close() {}

func (rows *deleteErrorRows) Err() error {
	return rows.rowsErr
}

func (*deleteErrorRows) CommandTag() pgconn.CommandTag {
	return pgconn.CommandTag{}
}

func (*deleteErrorRows) FieldDescriptions() []pgconn.FieldDescription {
	return nil
}

func (rows *deleteErrorRows) Next() bool {
	if !rows.next {
		return false
	}
	rows.next = false
	return true
}

func (rows *deleteErrorRows) Scan(...any) error {
	return rows.scanErr
}

func (*deleteErrorRows) Values() ([]any, error) {
	return nil, nil
}

func (*deleteErrorRows) RawValues() [][]byte {
	return nil
}

func (*deleteErrorRows) Conn() *pgx.Conn {
	return nil
}

func TestDeletePrepareSanitizesDatabaseFailures(t *testing.T) {
	const secret = "postgres://private-user:private-password@db/secret-marker"
	tests := []struct {
		name string
		db   *deleteErrorDB
		want string
	}{
		{
			name: "query",
			db: &deleteErrorDB{
				queryErr: errors.New("query failed " + secret),
			},
			want: "delete service unavailable: database query failed",
		},
		{
			name: "scan",
			db: &deleteErrorDB{
				rows: &deleteErrorRows{
					next:    true,
					scanErr: errors.New("scan failed " + secret),
				},
			},
			want: "delete service unavailable: database row scan failed",
		},
		{
			name: "iteration",
			db: &deleteErrorDB{
				rows: &deleteErrorRows{
					rowsErr: errors.New("iteration failed " + secret),
				},
			},
			want: "delete service unavailable: database row iteration failed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := NewDeleteService(
				test.db,
				nil,
				NewConfirmStore(time.Minute, time.Now),
				slog.New(slog.NewTextHandler(io.Discard, nil)),
			)
			_, _, err := service.Prepare(context.Background(), []int64{1})
			if !errors.Is(err, ErrDeleteUnavailable) {
				t.Fatalf("Prepare() error = %v, want ErrDeleteUnavailable", err)
			}
			if err.Error() != test.want {
				t.Fatalf("Prepare() error = %q, want %q", err, test.want)
			}
			if strings.Contains(err.Error(), secret) ||
				strings.Contains(err.Error(), "private-password") {
				t.Fatalf("Prepare() leaked database secret: %q", err)
			}
		})
	}
}

func TestDeletePrepareProtectsTheEffectiveLiveGroupRepresentative(t *testing.T) {
	db := &deleteErrorDB{queryErr: errors.New("stop after SQL capture")}
	service := NewDeleteService(
		db,
		nil,
		NewConfirmStore(time.Minute, time.Now),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	_, _, _ = service.Prepare(context.Background(), []int64{1})
	sql := strings.Join(strings.Fields(db.querySQL), " ")
	for _, fragment := range []string{
		"JOIN dup_members AS requested_members ON requested_members.group_id=representative_groups.id AND requested_members.file_id=requested.id",
		"FROM dup_members AS effective_members",
		"effective_members.group_id=representative_groups.id",
		"effective_files.status <> 'deleted'",
		"CASE WHEN effective_files.id=representative_groups.representative_file_id THEN 0 ELSE 1 END",
		"effective_representative.id=requested.id",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("prepare SQL missing %q:\n%s", fragment, sql)
		}
	}
}

func TestDeleteExecuteDefaultsSoftAndRegistersBeforeSynchronousReport(t *testing.T) {
	transport := &deleteTestTransport{
		online:      map[string]bool{"machine": true},
		onlineCalls: make(map[string]int),
		sendErrors:  make(map[string]error),
	}
	service, token := newDeleteTestService(t, []DeleteMember{{
		FileID: 1, MachineID: "machine", Path: `Z:\one`, Size: 1,
	}}, transport)
	transport.onSend = func(machineID string, task proto.DeleteTask) {
		service.HandleReport(machineID, &proto.DeleteReport{
			TaskID:  task.TaskID,
			Stats:   proto.DeleteStats{Total: 1, OK: 1},
			Entries: []proto.DeleteResult{{Path: `Z:\one`, OK: true}},
		})
	}
	taskID, err := service.Execute(context.Background(), token, "")
	if err != nil {
		t.Fatal(err)
	}
	if transport.sends[0].task.Mode != "soft" {
		t.Fatalf("default mode = %q, want soft", transport.sends[0].task.Mode)
	}
	status, ok := service.Status(taskID)
	if !ok || !status.Complete || status.OK != 1 || status.Pending != 0 {
		t.Fatalf("synchronous status = %#v, found=%v", status, ok)
	}
}

func TestDeleteSendErrorOverridesSynchronousReportForEveryMachineMember(t *testing.T) {
	transport := &deleteTestTransport{
		online:      map[string]bool{"machine": true},
		onlineCalls: make(map[string]int),
		sendErrors:  map[string]error{"machine": errors.New("send failed")},
	}
	service, token := newDeleteTestService(t, []DeleteMember{{
		FileID: 1, MachineID: "machine", Path: `Z:\one`, Size: 1,
	}}, transport)
	transport.onSend = func(machineID string, task proto.DeleteTask) {
		service.HandleReport(machineID, &proto.DeleteReport{
			TaskID:  task.TaskID,
			Stats:   proto.DeleteStats{Total: 1, OK: 1},
			Entries: []proto.DeleteResult{{Path: `Z:\one`, OK: true}},
		})
	}
	taskID, err := service.Execute(context.Background(), token, "soft")
	if err != nil {
		t.Fatal(err)
	}
	status, _ := service.Status(taskID)
	if !status.Complete || status.OK != 0 || status.Failed != 1 ||
		status.Uncertain != 0 ||
		status.ErrorCodes[proto.DeleteErrHelperLost] != 1 {
		t.Fatalf("send-error status = %#v", status)
	}
}

func TestDeleteExecuteMisconfigurationDoesNotConsumeToken(t *testing.T) {
	confirms := NewConfirmStore(time.Minute, time.Now)
	token, _, err := confirms.Create([]DeleteMember{{
		FileID: 1, MachineID: "machine", Path: `Z:\one`, Size: 1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	broken := NewDeleteService(nil, nil, confirms, nil)
	if _, err := broken.Execute(context.Background(), token, "soft"); !errors.Is(err, ErrDeleteUnavailable) {
		t.Fatalf("misconfigured Execute() error = %v", err)
	}
	transport := &deleteTestTransport{
		online:      map[string]bool{"machine": false},
		onlineCalls: make(map[string]int),
		sendErrors:  make(map[string]error),
	}
	working := NewDeleteService(nil, transport, confirms, nil)
	if _, err := working.Execute(context.Background(), token, "soft"); err != nil {
		t.Fatalf("token was consumed by misconfigured service: %v", err)
	}
}

func TestDeleteReportsAggregateOutOfOrderIdempotentlyAndDeepCopyStatus(t *testing.T) {
	transport := &deleteTestTransport{
		online:      map[string]bool{"machine": true},
		onlineCalls: make(map[string]int),
		sendErrors:  make(map[string]error),
	}
	service, token := newDeleteTestService(t, []DeleteMember{
		{FileID: 1, MachineID: "machine", Path: `A:\one`, Size: 1},
		{FileID: 2, MachineID: "machine", Path: `B:\two`, Size: 1},
		{FileID: 3, MachineID: "machine", Path: `C:\three`, Size: 1},
	}, transport)
	taskID, err := service.Execute(context.Background(), token, "soft")
	if err != nil {
		t.Fatal(err)
	}
	second := &proto.DeleteReport{
		TaskID: taskID, Seq: 1, LastSeq: 1,
		Stats:   proto.DeleteStats{Total: 1, OK: 1},
		Entries: []proto.DeleteResult{{Path: `C:\three`, OK: true}},
	}
	service.HandleReport("machine", second)
	service.HandleReport("machine", second)
	first := &proto.DeleteReport{
		TaskID: taskID, Seq: 0, LastSeq: 1,
		Stats: proto.DeleteStats{Total: 2, OK: 1, Failed: 1, Uncertain: 1},
		Entries: []proto.DeleteResult{
			{Path: `A:\one`, OK: true, StateSyncErr: "database update failed"},
			{Path: `B:\two`, ErrCode: proto.DeleteErrDeleteFailed, Err: "helper failure", Uncertain: true},
		},
	}
	service.HandleReport("machine", first)

	status, ok := service.Status(taskID)
	if !ok || !status.Complete || status.Total != 3 || status.OK != 2 ||
		status.Failed != 1 || status.Uncertain != 1 || status.Pending != 0 ||
		status.StateSyncFailures != 1 {
		t.Fatalf("status = %#v, found=%v", status, ok)
	}
	if status.ErrorCodes[proto.DeleteErrDeleteFailed] != 1 ||
		len(status.Problems) != 2 ||
		status.Problems[0].Path != `A:\one` ||
		status.Problems[1].Path != `B:\two` {
		t.Fatalf("problem aggregation = %#v codes=%#v", status.Problems, status.ErrorCodes)
	}
	status.ErrorCodes[proto.DeleteErrDeleteFailed] = 99
	status.Problems[0].Path = `Z:\mutated`
	status.ByMachine["machine"] = DeleteMachineStatus{}
	again, _ := service.Status(taskID)
	if again.ErrorCodes[proto.DeleteErrDeleteFailed] != 1 ||
		again.Problems[0].Path != `A:\one` ||
		again.ByMachine["machine"].OK != 2 {
		t.Fatalf("Status() was not deeply copied: %#v", again)
	}
}

func TestDeleteConflictingDuplicateAndMalformedReportFailClosed(t *testing.T) {
	tests := []struct {
		name   string
		second *proto.DeleteReport
	}{
		{
			name: "conflicting duplicate",
			second: &proto.DeleteReport{
				Seq: 0, LastSeq: 1,
				Stats: proto.DeleteStats{Total: 1, Failed: 1},
				Entries: []proto.DeleteResult{{
					Path: `A:\one`, ErrCode: proto.DeleteErrDeleteFailed,
				}},
			},
		},
		{
			name: "malformed stats",
			second: &proto.DeleteReport{
				Stats:   proto.DeleteStats{Total: 2, OK: 2},
				Entries: []proto.DeleteResult{{Path: `A:\one`, OK: true}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport := &deleteTestTransport{
				online:      map[string]bool{"machine": true},
				onlineCalls: make(map[string]int),
				sendErrors:  make(map[string]error),
			}
			service, token := newDeleteTestService(t, []DeleteMember{
				{FileID: 1, MachineID: "machine", Path: `A:\one`, Size: 1},
				{FileID: 2, MachineID: "machine", Path: `B:\two`, Size: 1},
			}, transport)
			taskID, err := service.Execute(context.Background(), token, "soft")
			if err != nil {
				t.Fatal(err)
			}
			if test.name == "conflicting duplicate" {
				service.HandleReport("machine", &proto.DeleteReport{
					TaskID: taskID, Seq: 0, LastSeq: 1,
					Stats:   proto.DeleteStats{Total: 1, OK: 1},
					Entries: []proto.DeleteResult{{Path: `A:\one`, OK: true}},
				})
			}
			test.second.TaskID = taskID
			service.HandleReport("machine", test.second)
			status, _ := service.Status(taskID)
			if !status.Complete || status.Pending != 0 {
				t.Fatalf("status did not fail closed: %#v", status)
			}
			if status.ErrorCodes[proto.DeleteErrDeleteFailed] == 0 {
				t.Fatalf("error codes = %#v", status.ErrorCodes)
			}
		})
	}
}

func TestDeleteCompletedSingleSequenceConflictingDuplicateFailsClosed(t *testing.T) {
	transport := &deleteTestTransport{
		online:      map[string]bool{"machine": true},
		onlineCalls: make(map[string]int),
		sendErrors:  make(map[string]error),
	}
	service, token := newDeleteTestService(t, []DeleteMember{{
		FileID: 1, MachineID: "machine", Path: `A:\one`, Size: 1,
	}}, transport)
	taskID, err := service.Execute(context.Background(), token, "soft")
	if err != nil {
		t.Fatal(err)
	}
	success := &proto.DeleteReport{
		TaskID:  taskID,
		Stats:   proto.DeleteStats{Total: 1, OK: 1},
		Entries: []proto.DeleteResult{{Path: `A:\one`, OK: true}},
	}
	service.HandleReport("machine", success)
	completed, _ := service.Status(taskID)
	if !completed.Complete || completed.OK != 1 {
		t.Fatalf("initial completion = %#v", completed)
	}

	service.HandleReport("machine", &proto.DeleteReport{
		TaskID: taskID,
		Stats:  proto.DeleteStats{Total: 1, Failed: 1, Uncertain: 1},
		Entries: []proto.DeleteResult{{
			Path:      `A:\one`,
			ErrCode:   proto.DeleteErrDeleteFailed,
			Err:       "contradictory result",
			Uncertain: true,
		}},
	})
	status, _ := service.Status(taskID)
	if !status.Complete || status.OK != 0 || status.Failed != 1 ||
		status.Uncertain != 1 ||
		status.ErrorCodes[proto.DeleteErrDeleteFailed] != 1 {
		t.Fatalf("conflicting duplicate status = %#v", status)
	}
}

func TestDeleteStatusDeadlineFinalizesOnlyUnresolvedPaths(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	transport := &deleteTestTransport{
		online:      map[string]bool{"machine": true},
		onlineCalls: make(map[string]int),
		sendErrors:  make(map[string]error),
	}
	service, token := newDeleteTestService(t, []DeleteMember{
		{FileID: 1, MachineID: "machine", Path: `A:\one`, Size: 1},
		{FileID: 2, MachineID: "machine", Path: `B:\two`, Size: 1},
	}, transport)
	service.now = func() time.Time { return now }
	taskID, err := service.Execute(context.Background(), token, "soft")
	if err != nil {
		t.Fatal(err)
	}
	service.HandleReport("machine", &proto.DeleteReport{
		TaskID: taskID, Seq: 0, LastSeq: 1,
		Stats:   proto.DeleteStats{Total: 1, OK: 1},
		Entries: []proto.DeleteResult{{Path: `A:\one`, OK: true}},
	})
	beforeDeadline, _ := service.Status(taskID)
	missingSequence, ok := beforeDeadline.ByMachine["machine"].Sequences[1]
	if !ok || missingSequence.Received {
		t.Fatalf("missing sequence status = %#v, found=%v", missingSequence, ok)
	}
	now = now.Add(deleteReportDeadline)
	status, _ := service.Status(taskID)
	if !status.Complete || status.OK != 1 || status.Failed != 1 ||
		status.Uncertain != 1 || status.ErrorCodes[proto.DeleteErrHelperLost] != 1 {
		t.Fatalf("deadline status = %#v", status)
	}
	service.HandleReport("machine", &proto.DeleteReport{
		TaskID: taskID, Seq: 0, LastSeq: 1,
		Stats: proto.DeleteStats{Total: 1, Failed: 1, Uncertain: 1},
		Entries: []proto.DeleteResult{{
			Path:      `A:\one`,
			ErrCode:   proto.DeleteErrDeleteFailed,
			Uncertain: true,
		}},
	})
	service.HandleReport("machine", &proto.DeleteReport{
		TaskID: taskID, Seq: 1, LastSeq: 1,
		Stats:   proto.DeleteStats{Total: 1, OK: 1},
		Entries: []proto.DeleteResult{{Path: `B:\two`, OK: true}},
	})
	late, _ := service.Status(taskID)
	if late.OK != 1 || late.Failed != 1 {
		t.Fatalf("late report revived task: %#v", late)
	}
}

func TestDeleteReportsFailClosedWhenAdvertisedSequencesOmitExpectedPath(t *testing.T) {
	transport := &deleteTestTransport{
		online:      map[string]bool{"machine": true},
		onlineCalls: make(map[string]int),
		sendErrors:  make(map[string]error),
	}
	service, token := newDeleteTestService(t, []DeleteMember{
		{FileID: 1, MachineID: "machine", Path: `A:\one`, Size: 1},
		{FileID: 2, MachineID: "machine", Path: `B:\two`, Size: 1},
	}, transport)
	taskID, err := service.Execute(context.Background(), token, "soft")
	if err != nil {
		t.Fatal(err)
	}
	service.HandleReport("machine", &proto.DeleteReport{
		TaskID:  taskID,
		Stats:   proto.DeleteStats{Total: 1, OK: 1},
		Entries: []proto.DeleteResult{{Path: `A:\one`, OK: true}},
	})
	status, _ := service.Status(taskID)
	if !status.Complete || status.OK != 1 || status.Failed != 1 ||
		status.Uncertain != 1 ||
		status.ErrorCodes[proto.DeleteErrDeleteFailed] != 1 {
		t.Fatalf("omitted-path status = %#v", status)
	}
}

func TestDeleteReportStateIsRaceSafe(t *testing.T) {
	transport := &deleteTestTransport{
		online:      map[string]bool{"machine": true},
		onlineCalls: make(map[string]int),
		sendErrors:  make(map[string]error),
	}
	service, token := newDeleteTestService(t, []DeleteMember{{
		FileID: 1, MachineID: "machine", Path: `A:\one`, Size: 1,
	}}, transport)
	taskID, err := service.Execute(context.Background(), token, "soft")
	if err != nil {
		t.Fatal(err)
	}
	report := &proto.DeleteReport{
		TaskID:  taskID,
		Stats:   proto.DeleteStats{Total: 1, OK: 1},
		Entries: []proto.DeleteResult{{Path: `A:\one`, OK: true}},
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			service.HandleReport("machine", report)
		}()
		go func() {
			defer wg.Done()
			_, _ = service.Status(taskID)
		}()
	}
	wg.Wait()
	status, _ := service.Status(taskID)
	if !status.Complete || status.OK != 1 || status.Total != 1 {
		t.Fatalf("concurrent status = %#v", status)
	}
}
