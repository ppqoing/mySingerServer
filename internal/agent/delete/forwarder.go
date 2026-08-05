package agentdelete

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"reflect"
	"sync"
	"time"

	"dedup/internal/agent"
	"dedup/internal/config"
	"dedup/internal/proto"
)

const helperRole = "delete-helper"

const helperLostMessage = "delete helper unavailable; start helper.exe as administrator"

type StateStore interface {
	MarkDeleted(context.Context, string, []string) error
}

type HelperDialer interface {
	Dial(context.Context) (net.Conn, error)
}

type Forwarder struct {
	machineID string
	cfg       config.DeleteConfig
	dialer    HelperDialer
	state     StateStore
	audit     *slog.Logger
	log       *slog.Logger

	dialTimeout   time.Duration
	helloTimeout  time.Duration
	reportTimeout time.Duration
}

func NewForwarder(
	machineID string,
	cfg config.DeleteConfig,
	dialer HelperDialer,
	state StateStore,
	audit *slog.Logger,
	log *slog.Logger,
) *Forwarder {
	if audit == nil {
		audit = discardLogger()
	}
	if log == nil {
		log = discardLogger()
	}
	return &Forwarder{
		machineID:     machineID,
		cfg:           cfg,
		dialer:        dialer,
		state:         state,
		audit:         audit,
		log:           log,
		dialTimeout:   time.Duration(cfg.DialTimeoutMS) * time.Millisecond,
		helloTimeout:  time.Duration(cfg.HelloTimeoutS) * time.Second,
		reportTimeout: time.Duration(cfg.ReportTimeoutS) * time.Second,
	}
}

func (f *Forwarder) Handle(
	ctx context.Context,
	task proto.DeleteTask,
	sender agent.Sender,
) error {
	if f == nil {
		return errors.New("delete forwarder: nil forwarder")
	}
	task.Entries = append([]string(nil), task.Entries...)
	if err := f.validate(ctx, task, sender); err != nil {
		return err
	}
	chunks := splitTask(task, f.cfg.MaxEntriesPerFrame)

	dialCtx, cancelDial := context.WithTimeout(ctx, f.dialTimeout)
	connection, dialErr := f.dialer.Dial(dialCtx)
	cancelDial()
	connectionIsNil := isNilValue(connection)
	if dialErr != nil || connectionIsNil {
		if !connectionIsNil {
			_ = connection.Close()
		}
		if dialErr == nil {
			dialErr = errors.New("dial returned a nil connection")
		}
		return f.deliverFailureReports(
			ctx,
			chunks,
			0,
			false,
			sender,
			fmt.Errorf("delete forwarder: helper dial: %w", dialErr),
		)
	}

	var closeOnce sync.Once
	closeConnection := func() {
		closeOnce.Do(func() {
			_ = connection.Close()
		})
	}
	stopCancellationWatch := make(chan struct{})
	cancellationWatchDone := make(chan struct{})
	go func() {
		defer close(cancellationWatchDone)
		select {
		case <-ctx.Done():
			closeConnection()
		case <-stopCancellationWatch:
		}
	}()
	defer func() {
		close(stopCancellationWatch)
		closeConnection()
		<-cancellationWatchDone
	}()

	framed := proto.NewConn(connection)
	if err := connection.SetReadDeadline(
		time.Now().Add(f.helloTimeout),
	); err != nil {
		return f.deliverFailureReports(
			ctx,
			chunks,
			0,
			false,
			sender,
			fmt.Errorf("delete forwarder: set Hello deadline: %w", err),
		)
	}
	if err := readAndValidateHello(framed); err != nil {
		return f.deliverFailureReports(
			ctx,
			chunks,
			0,
			false,
			sender,
			fmt.Errorf("delete forwarder: Helper Hello: %w", err),
		)
	}

	var accumulated error
	for index := range chunks {
		chunk := chunks[index]
		framed.SetWriteTimeout(proto.DefaultWriteTimeout)
		if err := framed.WriteFrame(proto.MsgDeleteTask, &chunk); err != nil {
			return f.deliverFailureReportsWithPrior(
				ctx,
				chunks,
				index,
				true,
				sender,
				accumulated,
				fmt.Errorf(
					"delete forwarder: write chunk seq=%d: %w",
					chunk.Seq,
					err,
				),
			)
		}
		if err := connection.SetReadDeadline(
			time.Now().Add(f.reportTimeout),
		); err != nil {
			return f.deliverFailureReportsWithPrior(
				ctx,
				chunks,
				index,
				true,
				sender,
				accumulated,
				fmt.Errorf(
					"delete forwarder: set report deadline seq=%d: %w",
					chunk.Seq,
					err,
				),
			)
		}
		report, err := readAndValidateReport(framed, chunk)
		if err != nil {
			return f.deliverFailureReportsWithPrior(
				ctx,
				chunks,
				index,
				true,
				sender,
				accumulated,
				fmt.Errorf(
					"delete forwarder: read report seq=%d: %w",
					chunk.Seq,
					err,
				),
			)
		}

		stateErr, sendErr := f.deliverReport(
			ctx,
			&report,
			chunk.Mode,
			sender,
		)
		accumulated = errors.Join(accumulated, stateErr)
		if sendErr != nil {
			return errors.Join(
				accumulated,
				fmt.Errorf(
					"delete forwarder: send report seq=%d: %w",
					chunk.Seq,
					sendErr,
				),
			)
		}
	}
	return accumulated
}

