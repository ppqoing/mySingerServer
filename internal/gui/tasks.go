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
		SELECT id, machine_id, phase, target, status, updated_at
		FROM scan_tasks
		WHERE status IN ('sent','acked','running')
		  AND COALESCE(target->>'type','scan') = 'scan'
		ORDER BY updated_at, id;`)
	if err != nil {
		return fmt.Errorf("restore tasks: query: %w", err)
	}
	defer rows.Close()

	restored := make([]*TaskInfo, 0)
	for rows.Next() {
		var task TaskInfo
		var targetJSON []byte
		if err := rows.Scan(
			&task.TaskID,
			&task.MachineID,
			&task.Phase,
			&targetJSON,
			&task.Status,
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
			task.Status = "running"
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
			task.Status = completedTaskStatus(value.Stats)
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
		task.Status,
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
