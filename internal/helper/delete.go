package helper

import (
	"context"
	"fmt"

	"dedup/internal/proto"
	"github.com/google/uuid"
)

type Processor struct {
	cfg       Config
	validator *Validator
	ops       processorOps
}

type preparedDelete struct {
	path      ValidatedPath
	validator error
}

func NewProcessor(cfg Config, validator *Validator) *Processor {
	processor := &Processor{
		cfg:       cfg,
		validator: validator,
		ops:       defaultProcessorOps(),
	}
	processor.ops.revalidateSource = processor.revalidateSource
	return processor
}

func (p *Processor) Process(
	ctx context.Context,
	task proto.DeleteTask,
) proto.DeleteReport {
	mode, frameCode, frameErr := p.validateFrame(task)
	if frameCode != "" {
		return frameFailureReport(task, frameCode, frameErr)
	}

	prepared := make([]preparedDelete, len(task.Entries))
	for index, entry := range task.Entries {
		validated, err := p.validator.ValidateFile(entry)
		prepared[index] = preparedDelete{path: validated, validator: err}
	}

	report := proto.DeleteReport{
		TaskID:  task.TaskID,
		Seq:     task.Seq,
		LastSeq: task.LastSeq,
		Entries: make([]proto.DeleteResult, len(task.Entries)),
	}
	for index, entry := range task.Entries {
		result := proto.DeleteResult{Path: entry}
		if err := ctx.Err(); err != nil {
			result = failedDeleteResult(
				entry,
				pathError(proto.DeleteErrDeleteFailed, err),
				proto.DeleteErrDeleteFailed,
			)
		} else if prepared[index].validator != nil {
			result = failedDeleteResult(
				entry,
				prepared[index].validator,
				proto.DeleteErrBadPath,
			)
		} else if mode == proto.ModeSoft {
			destination, err := p.softDelete(
				ctx,
				prepared[index].path,
				task.TaskID,
			)
			if err != nil {
				result = failedDeleteResult(
					entry,
					err,
					proto.DeleteErrRecycleFailed,
				)
			} else {
				result.OK = true
				result.RecycledTo = destination
			}
		} else {
			readonlyCleared, err := p.hardDelete(ctx, prepared[index].path)
			if err != nil {
				result = failedDeleteResult(
					entry,
					err,
					proto.DeleteErrDeleteFailed,
				)
				result.ReadonlyCleared = readonlyCleared
			} else {
				result.OK = true
				result.ReadonlyCleared = readonlyCleared
			}
		}
		report.Entries[index] = result
		if result.OK {
			report.Stats.OK++
		} else {
			report.Stats.Failed++
		}
		if result.Uncertain {
			report.Stats.Uncertain++
		}
	}
	report.Stats.Total = len(report.Entries)
	return report
}

func (p *Processor) validateFrame(
	task proto.DeleteTask,
) (string, string, error) {
	if !task.Confirmed {
		return "", proto.DeleteErrNotConfirmed, fmt.Errorf("delete is not confirmed")
	}
	mode := task.Mode
	if mode == "" {
		mode = proto.ModeSoft
	}
	if mode != proto.ModeSoft && mode != proto.ModeHard ||
		mode == proto.ModeHard && !p.cfg.AllowHardDelete {
		return "", proto.DeleteErrBadMode, fmt.Errorf("delete mode is not allowed")
	}
	parsedID, err := uuid.Parse(task.TaskID)
	if err != nil ||
		parsedID == uuid.Nil ||
		parsedID.String() != task.TaskID ||
		parsedID.Variant() != uuid.RFC4122 ||
		parsedID.Version() < 1 ||
		parsedID.Version() > 5 {
		return "", proto.DeleteErrBadPath, fmt.Errorf("task_id is not a canonical RFC 4122 UUID")
	}
	if task.Seq > task.LastSeq {
		return "", proto.DeleteErrBadPath, fmt.Errorf("sequence exceeds last_seq")
	}
	if len(task.Entries) < 1 ||
		len(task.Entries) > p.cfg.MaxEntriesPerFrame {
		return "", proto.DeleteErrBadPath, fmt.Errorf("entry count is outside the configured frame limit")
	}
	normalized := make([]string, 0, len(task.Entries))
	for _, entry := range task.Entries {
		if entry == "" {
			return "", proto.DeleteErrBadPath, fmt.Errorf("empty delete entry")
		}
		path, err := normalizeLocalAbsolute(entry)
		if err != nil {
			continue
		}
		for _, existing := range normalized {
			if ordinalEqualFold(existing, path) {
				return "", proto.DeleteErrBadPath, fmt.Errorf("duplicate normalized delete entry")
			}
		}
		normalized = append(normalized, path)
	}
	return mode, "", nil
}

func frameFailureReport(
	task proto.DeleteTask,
	code string,
	err error,
) proto.DeleteReport {
	report := proto.DeleteReport{
		TaskID:  task.TaskID,
		Seq:     task.Seq,
		LastSeq: task.LastSeq,
		Stats: proto.DeleteStats{
			Total:  len(task.Entries),
			Failed: len(task.Entries),
		},
		Entries: make([]proto.DeleteResult, len(task.Entries)),
	}
	for index, entry := range task.Entries {
		report.Entries[index] = proto.DeleteResult{
			Path:    entry,
			ErrCode: code,
			Err:     err.Error(),
		}
	}
	return report
}

func failedDeleteResult(
	path string,
	err error,
	fallback string,
) proto.DeleteResult {
	code := fallback
	var pathErr *PathError
	if errorsAsPath(err, &pathErr) {
		code = pathErr.Code
	}
	return proto.DeleteResult{
		Path:    path,
		ErrCode: code,
		Err:     err.Error(),
	}
}

func errorsAsPath(err error, target **PathError) bool {
	for err != nil {
		if pathErr, ok := err.(*PathError); ok {
			*target = pathErr
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}
