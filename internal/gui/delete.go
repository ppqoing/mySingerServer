package gui

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"dedup/internal/proto"
)

var (
	ErrConfirmationInvalid  = errors.New("invalid confirmation token")
	ErrConfirmationExpired  = errors.New("expired confirmation token")
	ErrConfirmationConsumed = errors.New("confirmation token already consumed")
	ErrDeleteMode           = errors.New("invalid delete mode")
	ErrDeleteSelection      = errors.New("conflicting or invalid member selection")
	ErrDeleteUnavailable    = errors.New("delete service unavailable")
)

type DeleteMember struct {
	FileID    int64
	MachineID string
	Path      string
	Size      int64
}

type DeleteSummary struct {
	TotalFiles int64            `json:"total_files"`
	TotalBytes int64            `json:"total_bytes"`
	ByMachine  map[string]int64 `json:"by_machine"`
	Samples    []string         `json:"samples"`
}

type confirmationRecord struct {
	created time.Time
	members []DeleteMember
}

const confirmTombstoneLimit = 1024

type ConfirmStore struct {
	mu             sync.Mutex
	ttl            time.Duration
	now            func() time.Time
	records        map[string]confirmationRecord
	used           map[string]string
	expired        map[string]struct{}
	tombstoneOrder []string
}

func NewConfirmStore(ttl time.Duration, now func() time.Time) *ConfirmStore {
	return &ConfirmStore{
		ttl:     ttl,
		now:     now,
		records: make(map[string]confirmationRecord),
		used:    make(map[string]string),
		expired: make(map[string]struct{}),
	}
}

func (s *ConfirmStore) Create(members []DeleteMember) (string, DeleteSummary, error) {
	if s == nil || s.ttl <= 0 || s.now == nil {
		return "", DeleteSummary{}, ErrDeleteUnavailable
	}
	s.mu.Lock()
	s.pruneExpiredLocked(s.now())
	s.mu.Unlock()
	canonical, summary, err := canonicalizeDeleteMembers(members)
	if err != nil {
		return "", DeleteSummary{}, err
	}
	for {
		tokenBytes := make([]byte, 16)
		if _, err := rand.Read(tokenBytes); err != nil {
			return "", DeleteSummary{}, ErrDeleteUnavailable
		}
		token := base64.RawURLEncoding.EncodeToString(tokenBytes)
		s.mu.Lock()
		now := s.now()
		s.pruneExpiredLocked(now)
		_, active := s.records[token]
		_, used := s.used[token]
		_, expired := s.expired[token]
		if !active && !used && !expired {
			s.records[token] = confirmationRecord{
				created: now,
				members: append([]DeleteMember(nil), canonical...),
			}
			s.mu.Unlock()
			return token, cloneDeleteSummary(summary), nil
		}
		s.mu.Unlock()
	}
}

func (s *ConfirmStore) Consume(token string) ([]DeleteMember, error) {
	return s.ConsumeWithTask(token, "")
}

