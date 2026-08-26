package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"dedup/internal/proto"
)

var ErrTaskEnvelopeConflict = errors.New("task_id already uses a different scan envelope")

var ErrTaskNotFound = errors.New("scan task not found")
var ErrTaskTerminal = errors.New("scan task is already terminal")

type TaskInfo struct {
	TaskID     string              `json:"task_id"`
	MachineID  string              `json:"machine_id"`
	Phase      int                 `json:"phase"`
	Roots      []string            `json:"roots"`
	Rescan     bool                `json:"rescan"`
	Status     string              `json:"status"`
	AckReason  string              `json:"ack_reason,omitempty"`
	Done       int64               `json:"done"`
	Total      int64               `json:"total"`
	Skipped    int64               `json:"skipped"`
	Failed     int64               `json:"failed"`
	ScanErrors int64               `json:"scan_errors"`
	ElapsedMS  int64               `json:"elapsed_ms"`
	Speed      float64             `json:"speed"`
	LastErr    string              `json:"last_err,omitempty"`
	Recent     []proto.FeatureItem `json:"recent"`
	LastSeq    uint64              `json:"last_seq,omitempty"`
	UpdatedAt  time.Time           `json:"updated_at"`

	// preCancelStatus remembers the live status a task had before entering the
	// in-memory "cancelling" state, so a failed cancel dispatch can roll back
	// and persistence can fall back to a CHECK-allowed value.
	preCancelStatus string
}

type TaskRegistry struct {
	mu   sync.Mutex
	byID map[string]*TaskInfo
	pg   *pgxpool.Pool
	log  *slog.Logger
}

func NewTaskRegistry(pool *pgxpool.Pool, logger *slog.Logger) *TaskRegistry {
	return &TaskRegistry{
		byID: make(map[string]*TaskInfo),
		pg:   pool,
		log:  logger,
	}
}

func (registry *TaskRegistry) Restore(ctx context.Context) error {
	if registry.pg == nil {
		return nil
	}
	rows, err := registry.pg.Query(ctx, `
		WITH active AS (
			SELECT id,machine_id,phase,target,status,stats_json,updated_at
			FROM scan_tasks
			WHERE status IN ('sent','acked','running')
			  AND COALESCE(target->>'type','scan') = 'scan'
		), terminal AS (
			SELECT id,machine_id,phase,target,status,stats_json,updated_at
			FROM scan_tasks
			WHERE status IN ('done','failed')
			  AND COALESCE(target->>'type','scan') = 'scan'
			ORDER BY updated_at DESC,id DESC
			LIMIT 200
		)
		SELECT * FROM active
		UNION ALL
		SELECT * FROM terminal
		ORDER BY updated_at,id;`)
	if err != nil {
		return fmt.Errorf("restore tasks: query: %w", err)
	}
	defer rows.Close()

	restored := make([]*TaskInfo, 0)
	for rows.Next() {
		var task TaskInfo
		var targetJSON, statsJSON []byte
		if err := rows.Scan(
			&task.TaskID,
			&task.MachineID,
			&task.Phase,
			&targetJSON,
			&task.Status,
			&statsJSON,
			&task.UpdatedAt,
		); err != nil {
			return fmt.Errorf("restore tasks: scan: %w", err)
		}
		var target struct {
			Roots  []string `json:"roots"`
			Rescan bool     `json:"rescan"`
		}
		if err := json.Unmarshal(targetJSON, &target); err != nil {
			return fmt.Errorf("restore task %s target: %w", task.TaskID, err)
		}
		if task.TaskID == "" || task.MachineID == "" ||
			task.Phase < 1 || task.Phase > 255 || len(target.Roots) == 0 {
			return fmt.Errorf("restore task %s: invalid scan envelope", task.TaskID)
		}
		task.Roots = append([]string(nil), target.Roots...)
		task.Rescan = target.Rescan
		if len(statsJSON) != 0 {
			var stats proto.TaskStats
			if err := json.Unmarshal(statsJSON, &stats); err != nil {
				return fmt.Errorf("restore task %s stats: %w", task.TaskID, err)
			}
			applyTaskStats(&task, stats)
		}
		restored = append(restored, &task)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("restore tasks: rows: %w", err)
	}
	registry.mu.Lock()
	for _, task := range restored {
		registry.byID[task.TaskID] = cloneTask(task)
	}
	registry.mu.Unlock()
	return nil
}