func (f *Forwarder) validate(
	ctx context.Context,
	task proto.DeleteTask,
	sender agent.Sender,
) error {
	switch {
	case isNilValue(ctx):
		return errors.New("delete forwarder: nil context")
	case f.machineID == "":
		return errors.New("delete forwarder: empty machine ID")
	case isNilValue(f.dialer):
		return errors.New("delete forwarder: nil Helper dialer")
	case isNilValue(f.state):
		return errors.New("delete forwarder: nil state store")
	case sender == nil:
		return errors.New("delete forwarder: nil GUI sender")
	case f.cfg.MaxEntriesPerFrame < 1 ||
		f.cfg.MaxEntriesPerFrame > 2000:
		return fmt.Errorf(
			"delete forwarder: max_entries_per_frame %d outside 1..2000",
			f.cfg.MaxEntriesPerFrame,
		)
	case f.cfg.DialTimeoutMS <= 0:
		return errors.New("delete forwarder: dial timeout must be positive")
	case f.cfg.HelloTimeoutS <= 0:
		return errors.New("delete forwarder: Hello timeout must be positive")
	case f.cfg.ReportTimeoutS <= 0:
		return errors.New("delete forwarder: report timeout must be positive")
	case task.Seq != 0 || task.LastSeq != 0:
		return errors.New("delete forwarder: GUI task must be unsplit")
	case len(task.Entries) == 0:
		return errors.New("delete forwarder: empty delete task")
	}
	seen := make(map[string]struct{}, len(task.Entries))
	for _, path := range task.Entries {
		if path == "" {
			return errors.New("delete forwarder: empty path")
		}
		if _, exists := seen[path]; exists {
			return fmt.Errorf("delete forwarder: duplicate path %q", path)
		}
		seen[path] = struct{}{}
	}
	return nil
}