// ConsumeWithTask consumes the token like Consume and, on success, records
// taskID in the tombstone so a repeated execute of the same token can be
// answered with the first accepted task instead of a conflict.
func (s *ConfirmStore) ConsumeWithTask(
	token string,
	taskID string,
) ([]DeleteMember, error) {
	if s == nil || s.ttl <= 0 || s.now == nil {
		return nil, ErrDeleteUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.pruneExpiredLocked(now)
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(token) != 22 || len(decoded) != 16 {
		return nil, ErrConfirmationInvalid
	}
	if _, ok := s.used[token]; ok {
		return nil, ErrConfirmationConsumed
	}
	if _, ok := s.expired[token]; ok {
		return nil, ErrConfirmationExpired
	}
	record, ok := s.records[token]
	if !ok {
		return nil, ErrConfirmationInvalid
	}
	delete(s.records, token)
	if !now.Before(record.created.Add(s.ttl)) {
		s.expired[token] = struct{}{}
		s.retainTombstoneLocked(token)
		return nil, ErrConfirmationExpired
	}
	s.used[token] = taskID
	s.retainTombstoneLocked(token)
	return append([]DeleteMember(nil), record.members...), nil
}

// ConsumedTaskID reports the task ID recorded when the token was consumed by
// an execute. Tokens consumed without a task (or never consumed) report "".
func (s *ConfirmStore) ConsumedTaskID(token string) (string, bool) {
	if s == nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	taskID, ok := s.used[token]
	return taskID, ok
}

func (s *ConfirmStore) pruneExpiredLocked(now time.Time) {
	for token, record := range s.records {
		if now.Before(record.created.Add(s.ttl)) {
			continue
		}
		delete(s.records, token)
		s.expired[token] = struct{}{}
		s.retainTombstoneLocked(token)
	}
}

func (s *ConfirmStore) retainTombstoneLocked(token string) {
	s.tombstoneOrder = append(s.tombstoneOrder, token)
	for len(s.tombstoneOrder) > confirmTombstoneLimit {
		oldest := s.tombstoneOrder[0]
		s.tombstoneOrder[0] = ""
		s.tombstoneOrder = s.tombstoneOrder[1:]
		delete(s.used, oldest)
		delete(s.expired, oldest)
	}
}

func canonicalizeDeleteMembers(members []DeleteMember) ([]DeleteMember, DeleteSummary, error) {
	if len(members) == 0 || uint64(len(members)) > math.MaxInt64 {
		return nil, DeleteSummary{}, ErrDeleteSelection
	}
	type machinePath struct {
		machineID string
		path      string
	}
	byID := make(map[int64]DeleteMember, len(members))
	byPath := make(map[machinePath]DeleteMember, len(members))
	for _, member := range members {
		if member.FileID <= 0 || member.MachineID == "" || member.Path == "" ||
			member.Size < 0 {
			return nil, DeleteSummary{}, ErrDeleteSelection
		}
		if existing, ok := byID[member.FileID]; ok {
			if existing != member {
				return nil, DeleteSummary{}, ErrDeleteSelection
			}
			continue
		}
		pathKey := machinePath{machineID: member.MachineID, path: member.Path}
		if existing, ok := byPath[pathKey]; ok {
			if existing != member {
				return nil, DeleteSummary{}, ErrDeleteSelection
			}
			continue
		}
		byID[member.FileID] = member
		byPath[pathKey] = member
	}
	canonical := make([]DeleteMember, 0, len(byID))
	for _, member := range byID {
		canonical = append(canonical, member)
	}
	sort.Slice(canonical, func(left, right int) bool {
		if canonical[left].MachineID != canonical[right].MachineID {
			return canonical[left].MachineID < canonical[right].MachineID
		}
		if canonical[left].Path != canonical[right].Path {
			return canonical[left].Path < canonical[right].Path
		}
		return canonical[left].FileID < canonical[right].FileID
	})
	summary := DeleteSummary{
		TotalFiles: int64(len(canonical)),
		ByMachine:  make(map[string]int64),
	}
	for _, member := range canonical {
		if member.Size > math.MaxInt64-summary.TotalBytes {
			return nil, DeleteSummary{}, ErrDeleteSelection
		}
		summary.TotalBytes += member.Size
		if summary.ByMachine[member.MachineID] == math.MaxInt64 {
			return nil, DeleteSummary{}, ErrDeleteSelection
		}
		summary.ByMachine[member.MachineID]++
		if len(summary.Samples) < 20 {
			summary.Samples = append(summary.Samples, member.Path)
		}
	}
	return canonical, summary, nil
}

func cloneDeleteSummary(summary DeleteSummary) DeleteSummary {
	clone := summary
	clone.ByMachine = make(map[string]int64, len(summary.ByMachine))
	for machineID, count := range summary.ByMachine {
		clone.ByMachine[machineID] = count
	}
	clone.Samples = append([]string(nil), summary.Samples...)
	return clone
}

type DeleteTransport interface {
	Send(machineID string, msgType uint8, value any) error
	IsOnline(machineID string) bool
}

type DeleteProblemItem struct {
	MachineID    string `json:"machine_id"`
	Sequence     uint32 `json:"sequence"`
	Path         string `json:"path"`
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	Uncertain    bool   `json:"uncertain"`
	StateSyncErr string `json:"state_sync_err,omitempty"`
}

type DeleteSequenceStatus struct {
	Sequence  uint32 `json:"sequence"`
	LastSeq   uint32 `json:"last_seq"`
	Received  bool   `json:"received"`
	Total     int64  `json:"total"`
	OK        int64  `json:"ok"`
	Failed    int64  `json:"failed"`
	Uncertain int64  `json:"uncertain"`
}

type DeleteMachineStatus struct {
	MachineID         string                          `json:"machine_id"`
	Total             int64                           `json:"total"`
	OK                int64                           `json:"ok"`
	Failed            int64                           `json:"failed"`
	Uncertain         int64                           `json:"uncertain"`
	Pending           int64                           `json:"pending"`
	Complete          bool                            `json:"complete"`
	StateSyncFailures int64                           `json:"state_sync_failures"`
	Sequences         map[uint32]DeleteSequenceStatus `json:"sequences"`
	// RecycledTo maps each soft-deleted path to the recycle destination
	// reported by the agent. Absent for hard mode or when nothing recycled.
	RecycledTo map[string]string `json:"recycled_to,omitempty"`
}

type DeleteTaskStatus struct {
	TaskID            string                         `json:"task_id"`
	Mode              string                         `json:"mode"`
	Total             int64                          `json:"total"`
	OK                int64                          `json:"ok"`
	Failed            int64                          `json:"failed"`
	Uncertain         int64                          `json:"uncertain"`
	Pending           int64                          `json:"pending"`
	Complete          bool                           `json:"complete"`
	StateSyncFailures int64                          `json:"state_sync_failures"`
	ByMachine         map[string]DeleteMachineStatus `json:"by_machine"`
	ErrorCodes        map[string]int64               `json:"error_codes"`
	Problems          []DeleteProblemItem            `json:"problems"`
}

const deleteReportDeadline = 12 * time.Minute

// deleteTerminalRetention is how long a finished task stays queryable before
// its state is reclaimed, bounding the otherwise unbounded tasks map growth.
const deleteTerminalRetention = 30 * time.Minute

type deleteStoredResult struct {
	sequence uint32
	result   proto.DeleteResult
}

type deleteMachineState struct {
	expected     map[string]DeleteMember
	results      map[string]deleteStoredResult
	reports      map[uint32]proto.DeleteReport
	pathSequence map[string]uint32
	lastSeq      uint32
	lastSeqKnown bool
	terminal     bool
}

type deleteTaskState struct {
	taskID           string
	mode             string
	createdAt        time.Time
	deadline         time.Time
	machines         map[string]*deleteMachineState
	terminal         bool
	deadlineTerminal bool
	terminalAt       time.Time
	// snapshot is set for tasks restored from delete_tasks after a restart.
	// Snapshot-backed tasks serve the persisted status verbatim (the member
	// detail needed to validate late reports is not persisted), so reports
	// for them are dropped; once the report deadline passes again the
	// snapshot flips to a deadline-exceeded terminal state.
	snapshot *DeleteTaskStatus
}

// deleteTaskStore is the persistence backend for delete task snapshots;
// *pgxpool.Pool and pgx.Tx both satisfy it.
type deleteTaskStore interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type DeleteService struct {
	db        groupQueryDB
	transport DeleteTransport
	confirms  *ConfirmStore
	logger    *slog.Logger
	store     deleteTaskStore

	mu    sync.Mutex
	tasks map[string]*deleteTaskState
	now   func() time.Time
}

func NewDeleteService(
	db groupQueryDB,
	transport DeleteTransport,
	confirms *ConfirmStore,
	logger *slog.Logger,
) *DeleteService {
	if logger == nil {
		logger = slog.Default()
	}
	return &DeleteService{
		db:        db,
		transport: transport,
		confirms:  confirms,
		logger:    logger,
		tasks:     make(map[string]*deleteTaskState),
		now:       time.Now,
	}
}

// SetTaskStore attaches the delete_tasks persistence backend. Without a
// store the service degrades to in-memory only.
func (s *DeleteService) SetTaskStore(store deleteTaskStore) {
	if s == nil {
		return
	}
	s.store = store
}

// Restore reloads non-terminal delete tasks persisted before a restart, like
// TaskRegistry.Restore does for scan tasks. The in-memory map stays the
// runtime authority; restored rows never overwrite in-memory state.
func (s *DeleteService) Restore(ctx context.Context) error {
	if s == nil || s.store == nil {
		return nil
	}
	rows, err := s.store.Query(ctx, `
		SELECT id, mode, status_json, created_at
		FROM delete_tasks
		WHERE COALESCE(status_json->>'complete','false') <> 'true'
		ORDER BY created_at, id;`)
	if err != nil {
		return fmt.Errorf("restore delete tasks: query: %w", err)
	}
	defer rows.Close()

	restored := make([]*deleteTaskState, 0)
	for rows.Next() {
		var (
			taskID     string
			mode       string
			statusJSON []byte
			createdAt  time.Time
		)
		if err := rows.Scan(&taskID, &mode, &statusJSON, &createdAt); err != nil {
			return fmt.Errorf("restore delete tasks: scan: %w", err)
		}
		if taskID == "" || (mode != proto.ModeSoft && mode != proto.ModeHard) {
			return fmt.Errorf("restore delete task %s: invalid envelope", taskID)
		}
		var status DeleteTaskStatus
		if err := json.Unmarshal(statusJSON, &status); err != nil {
			return fmt.Errorf("restore delete task %s status: %w", taskID, err)
		}
		status.TaskID = taskID
		status.Mode = mode
		snapshot := status
		restored = append(restored, &deleteTaskState{
			taskID:    taskID,
			mode:      mode,
			createdAt: createdAt,
			deadline:  s.now().Add(deleteReportDeadline),
			machines:  make(map[string]*deleteMachineState),
			snapshot:  &snapshot,
		})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("restore delete tasks: rows: %w", err)
	}
	s.mu.Lock()
	for _, task := range restored {
		if _, exists := s.tasks[task.taskID]; !exists {
			s.tasks[task.taskID] = task
		}
	}
	s.mu.Unlock()
	return nil
}

// deleteTaskListLimit bounds GET /api/delete/tasks responses.
const deleteTaskListLimit = 100

// DeleteTaskSummary is the sanitized list view of a delete task: counts only,
// no problem detail (same disclosure level as safeDeleteStatus).
type DeleteTaskSummary struct {
	TaskID    string    `json:"task_id"`
	Mode      string    `json:"mode"`
	Total     int64     `json:"total"`
	OK        int64     `json:"ok"`
	Failed    int64     `json:"failed"`
	Uncertain int64     `json:"uncertain"`
	Pending   int64     `json:"pending"`
	Complete  bool      `json:"complete"`
	CreatedAt time.Time `json:"created_at"`
}

// ListTasks returns up to limit task summaries, in-progress first, newest
// created first. It reads from delete_tasks when a store is attached and
// falls back to the in-memory map when the store is missing or fails.
func (s *DeleteService) ListTasks(ctx context.Context, limit int) []DeleteTaskSummary {
	if s == nil {
		return []DeleteTaskSummary{}
	}
	if limit <= 0 {
		limit = deleteTaskListLimit
	}
	if s.store != nil {
		summaries, err := s.listTasksFromStore(ctx, limit)
		if err == nil {
			return summaries
		}
		s.logger.Warn("list delete tasks from store, falling back to memory", "err", err)
	}
	return s.listTasksFromMemory(limit)
}

func (s *DeleteService) listTasksFromStore(
	ctx context.Context,
	limit int,
) ([]DeleteTaskSummary, error) {
	rows, err := s.store.Query(ctx, `
		SELECT id, mode, status_json, created_at
		FROM delete_tasks
		ORDER BY COALESCE(status_json->>'complete','false')::boolean,
			created_at DESC, id
		LIMIT $1;`, limit)
	if err != nil {
		return nil, fmt.Errorf("list delete tasks: query: %w", err)
	}
	defer rows.Close()

	summaries := make([]DeleteTaskSummary, 0)
	for rows.Next() {
		var (
			summary    DeleteTaskSummary
			statusJSON []byte
		)
		if err := rows.Scan(
			&summary.TaskID,
			&summary.Mode,
			&statusJSON,
			&summary.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("list delete tasks: scan: %w", err)
		}
		var status DeleteTaskStatus
		if err := json.Unmarshal(statusJSON, &status); err != nil {
			return nil, fmt.Errorf("list delete tasks: task %s status: %w", summary.TaskID, err)
		}
		summary.Total = status.Total
		summary.OK = status.OK
		summary.Failed = status.Failed
		summary.Uncertain = status.Uncertain
		summary.Pending = status.Pending
		summary.Complete = status.Complete
		summaries = append(summaries, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list delete tasks: rows: %w", err)
	}
	return summaries, nil
}

func (s *DeleteService) listTasksFromMemory(limit int) []DeleteTaskSummary {
	s.mu.Lock()
	statuses := make([]DeleteTaskStatus, 0, len(s.tasks))
	createdAt := make(map[string]time.Time, len(s.tasks))
	for taskID, task := range s.tasks {
		statuses = append(statuses, buildDeleteTaskStatus(task))
		createdAt[taskID] = task.createdAt
	}
	s.mu.Unlock()
	summaries := make([]DeleteTaskSummary, 0, len(statuses))
	for _, status := range statuses {
		summaries = append(summaries, DeleteTaskSummary{
			TaskID:    status.TaskID,
			Mode:      status.Mode,
			Total:     status.Total,
			OK:        status.OK,
			Failed:    status.Failed,
			Uncertain: status.Uncertain,
			Pending:   status.Pending,
			Complete:  status.Complete,
			CreatedAt: createdAt[status.TaskID],
		})
	}
	sort.Slice(summaries, func(left, right int) bool {
		if summaries[left].Complete != summaries[right].Complete {
			return !summaries[left].Complete
		}
		if !summaries[left].CreatedAt.Equal(summaries[right].CreatedAt) {
			return summaries[left].CreatedAt.After(summaries[right].CreatedAt)
		}
		return summaries[left].TaskID < summaries[right].TaskID
	})
	if len(summaries) > limit {
		summaries = summaries[:limit]
	}
	return summaries
}

// persistTask upserts the current status snapshot of one task into
// delete_tasks. Persistence is best-effort: failures degrade to in-memory
// only with a warning, mirroring TaskRegistry.upsertScanTask.
func (s *DeleteService) persistTask(taskID string) {
	if s == nil || s.store == nil {
		return
	}
	s.mu.Lock()
	task, ok := s.tasks[taskID]
	if !ok {
		s.mu.Unlock()
		return
	}
	payload, err := json.Marshal(buildDeleteTaskStatus(task))
	mode := task.mode
	createdAt := task.createdAt
	s.mu.Unlock()
	if err != nil {
		s.logger.Warn("marshal delete task snapshot", "task", taskID, "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = s.store.Exec(ctx, `
		INSERT INTO delete_tasks (id, mode, status_json, created_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE SET
			status_json = CASE
				WHEN COALESCE(delete_tasks.status_json->>'complete','false') = 'true'
					AND COALESCE(EXCLUDED.status_json->>'complete','false') <> 'true'
				THEN delete_tasks.status_json
				ELSE EXCLUDED.status_json
			END,
			updated_at = now();`,
		taskID,
		mode,
		payload,
		createdAt,
	)
	if err != nil {
		s.logger.Warn("upsert delete task", "task", taskID, "err", err)
	}
}

func (s *DeleteService) persistTasks(tasks []*deleteTaskState) {
	for _, task := range tasks {
		if task != nil {
			s.persistTask(task.taskID)
		}
	}
}

func (s *DeleteService) Prepare(
	ctx context.Context,
	fileIDs []int64,
) (DeleteSummary, string, error) {
	if len(fileIDs) == 0 {
		return DeleteSummary{}, "", ErrDeleteSelection
	}
	unique := make(map[int64]struct{}, len(fileIDs))
	for _, fileID := range fileIDs {
		if fileID <= 0 {
			return DeleteSummary{}, "", ErrDeleteSelection
		}
		unique[fileID] = struct{}{}
	}
	if s == nil || s.db == nil || s.confirms == nil {
		return DeleteSummary{}, "", ErrDeleteUnavailable
	}
	sortedIDs := make([]int64, 0, len(unique))
	for fileID := range unique {
		sortedIDs = append(sortedIDs, fileID)
	}
	sort.Slice(sortedIDs, func(left, right int) bool {
		return sortedIDs[left] < sortedIDs[right]
	})
	rows, err := s.db.Query(ctx, `
		WITH requested(id) AS (
			SELECT unnest($1::bigint[])
		)
		SELECT
			requested.id,
			files.machine_id,
			files.path,
			files.size,
			files.status,
			count(DISTINCT groups.id)::bigint,
			EXISTS (
				SELECT 1
				FROM dup_groups AS representative_groups
				JOIN dup_members AS requested_members
				  ON requested_members.group_id=representative_groups.id
				 AND requested_members.file_id=requested.id
				JOIN LATERAL (
					SELECT effective_files.id
					FROM dup_members AS effective_members
					JOIN files AS effective_files
					  ON effective_files.id=effective_members.file_id
					WHERE effective_members.group_id=representative_groups.id
					  AND effective_files.status <> 'deleted'
					ORDER BY
					  CASE WHEN effective_files.id=representative_groups.representative_file_id THEN 0 ELSE 1 END,
					  effective_files.machine_id,effective_files.path,effective_files.id
					LIMIT 1
				) AS effective_representative ON true
				WHERE effective_representative.id=requested.id
			)
		FROM requested
		LEFT JOIN files ON files.id=requested.id
		LEFT JOIN dup_members AS members ON members.file_id=requested.id
		LEFT JOIN dup_groups AS groups ON groups.id=members.group_id
		GROUP BY
			requested.id,
			files.machine_id,
			files.path,
			files.size,
			files.status
		ORDER BY requested.id`,
		sortedIDs,
	)
	if err != nil {
		return DeleteSummary{}, "",
			fmt.Errorf("%w: database query failed", ErrDeleteUnavailable)
	}
	defer rows.Close()
	members := make([]DeleteMember, 0, len(sortedIDs))
	seen := make(map[int64]struct{}, len(sortedIDs))
	for rows.Next() {
		var (
			fileID          int64
			machineID       *string
			path            *string
			size            *int64
			status          *string
			membershipCount int64
			representative  bool
		)
		if err := rows.Scan(
			&fileID,
			&machineID,
			&path,
			&size,
			&status,
			&membershipCount,
			&representative,
		); err != nil {
			return DeleteSummary{}, "",
				fmt.Errorf("%w: database row scan failed", ErrDeleteUnavailable)
		}
		if _, requested := unique[fileID]; !requested {
			return DeleteSummary{}, "", ErrDeleteSelection
		}
		if _, duplicate := seen[fileID]; duplicate {
			return DeleteSummary{}, "", ErrDeleteSelection
		}
		seen[fileID] = struct{}{}
		if machineID == nil || *machineID == "" || path == nil || *path == "" ||
			size == nil || *size < 0 || status == nil || *status == proto.StatusDeleted ||
			membershipCount <= 0 || representative {
			return DeleteSummary{}, "", ErrDeleteSelection
		}
		members = append(members, DeleteMember{
			FileID:    fileID,
			MachineID: *machineID,
			Path:      *path,
			Size:      *size,
		})
	}
	if err := rows.Err(); err != nil {
		return DeleteSummary{}, "",
			fmt.Errorf("%w: database row iteration failed", ErrDeleteUnavailable)
	}
	if len(seen) != len(sortedIDs) {
		return DeleteSummary{}, "", ErrDeleteSelection
	}
	token, summary, err := s.confirms.Create(members)
	if err != nil {
		return DeleteSummary{}, "", err
	}
	return summary, token, nil
}

func (s *DeleteService) Execute(
	_ context.Context,
	token string,
	mode string,
) (string, error) {
	switch mode {
	case "":
		mode = proto.ModeSoft
	case proto.ModeSoft, proto.ModeHard:
	default:
		return "", ErrDeleteMode
	}
	if s == nil || s.transport == nil || s.confirms == nil || s.now == nil ||
		s.tasks == nil {
		return "", ErrDeleteUnavailable
	}
	taskUUID, err := uuid.NewRandom()
	if err != nil {
		return "", fmt.Errorf("%w: task identity", ErrDeleteUnavailable)
	}
	taskID := taskUUID.String()
	members, err := s.confirms.ConsumeWithTask(token, taskID)
	if err != nil {
		return "", err
	}
	grouped := make(map[string][]DeleteMember)
	for _, member := range members {
		grouped[member.MachineID] = append(grouped[member.MachineID], member)
	}
	now := s.now()
	task := &deleteTaskState{
		taskID:    taskID,
		mode:      mode,
		createdAt: now,
		deadline:  now.Add(deleteReportDeadline),
		machines:  make(map[string]*deleteMachineState, len(grouped)),
	}
	for machineID, machineMembers := range grouped {
		machine := &deleteMachineState{
			expected:     make(map[string]DeleteMember, len(machineMembers)),
			results:      make(map[string]deleteStoredResult, len(machineMembers)),
			reports:      make(map[uint32]proto.DeleteReport),
			pathSequence: make(map[string]uint32, len(machineMembers)),
		}
		for _, member := range machineMembers {
			machine.expected[member.Path] = member
		}
		task.machines[machineID] = machine
	}
	s.mu.Lock()
	s.tasks[taskID] = task
	finalized := s.pruneTasksLocked(s.now())
	s.mu.Unlock()

	machineIDs := make([]string, 0, len(grouped))
	for machineID := range grouped {
		machineIDs = append(machineIDs, machineID)
	}
	sort.Strings(machineIDs)
	for _, machineID := range machineIDs {
		machineMembers := grouped[machineID]
		paths := make([]string, len(machineMembers))
		for index, member := range machineMembers {
			paths[index] = member.Path
		}
		if !s.transport.IsOnline(machineID) {
			s.mu.Lock()
			s.failAllLocked(
				task.machines[machineID],
				0,
				proto.DeleteErrHelperLost,
				"delete helper unavailable",
				false,
			)
			s.refreshTerminalLocked(task)
			s.mu.Unlock()
			continue
		}
		sendErr := s.transport.Send(machineID, proto.MsgDeleteTask, &proto.DeleteTask{
			TaskID:    taskID,
			Seq:       0,
			LastSeq:   0,
			Mode:      mode,
			Confirmed: true,
			Entries:   paths,
		})
		if sendErr != nil {
			s.mu.Lock()
			s.failAllLocked(
				task.machines[machineID],
				0,
				proto.DeleteErrHelperLost,
				"delete helper unavailable",
				false,
			)
			s.refreshTerminalLocked(task)
			s.mu.Unlock()
		}
	}
	s.persistTask(taskID)
	s.persistTasks(finalized)
	return taskID, nil
}

func (s *DeleteService) HandleReport(machineID string, report *proto.DeleteReport) {
	if s == nil || report == nil {
		return
	}
	copied := cloneDeleteReport(*report)
	s.mu.Lock()
	taskID, persist := s.applyReportLocked(machineID, copied)
	s.mu.Unlock()
	if persist {
		s.persistTask(taskID)
	}
}

// applyReportLocked applies one report to the in-memory state. It returns
// the task ID and whether the state changed in a way worth persisting.
func (s *DeleteService) applyReportLocked(
	machineID string,
	copied proto.DeleteReport,
) (string, bool) {
	task, ok := s.tasks[copied.TaskID]
	if !ok {
		return "", false
	}
	persist := s.finalizeDeadlineLocked(task)
	machine, ok := task.machines[machineID]
	if !ok || task.deadlineTerminal {
		return task.taskID, persist
	}
	if task.snapshot != nil {
		// Snapshot-backed (restored) tasks cannot validate late reports
		// against the persisted member detail, so reports are dropped.
		return task.taskID, persist
	}
	if existing, ok := machine.reports[copied.Seq]; ok {
		if reflect.DeepEqual(existing, copied) {
			return task.taskID, persist
		}
		s.failAllLocked(
			machine,
			copied.Seq,
			proto.DeleteErrDeleteFailed,
			"conflicting delete report",
			true,
		)
		s.refreshTerminalLocked(task)
		return task.taskID, true
	}
	if task.terminal || machine.terminal {
		return task.taskID, persist
	}
	if err := validateDeleteReport(machine, task.taskID, copied); err != nil {
		s.failAllLocked(
			machine,
			copied.Seq,
			proto.DeleteErrDeleteFailed,
			"invalid delete report",
			true,
		)
		s.refreshTerminalLocked(task)
		return task.taskID, true
	}
	for _, entry := range copied.Entries {
		if previousSequence, exists := machine.pathSequence[entry.Path]; exists &&
			previousSequence != copied.Seq {
			s.failAllLocked(
				machine,
				copied.Seq,
				proto.DeleteErrDeleteFailed,
				"conflicting delete report",
				true,
			)
			s.refreshTerminalLocked(task)
			return task.taskID, true
		}
	}
	if !machine.lastSeqKnown {
		machine.lastSeq = copied.LastSeq
		machine.lastSeqKnown = true
	}
	machine.reports[copied.Seq] = copied
	for _, entry := range copied.Entries {
		machine.pathSequence[entry.Path] = copied.Seq
		machine.results[entry.Path] = deleteStoredResult{
			sequence: copied.Seq,
			result:   entry,
		}
	}
	if machineHasAllSequences(machine) {
		s.failUnresolvedLocked(
			machine,
			machine.lastSeq,
			proto.DeleteErrDeleteFailed,
			"delete report omitted expected item",
			true,
		)
		if len(machine.results) == len(machine.expected) {
			machine.terminal = true
		}
	}
	s.refreshTerminalLocked(task)
	return task.taskID, true
}

func (s *DeleteService) Status(taskID string) (DeleteTaskStatus, bool) {
	if s == nil {
		return DeleteTaskStatus{}, false
	}
	s.mu.Lock()
	finalized := s.pruneTasksLocked(s.now())
	task, ok := s.tasks[taskID]
	if !ok {
		s.mu.Unlock()
		s.persistTasks(finalized)
		return DeleteTaskStatus{}, false
	}
	if s.finalizeDeadlineLocked(task) {
		finalized = append(finalized, task)
	}
	status := buildDeleteTaskStatus(task)
	s.mu.Unlock()
	s.persistTasks(finalized)
	return status, true
}

// ConsumedTaskID reports the task first accepted for a consumed confirmation
// token, letting the HTTP layer answer a repeated execute idempotently.
func (s *DeleteService) ConsumedTaskID(token string) (string, bool) {
	if s == nil || s.confirms == nil {
		return "", false
	}
	return s.confirms.ConsumedTaskID(token)
}

func validateDeleteReport(
	machine *deleteMachineState,
	taskID string,
	report proto.DeleteReport,
) error {
	if report.TaskID != taskID || report.Seq > report.LastSeq ||
		uint64(report.LastSeq)+1 > uint64(len(machine.expected)) ||
		len(report.Entries) == 0 {
		return ErrDeleteSelection
	}
	if machine.lastSeqKnown && machine.lastSeq != report.LastSeq {
		return ErrDeleteSelection
	}
	seen := make(map[string]struct{}, len(report.Entries))
	stats := proto.DeleteStats{Total: len(report.Entries)}
	for _, entry := range report.Entries {
		if entry.Path == "" {
			return ErrDeleteSelection
		}
		if _, ok := machine.expected[entry.Path]; !ok {
			return ErrDeleteSelection
		}
		if _, duplicate := seen[entry.Path]; duplicate {
			return ErrDeleteSelection
		}
		seen[entry.Path] = struct{}{}
		if entry.OK {
			if entry.Uncertain || entry.ErrCode != "" {
				return ErrDeleteSelection
			}
			stats.OK++
		} else {
			if entry.ErrCode == "" {
				return ErrDeleteSelection
			}
			stats.Failed++
			if entry.Uncertain {
				stats.Uncertain++
			}
		}
	}
	if stats != report.Stats {
		return ErrDeleteSelection
	}
	return nil
}

func cloneDeleteReport(report proto.DeleteReport) proto.DeleteReport {
	report.Entries = append([]proto.DeleteResult(nil), report.Entries...)
	return report
}

func machineHasAllSequences(machine *deleteMachineState) bool {
	if !machine.lastSeqKnown || uint64(machine.lastSeq)+1 > uint64(len(machine.expected)) {
		return false
	}
	for sequence := uint32(0); ; sequence++ {
		if _, ok := machine.reports[sequence]; !ok {
			return false
		}
		if sequence == machine.lastSeq {
			return true
		}
	}
}

func (s *DeleteService) failAllLocked(
	machine *deleteMachineState,
	sequence uint32,
	errorCode string,
	message string,
	uncertain bool,
) {
	if machine == nil {
		return
	}
	machine.results = make(map[string]deleteStoredResult, len(machine.expected))
	machine.reports = make(map[uint32]proto.DeleteReport)
	machine.pathSequence = make(map[string]uint32, len(machine.expected))
	machine.lastSeq = 0
	machine.lastSeqKnown = false
	machine.terminal = false
	s.failUnresolvedLocked(machine, sequence, errorCode, message, uncertain)
}

func (s *DeleteService) failUnresolvedLocked(
	machine *deleteMachineState,
	sequence uint32,
	errorCode string,
	message string,
	uncertain bool,
) {
	if machine == nil || machine.terminal {
		return
	}
	for path := range machine.expected {
		if _, resolved := machine.results[path]; resolved {
			continue
		}
		machine.results[path] = deleteStoredResult{
			sequence: sequence,
			result: proto.DeleteResult{
				Path:      path,
				ErrCode:   errorCode,
				Err:       message,
				Uncertain: uncertain,
			},
		}
	}
	if len(machine.results) == len(machine.expected) {
		machine.terminal = true
	}
}

// finalizeDeadlineLocked flips a task past its report deadline into a
// terminal state and reports whether anything changed.
func (s *DeleteService) finalizeDeadlineLocked(task *deleteTaskState) bool {
	if task.terminal || s.now().Before(task.deadline) {
		return false
	}
	task.deadlineTerminal = true
	if task.snapshot != nil {
		finalizeRestoredDeleteSnapshot(task.snapshot)
		s.refreshTerminalLocked(task)
		return true
	}
	for _, machine := range task.machines {
		s.failUnresolvedLocked(
			machine,
			machine.lastSeq,
			proto.DeleteErrHelperLost,
			"delete report deadline exceeded",
			true,
		)
	}
	s.refreshTerminalLocked(task)
	return true
}

// finalizeRestoredDeleteSnapshot flips the pending counts of a restored
// snapshot to failed/uncertain, matching what failUnresolvedLocked does for
// live tasks when the report deadline is exceeded.
func finalizeRestoredDeleteSnapshot(status *DeleteTaskStatus) {
	if status.Pending > 0 {
		if status.ErrorCodes == nil {
			status.ErrorCodes = make(map[string]int64)
		}
		status.ErrorCodes[proto.DeleteErrHelperLost] += status.Pending
	}
	status.Failed += status.Pending
	status.Uncertain += status.Pending
	status.Pending = 0
	status.Complete = true
	for machineID, machine := range status.ByMachine {
		machine.Failed += machine.Pending
		machine.Uncertain += machine.Pending
		machine.Pending = 0
		machine.Complete = true
		status.ByMachine[machineID] = machine
	}
}

func (s *DeleteService) refreshTerminalLocked(task *deleteTaskState) {
	task.terminal = true
	for _, machine := range task.machines {
		if !machine.terminal {
			task.terminal = false
			return
		}
	}
	if task.terminalAt.IsZero() {
		task.terminalAt = s.now()
	}
}

// pruneTasksLocked reclaims terminal tasks past the retention window. It also
// finalizes tasks whose deadline has passed so that tasks nobody polls still
// reach a terminal state and become eligible for cleanup. It returns the
// tasks it finalized during this call so callers can persist them.
func (s *DeleteService) pruneTasksLocked(now time.Time) []*deleteTaskState {
	var finalized []*deleteTaskState
	for taskID, task := range s.tasks {
		if s.finalizeDeadlineLocked(task) {
			finalized = append(finalized, task)
		}
		if task.terminal && now.Sub(task.terminalAt) >= deleteTerminalRetention {
			delete(s.tasks, taskID)
		}
	}
	return finalized
}

func cloneDeleteTaskStatus(status DeleteTaskStatus) DeleteTaskStatus {
	clone := status
	clone.ByMachine = make(map[string]DeleteMachineStatus, len(status.ByMachine))
	for machineID, machine := range status.ByMachine {
		machine.Sequences = cloneDeleteSequences(machine.Sequences)
		machine.RecycledTo = cloneDeleteRecycledTo(machine.RecycledTo)
		clone.ByMachine[machineID] = machine
	}
	clone.ErrorCodes = make(map[string]int64, len(status.ErrorCodes))
	for code, count := range status.ErrorCodes {
		clone.ErrorCodes[code] = count
	}
	clone.Problems = append([]DeleteProblemItem(nil), status.Problems...)
	return clone
}

func cloneDeleteRecycledTo(recycledTo map[string]string) map[string]string {
	if recycledTo == nil {
		return nil
	}
	clone := make(map[string]string, len(recycledTo))
	for path, destination := range recycledTo {
		clone[path] = destination
	}
	return clone
}

func buildDeleteTaskStatus(task *deleteTaskState) DeleteTaskStatus {
	if task.snapshot != nil {
		return cloneDeleteTaskStatus(*task.snapshot)
	}
	status := DeleteTaskStatus{
		TaskID:     task.taskID,
		Mode:       task.mode,
		Complete:   task.terminal,
		ByMachine:  make(map[string]DeleteMachineStatus, len(task.machines)),
		ErrorCodes: make(map[string]int64),
	}
	machineIDs := make([]string, 0, len(task.machines))
	for machineID := range task.machines {
		machineIDs = append(machineIDs, machineID)
	}
	sort.Strings(machineIDs)
	for _, machineID := range machineIDs {
		machine := task.machines[machineID]
		machineStatus := DeleteMachineStatus{
			MachineID: machineID,
			Total:     int64(len(machine.expected)),
			Pending:   int64(len(machine.expected) - len(machine.results)),
			Complete:  machine.terminal,
			Sequences: make(map[uint32]DeleteSequenceStatus, len(machine.reports)),
		}
		if machine.lastSeqKnown {
			for sequence := uint32(0); ; sequence++ {
				machineStatus.Sequences[sequence] = DeleteSequenceStatus{
					Sequence: sequence,
					LastSeq:  machine.lastSeq,
				}
				if sequence == machine.lastSeq {
					break
				}
			}
		} else {
			machineStatus.Sequences[0] = DeleteSequenceStatus{Sequence: 0}
		}
		for sequence, report := range machine.reports {
			machineStatus.Sequences[sequence] = DeleteSequenceStatus{
				Sequence:  sequence,
				LastSeq:   report.LastSeq,
				Received:  true,
				Total:     int64(report.Stats.Total),
				OK:        int64(report.Stats.OK),
				Failed:    int64(report.Stats.Failed),
				Uncertain: int64(report.Stats.Uncertain),
			}
		}
		for _, stored := range machine.results {
			result := stored.result
			if result.OK {
				machineStatus.OK++
			} else {
				machineStatus.Failed++
				if result.Uncertain {
					machineStatus.Uncertain++
				}
				status.ErrorCodes[result.ErrCode]++
			}
			if result.OK && result.RecycledTo != "" {
				if machineStatus.RecycledTo == nil {
					machineStatus.RecycledTo = make(map[string]string)
				}
				machineStatus.RecycledTo[result.Path] = result.RecycledTo
			}
			if result.StateSyncErr != "" {
				machineStatus.StateSyncFailures++
			}
			if !result.OK || result.StateSyncErr != "" {
				status.Problems = append(status.Problems, DeleteProblemItem{
					MachineID:    machineID,
					Sequence:     stored.sequence,
					Path:         result.Path,
					ErrorCode:    result.ErrCode,
					ErrorMessage: result.Err,
					Uncertain:    result.Uncertain,
					StateSyncErr: result.StateSyncErr,
				})
			}
		}
		status.Total += machineStatus.Total
		status.OK += machineStatus.OK
		status.Failed += machineStatus.Failed
		status.Uncertain += machineStatus.Uncertain
		status.Pending += machineStatus.Pending
		status.StateSyncFailures += machineStatus.StateSyncFailures
		status.ByMachine[machineID] = machineStatus
	}
	sort.Slice(status.Problems, func(left, right int) bool {
		if status.Problems[left].MachineID != status.Problems[right].MachineID {
			return status.Problems[left].MachineID < status.Problems[right].MachineID
		}
		if status.Problems[left].Sequence != status.Problems[right].Sequence {
			return status.Problems[left].Sequence < status.Problems[right].Sequence
		}
		return status.Problems[left].Path < status.Problems[right].Path
	})
	return status
}