func (registry *TaskRegistry) Register(task *TaskInfo) error {
	copyTask := cloneTask(task)
	if copyTask.UpdatedAt.IsZero() {
		copyTask.UpdatedAt = time.Now()
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if existing := registry.byID[copyTask.TaskID]; existing != nil {
		if !sameTaskEnvelope(existing, copyTask) {
			return ErrTaskEnvelopeConflict
		}
		if existing.Status == "failed" {
			// A same-envelope retry after a failed dispatch: the task never
			// reached the agent, so reset the terminal state and let the new
			// dispatch and its receipts flow normally.
			existing.Status = "sent"
			existing.LastErr = ""
			existing.AckReason = ""
			existing.UpdatedAt = time.Now()
			registry.resetFailedTask(existing.TaskID)
		}
		return nil
	}
	if registry.pg != nil {
		status, err := registry.persistInitialTask(copyTask)
		if err != nil {
			return err
		}
		copyTask.Status = status
	}
	registry.byID[copyTask.TaskID] = copyTask
	return nil
}

// resetFailedTask force-clears a terminal 'failed' record for a same-envelope
// retry. The generic upsert preserves terminal states, so a dedicated UPDATE
// is required here.
func (registry *TaskRegistry) resetFailedTask(taskID string) {
	if registry.pg == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := registry.pg.Exec(ctx, `
		UPDATE scan_tasks
		SET status = 'sent', updated_at = now()
		WHERE id = $1 AND status = 'failed';`,
		taskID,
	)
	if err != nil {
		registry.log.Error("reset failed scan task", "err", err)
	}
}

func (registry *TaskRegistry) MarkSendFailed(taskID string, err error) {
	registry.mu.Lock()
	task := registry.byID[taskID]
	if task != nil {
		task.Status = "failed"
		task.LastErr = err.Error()
		task.UpdatedAt = time.Now()
	}
	copyTask := cloneTask(task)
	registry.mu.Unlock()
	if copyTask != nil {
		registry.upsertScanTask(copyTask, nil)
	}
}

// BeginCancel moves a live scan task into the in-memory "cancelling" state
// and returns a snapshot for dispatching proto.MsgScanTaskCancel. The state
// is deliberately memory-only: scan_tasks.status has a CHECK constraint
// without it, and a Manager restart simply re-dispatches the restored task —
// an agent that already cancelled answers already_done (see Dispatch).
// alreadyCancelling=true means an earlier cancel is in flight; the caller
// must not re-send the message.
func (registry *TaskRegistry) BeginCancel(
	taskID string,
) (task *TaskInfo, alreadyCancelling bool, err error) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	current := registry.byID[taskID]
	if current == nil {
		return nil, false, ErrTaskNotFound
	}
	if isTerminalTaskStatus(current.Status) {
		return nil, false, ErrTaskTerminal
	}
	if current.Status == "cancelling" {
		return cloneTask(current), true, nil
	}
	current.preCancelStatus = current.Status
	current.Status = "cancelling"
	current.UpdatedAt = time.Now()
	return cloneTask(current), false, nil
}

// RollbackCancel restores the pre-cancel status when the cancel message
// could not be delivered, so agent receipts keep flowing normally. A task
// that reached a terminal state in the meantime is left alone.
func (registry *TaskRegistry) RollbackCancel(taskID string) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	task := registry.byID[taskID]
	if task == nil || task.Status != "cancelling" {
		return
	}
	task.Status = task.preCancelStatus
	if task.Status == "" || task.Status == "cancelling" {
		task.Status = "running"
	}
	task.preCancelStatus = ""
	task.UpdatedAt = time.Now()
}