func isNilValue(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan,
		reflect.Func,
		reflect.Interface,
		reflect.Map,
		reflect.Pointer,
		reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func splitTask(task proto.DeleteTask, maximum int) []proto.DeleteTask {
	chunkCount := (len(task.Entries) + maximum - 1) / maximum
	lastSequence := uint32(chunkCount - 1)
	chunks := make([]proto.DeleteTask, 0, chunkCount)
	for start := 0; start < len(task.Entries); start += maximum {
		end := start + maximum
		if end > len(task.Entries) {
			end = len(task.Entries)
		}
		chunks = append(chunks, proto.DeleteTask{
			TaskID:    task.TaskID,
			Seq:       uint32(len(chunks)),
			LastSeq:   lastSequence,
			Mode:      task.Mode,
			Confirmed: task.Confirmed,
			Entries:   task.Entries[start:end],
		})
	}
	return chunks
}

func readAndValidateHello(framed *proto.Conn) error {
	messageType, body, err := framed.ReadFrame()
	if err != nil {
		return err
	}
	if messageType != proto.MsgHello {
		return fmt.Errorf(
			"message type %d, want %d",
			messageType,
			proto.MsgHello,
		)
	}
	decoded, err := proto.Decode(messageType, body)
	if err != nil {
		return err
	}
	hello, ok := decoded.(*proto.Hello)
	if !ok {
		return fmt.Errorf("decoded Hello type %T", decoded)
	}
	if hello.Role != helperRole {
		return fmt.Errorf("role %q is not %q", hello.Role, helperRole)
	}
	if hello.Version != proto.ProtocolVersion {
		return fmt.Errorf(
			"version %d, want %d",
			hello.Version,
			proto.ProtocolVersion,
		)
	}
	if hello.PID <= 0 {
		return fmt.Errorf("PID %d is not positive", hello.PID)
	}
	return nil
}

func readAndValidateReport(
	framed *proto.Conn,
	chunk proto.DeleteTask,
) (proto.DeleteReport, error) {
	messageType, body, err := framed.ReadFrame()
	if err != nil {
		return proto.DeleteReport{}, err
	}
	if messageType != proto.MsgDeleteReport {
		return proto.DeleteReport{}, fmt.Errorf(
			"message type %d, want %d",
			messageType,
			proto.MsgDeleteReport,
		)
	}
	decoded, err := proto.Decode(messageType, body)
	if err != nil {
		return proto.DeleteReport{}, err
	}
	report, ok := decoded.(*proto.DeleteReport)
	if !ok {
		return proto.DeleteReport{}, fmt.Errorf(
			"decoded report type %T",
			decoded,
		)
	}
	if report.TaskID != chunk.TaskID ||
		report.Seq != chunk.Seq ||
		report.LastSeq != chunk.LastSeq {
		return proto.DeleteReport{}, fmt.Errorf(
			"metadata mismatch task=%q seq=%d last_seq=%d",
			report.TaskID,
			report.Seq,
			report.LastSeq,
		)
	}

	requested := make(map[string]struct{}, len(chunk.Entries))
	for _, path := range chunk.Entries {
		requested[path] = struct{}{}
	}
	returned := make(map[string]proto.DeleteResult, len(report.Entries))
	for _, result := range report.Entries {
		result.StateSyncErr = ""
		if _, exists := requested[result.Path]; !exists {
			return proto.DeleteReport{}, fmt.Errorf(
				"report contains foreign path %q",
				result.Path,
			)
		}
		if _, exists := returned[result.Path]; exists {
			return proto.DeleteReport{}, fmt.Errorf(
				"report contains duplicate path %q",
				result.Path,
			)
		}
		returned[result.Path] = result
	}

	canonical := proto.DeleteReport{
		TaskID:  chunk.TaskID,
		Seq:     chunk.Seq,
		LastSeq: chunk.LastSeq,
		Entries: make([]proto.DeleteResult, 0, len(chunk.Entries)),
	}
	for _, path := range chunk.Entries {
		if result, exists := returned[path]; exists {
			canonical.Entries = append(canonical.Entries, result)
			continue
		}
		canonical.Entries = append(
			canonical.Entries,
			helperLostResult(path, true),
		)
	}
	canonical.Stats = calculateStats(canonical.Entries)
	return canonical, nil
}

func (f *Forwarder) deliverFailureReports(
	ctx context.Context,
	chunks []proto.DeleteTask,
	start int,
	currentUncertain bool,
	sender agent.Sender,
	cause error,
) error {
	return f.deliverFailureReportsWithPrior(
		ctx,
		chunks,
		start,
		currentUncertain,
		sender,
		nil,
		cause,
	)
}

func (f *Forwarder) deliverFailureReportsWithPrior(
	ctx context.Context,
	chunks []proto.DeleteTask,
	start int,
	currentUncertain bool,
	sender agent.Sender,
	prior error,
	cause error,
) error {
	accumulated := errors.Join(prior, cause)
	for index := start; index < len(chunks); index++ {
		uncertain := currentUncertain && index == start
		report := syntheticReport(chunks[index], uncertain)
		stateErr, sendErr := f.deliverReport(
			ctx,
			&report,
			chunks[index].Mode,
			sender,
		)
		accumulated = errors.Join(accumulated, stateErr)
		if sendErr != nil {
			return errors.Join(
				accumulated,
				fmt.Errorf(
					"delete forwarder: send synthetic report seq=%d: %w",
					report.Seq,
					sendErr,
				),
			)
		}
	}
	return accumulated
}

func (f *Forwarder) deliverReport(
	ctx context.Context,
	report *proto.DeleteReport,
	mode string,
	sender agent.Sender,
) (stateErr error, sendErr error) {
	if mode == "" {
		mode = proto.ModeSoft
	}
	for _, result := range report.Entries {
		f.audit.Info(
			"delete_physical_result",
			"task_id", report.TaskID,
			"machine_id", f.machineID,
			"seq", report.Seq,
			"path", result.Path,
			"mode", mode,
			"ok", result.OK,
			"err_code", result.ErrCode,
			"err", result.Err,
			"readonly_cleared", result.ReadonlyCleared,
			"recycled_to", result.RecycledTo,
			"uncertain", result.Uncertain,
		)
	}

	successful := make([]string, 0, len(report.Entries))
	for _, result := range report.Entries {
		if result.OK {
			successful = append(successful, result.Path)
		}
	}
	if len(successful) != 0 {
		if err := f.state.MarkDeleted(ctx, f.machineID, successful); err != nil {
			for index := range report.Entries {
				if report.Entries[index].OK {
					report.Entries[index].StateSyncErr = err.Error()
				}
			}
			f.audit.Error(
				"delete_state_sync_error",
				"task_id", report.TaskID,
				"machine_id", f.machineID,
				"seq", report.Seq,
				"err", err.Error(),
				"success_count", len(successful),
			)
			stateErr = fmt.Errorf(
				"delete forwarder: state sync seq=%d: %w",
				report.Seq,
				err,
			)
		}
	}
	sendErr = sender(proto.MsgDeleteReport, report)
	return stateErr, sendErr
}

func syntheticReport(
	chunk proto.DeleteTask,
	uncertain bool,
) proto.DeleteReport {
	results := make([]proto.DeleteResult, len(chunk.Entries))
	for index, path := range chunk.Entries {
		results[index] = helperLostResult(path, uncertain)
	}
	return proto.DeleteReport{
		TaskID:  chunk.TaskID,
		Seq:     chunk.Seq,
		LastSeq: chunk.LastSeq,
		Stats:   calculateStats(results),
		Entries: results,
	}
}

func helperLostResult(path string, uncertain bool) proto.DeleteResult {
	return proto.DeleteResult{
		Path:      path,
		ErrCode:   proto.DeleteErrHelperLost,
		Err:       helperLostMessage,
		Uncertain: uncertain,
	}
}

func calculateStats(entries []proto.DeleteResult) proto.DeleteStats {
	stats := proto.DeleteStats{Total: len(entries)}
	for _, entry := range entries {
		if entry.OK {
			stats.OK++
		} else {
			stats.Failed++
		}
		if entry.Uncertain {
			stats.Uncertain++
		}
	}
	return stats
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
