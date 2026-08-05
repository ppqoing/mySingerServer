package gui

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

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
	used           map[string]struct{}
	expired        map[string]struct{}
	tombstoneOrder []string
}

func NewConfirmStore(ttl time.Duration, now func() time.Time) *ConfirmStore {
	return &ConfirmStore{
		ttl:     ttl,
		now:     now,
		records: make(map[string]confirmationRecord),
		used:    make(map[string]struct{}),
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
	s.used[token] = struct{}{}
	s.retainTombstoneLocked(token)
	return append([]DeleteMember(nil), record.members...), nil
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
	deadline         time.Time
	machines         map[string]*deleteMachineState
	terminal         bool
	deadlineTerminal bool
}

type DeleteService struct {
	db        groupQueryDB
	transport DeleteTransport
	confirms  *ConfirmStore
	logger    *slog.Logger

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
	members, err := s.confirms.Consume(token)
	if err != nil {
		return "", err
	}
	grouped := make(map[string][]DeleteMember)
	for _, member := range members {
		grouped[member.MachineID] = append(grouped[member.MachineID], member)
	}
	task := &deleteTaskState{
		taskID:   taskID,
		mode:     mode,
		deadline: s.now().Add(deleteReportDeadline),
		machines: make(map[string]*deleteMachineState, len(grouped)),
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
	return taskID, nil
}

func (s *DeleteService) HandleReport(machineID string, report *proto.DeleteReport) {
	if s == nil || report == nil {
		return
	}
	copied := cloneDeleteReport(*report)
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[copied.TaskID]
	if !ok {
		return
	}
	s.finalizeDeadlineLocked(task)
	machine, ok := task.machines[machineID]
	if !ok || task.deadlineTerminal {
		return
	}
	if existing, ok := machine.reports[copied.Seq]; ok {
		if reflect.DeepEqual(existing, copied) {
			return
		}
		s.failAllLocked(
			machine,
			copied.Seq,
			proto.DeleteErrDeleteFailed,
			"conflicting delete report",
			true,
		)
		s.refreshTerminalLocked(task)
		return
	}
	if task.terminal || machine.terminal {
		return
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
		return
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
			return
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
}

func (s *DeleteService) Status(taskID string) (DeleteTaskStatus, bool) {
	if s == nil {
		return DeleteTaskStatus{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[taskID]
	if !ok {
		return DeleteTaskStatus{}, false
	}
	s.finalizeDeadlineLocked(task)
	return buildDeleteTaskStatus(task), true
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

func (s *DeleteService) finalizeDeadlineLocked(task *deleteTaskState) {
	if task.terminal || s.now().Before(task.deadline) {
		return
	}
	task.deadlineTerminal = true
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
}

func (s *DeleteService) refreshTerminalLocked(task *deleteTaskState) {
	task.terminal = true
	for _, machine := range task.machines {
		if !machine.terminal {
			task.terminal = false
			return
		}
	}
}

func buildDeleteTaskStatus(task *deleteTaskState) DeleteTaskStatus {
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