func (registry *TaskRegistry) Dispatch(machineID string, message any) {
	var persistTask *TaskInfo
	var persistStats *proto.TaskStats
	registry.mu.Lock()
	switch value := message.(type) {
	case *proto.TaskAck:
		if task := registry.byID[value.TaskID]; task != nil {
			if isTerminalTaskStatus(task.Status) {
				break
			}
			task.AckReason = value.Reason
			if value.Total >= 0 || task.Total < 0 {
				task.Total = value.Total
			}
			task.UpdatedAt = time.Now()
			if !value.Accepted {
				task.Status = "failed"
				task.LastErr = value.Reason
			} else if value.Reason == "already_done" {
				if value.Stats != nil {
					applyTaskStats(task, *value.Stats)
					task.Status = completedTaskStatus(*value.Stats)
					stats := *value.Stats
					persistStats = &stats
				} else {
					task.Status = "done"
				}
			} else if taskStatusRank(task.Status) < taskStatusRank("acked") {
				task.Status = "acked"
			}
			persistTask = cloneTask(task)
		}
	case *proto.TaskProgress:
		if task := registry.byID[value.TaskID]; task != nil {
			if isTerminalTaskStatus(task.Status) {
				break
			}
			// Progress may still arrive while a cancel unwinds the scan;
			// keep the user-visible "cancelling" until the terminal receipt.
			if task.Status != "cancelling" {
				task.Status = "running"
			}
			task.Done = value.Done
			task.Total = value.Total
			task.Speed = value.Speed
			task.UpdatedAt = time.Now()
			persistTask = cloneTask(task)
		}
	case *proto.FeatureResult:
		if task := registry.byID[value.TaskID]; task != nil {
			if value.Seq != task.LastSeq+1 {
				task.LastErr = fmt.Sprintf(
					"feature sequence gap: got %d after %d",
					value.Seq,
					task.LastSeq,
				)
			}
			task.LastSeq = value.Seq
			task.Recent = append(task.Recent, value.Items...)
			if len(task.Recent) > 50 {
				task.Recent = append(
					[]proto.FeatureItem(nil),
					task.Recent[len(task.Recent)-50:]...,
				)
			}
			task.UpdatedAt = time.Now()
		}
	case *proto.TaskDone:
		if task := registry.byID[value.TaskID]; task != nil {
			applyTaskStats(task, value.Stats)
			if value.Reason == "cancelled" || task.Status == "cancelling" {
				// 取消完成的最小展示态：status 映射为 failed（终态、可重试），
				// ack_reason 保留 "cancelled"，前端据此显示"已取消"。
				// TaskDone.Reason 使该结论自描述：Manager 重启恢复后仍能识别。
				task.AckReason = "cancelled"
				task.Status = "failed"
			} else {
				task.Status = completedTaskStatus(value.Stats)
			}
			task.UpdatedAt = time.Now()
			persistTask = cloneTask(task)
			stats := value.Stats
			persistStats = &stats
		}
	case *proto.Error:
		if value.TaskID != "" {
			if task := registry.byID[value.TaskID]; task != nil {
				task.LastErr = value.Msg
				task.UpdatedAt = time.Now()
			}
		}
		registry.log.Warn(
			"agent error",
			"machine", machineID,
			"task", value.TaskID,
			"stage", value.Stage,
			"path", value.Path,
			"msg", value.Msg,
		)
	}
	registry.mu.Unlock()
	if persistTask != nil {
		registry.upsertScanTask(persistTask, persistStats)
	}
}

func isTerminalTaskStatus(status string) bool {
	return status == "done" || status == "failed"
}

func taskStatusRank(status string) int {
	switch status {
	case "sent":
		return 0
	case "acked":
		return 1
	case "running":
		return 2
	// "cancelling" outranks acked/running so late receipts cannot regress it;
	// only a TaskDone (or rejected cancel ack) moves it to a terminal state.
	case "cancelling":
		return 3
	case "done", "failed":
		return 3
	default:
		return -1
	}
}

func sameTaskEnvelope(left, right *TaskInfo) bool {
	if left.TaskID != right.TaskID || left.MachineID != right.MachineID ||
		left.Phase != right.Phase || left.Rescan != right.Rescan ||
		len(left.Roots) != len(right.Roots) {
		return false
	}
	for index := range left.Roots {
		if left.Roots[index] != right.Roots[index] {
			return false
		}
	}
	return true
}

func applyTaskStats(task *TaskInfo, stats proto.TaskStats) {
	task.Total = stats.Total
	task.Done = stats.Done
	task.Skipped = stats.Skipped
	task.Failed = stats.Failed
	task.ScanErrors = stats.ScanErrors
	task.ElapsedMS = stats.ElapsedMS
}

func completedTaskStatus(stats proto.TaskStats) string {
	if stats.ScanErrors > 0 {
		return "failed"
	}
	return "done"
}

func (registry *TaskRegistry) List() []*TaskInfo {
	registry.mu.Lock()
	out := make([]*TaskInfo, 0, len(registry.byID))
	for _, task := range registry.byID {
		out = append(out, cloneTask(task))
	}
	registry.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out
}

func (registry *TaskRegistry) PendingScans(machineID string) []proto.ScanTask {
	registry.mu.Lock()
	out := make([]proto.ScanTask, 0)
	for _, task := range registry.byID {
		if task.MachineID != machineID ||
			(task.Status != "sent" &&
				task.Status != "acked" &&
				task.Status != "running") {
			continue
		}
		out = append(out, proto.ScanTask{
			TaskID: task.TaskID,
			Roots:  append([]string(nil), task.Roots...),
			Phase:  uint8(task.Phase),
			Options: proto.ScanOptions{
				Rescan: task.Rescan,
			},
		})
	}
	registry.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		return out[i].TaskID < out[j].TaskID
	})
	return out
}

func cloneTask(task *TaskInfo) *TaskInfo {
	if task == nil {
		return nil
	}
	copyTask := *task
	copyTask.Roots = append([]string(nil), task.Roots...)
	copyTask.Recent = append([]proto.FeatureItem(nil), task.Recent...)
	return &copyTask
}

func (registry *TaskRegistry) upsertScanTask(
	task *TaskInfo,
	stats *proto.TaskStats,
) {
	if registry.pg == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var statsJSON []byte
	if stats != nil {
		statsJSON, _ = json.Marshal(stats)
	}
	// "cancelling" 是内存中间态，scan_tasks.status 的 CHECK 约束不含它；
	// 落库回退为取消前状态。恢复后任务按运行中重派——若 agent 已取消，
	// 会以 already_done 或 TaskDone.Reason="cancelled" 收口（见 Dispatch）。
	persistStatus := task.Status
	if persistStatus == "cancelling" {
		persistStatus = task.preCancelStatus
		if persistStatus == "" || persistStatus == "cancelling" {
			persistStatus = "running"
		}
	}
	_, err := registry.pg.Exec(ctx, `
		INSERT INTO scan_tasks (
		    id, machine_id, phase, target, status, stats_json
		)
		VALUES (
		    $1, $2, $3,
		    jsonb_build_object(
		        'roots', to_jsonb($4::text[]),
		        'rescan', $5::boolean
		    ),
		    $6, $7
		)
		ON CONFLICT (id) DO UPDATE SET
		    status = CASE
		        WHEN scan_tasks.status IN ('done','failed')
		        THEN scan_tasks.status
		        ELSE EXCLUDED.status
		    END,
		    stats_json = COALESCE(EXCLUDED.stats_json, scan_tasks.stats_json),
		    updated_at = now();`,
		task.TaskID,
		task.MachineID,
		task.Phase,
		task.Roots,
		task.Rescan,
		persistStatus,
		nullableJSON(statsJSON),
	)
	if err != nil {
		registry.log.Error("upsert scan_tasks", "err", err)
	}
}

func (registry *TaskRegistry) persistInitialTask(task *TaskInfo) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var status string
	err := registry.pg.QueryRow(ctx, `
		INSERT INTO scan_tasks (
		    id, machine_id, phase, target, status
		)
		VALUES (
		    $1, $2, $3,
		    jsonb_build_object(
		        'roots', to_jsonb($4::text[]),
		        'rescan', $5::boolean
		    ),
		    $6
		)
		ON CONFLICT (id) DO UPDATE SET
		    updated_at = scan_tasks.updated_at
		WHERE scan_tasks.machine_id = EXCLUDED.machine_id
		  AND scan_tasks.phase = EXCLUDED.phase
		  AND scan_tasks.target = EXCLUDED.target
		RETURNING status;`,
		task.TaskID,
		task.MachineID,
		task.Phase,
		task.Roots,
		task.Rescan,
		task.Status,
	).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrTaskEnvelopeConflict
	}
	if err != nil {
		return "", fmt.Errorf("persist initial scan task: %w", err)
	}
	return status, nil
}

func nullableJSON(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}
