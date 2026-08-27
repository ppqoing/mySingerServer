package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"dedup/internal/nodetray/agentclient"
	"dedup/internal/nodetray/config"
	"dedup/internal/nodetray/traymodel"
	"dedup/internal/nodetray/windows/elevation"
	nodetask "dedup/internal/nodetray/windows/task"
	"dedup/internal/proto"
)

type Store interface {
	LoadTraySettings() (traymodel.TraySettings, error)
	SaveTraySettings(traymodel.TraySettings) error
	LoadHelperForm() (config.HelperForm, error)
	ValidateHelperForm(config.HelperForm) []config.FieldError
	SaveHelperForm(config.HelperForm) (string, error)
	PrepareHelperWrite(config.HelperForm) (config.PreparedWrite, error)
	PrepareDefaultHelperWrite() (config.PreparedWrite, error)
	HelperFingerprint() (string, error)
}

type AgentConfigSaveResult struct {
	SHA256          string
	RestartRequired bool
}

// AgentConfigGateway owns local Agent configuration I/O and stages a changed
// listen endpoint for the separate runtime control connection.
type AgentConfigGateway interface {
	LoadAgentForm(context.Context) (config.AgentForm, error)
	ValidateAgentForm(context.Context, config.AgentForm) []config.FieldError
	SaveAgentForm(context.Context, config.AgentForm) (AgentConfigSaveResult, error)
	PromotePendingEndpoint()
}

type LocalAgentGateway interface {
	CallLocal(context.Context, string, any, any) error
}

// Validator is the local pure Helper form-validation boundary. Agent
// validation belongs to AgentConfigGateway.
type Validator interface {
	ValidateHelper(config.HelperForm) []config.FieldError
}

type Component interface {
	Start(context.Context) traymodel.OperationResult
	Stop(context.Context) traymodel.OperationResult
	Restart(context.Context) traymodel.OperationResult
	ForceStopTracked(context.Context) traymodel.OperationResult
	Refresh(context.Context) traymodel.ComponentState
}

type FingerprintUpdater interface {
	UpdateExpectedSHA256(string) traymodel.OperationResult
}

type TaskController interface {
	Inspect(context.Context) (nodetask.Status, error)
	Run(context.Context) error
}

type ElevationClient interface {
	Invoke(context.Context, elevation.Action, []byte) (elevation.InvocationResult, error)
}

type LoginStart interface {
	Enabled() (bool, string, error)
	Enable(string) error
	Disable() error
}

type PathResolver interface{ Final(string) (string, error) }
type LocationOpener interface {
	Open(context.Context, string) error
}
type WorkerProvider interface {
	Snapshot(context.Context) ([]traymodel.WorkerState, error)
}

type ProcessWaiter interface {
	WaitPIDGone(context.Context, int) error
}

type Location struct {
	Path string
	Root string
}

type Dependencies struct {
	Store             Store
	Validator         Validator
	AgentConfig       AgentConfigGateway
	LocalAgent        LocalAgentGateway
	MachineID         string
	Agent             Component
	Helper            Component
	AgentFingerprint  FingerprintUpdater
	HelperFingerprint FingerprintUpdater
	Task              TaskController
	Elevation         ElevationClient
	LoginStart        LoginStart
	TrayExecutable    string
	TaskDefinition    nodetask.Definition
	Locations         map[traymodel.LocationKind]Location
	PathResolver      PathResolver
	Opener            LocationOpener
	Workers           WorkerProvider
	ProcessWaiter     ProcessWaiter
}

type Service struct {
	agentConfigMu       sync.Mutex
	store               Store
	validator           Validator
	agentConfig         AgentConfigGateway
	localAgent          LocalAgentGateway
	localRequestTimeout time.Duration
	deleteMu            sync.Mutex
	deleteTokens        map[string]localDeleteToken
	machineID           string
	agent               Component
	helper              Component
	agentFingerprint    FingerprintUpdater
	helperFingerprint   FingerprintUpdater
	task                TaskController
	elevation           ElevationClient
	loginStart          LoginStart
	trayExecutable      string
	locations           map[traymodel.LocationKind]Location
	pathResolver        PathResolver
	opener              LocationOpener
	workers             WorkerProvider
	processWaiter       ProcessWaiter
}

func NewService(dependencies Dependencies) *Service {
	locations := make(map[traymodel.LocationKind]Location, len(dependencies.Locations))
	for kind, location := range dependencies.Locations {
		locations[kind] = location
	}
	return &Service{
		store: dependencies.Store, validator: dependencies.Validator, agentConfig: dependencies.AgentConfig,
		localAgent: dependencies.LocalAgent, localRequestTimeout: localRequestTimeout, deleteTokens: make(map[string]localDeleteToken),
		machineID: dependencies.MachineID,
		agent:     dependencies.Agent, helper: dependencies.Helper, task: dependencies.Task,
		agentFingerprint: dependencies.AgentFingerprint, helperFingerprint: dependencies.HelperFingerprint,
		elevation: dependencies.Elevation, loginStart: dependencies.LoginStart,
		trayExecutable: dependencies.TrayExecutable,
		locations: locations, pathResolver: dependencies.PathResolver,
		opener: dependencies.Opener, workers: dependencies.Workers, processWaiter: dependencies.ProcessWaiter,
	}
}

type localDeleteToken struct {
	digest    string
	token     string
	expiresAt int64
}

const localRequestTimeout = 10 * time.Second

func (s *Service) localCall(ctx context.Context, operation string, request, response any) error {
	if s == nil || s.localAgent == nil || ctx == nil {
		return errors.New("agent_disconnected")
	}
	timeout := s.localRequestTimeout
	if timeout <= 0 {
		timeout = localRequestTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return s.localAgent.CallLocal(callCtx, operation, request, response)
}

func localError(err error) (string, string) {
	if code, summary, ok := mappedLocalRemoteError(err); ok {
		return code, summary
	}
	return "local_operation_failed", "本机 Agent 暂不可用，请稍后重试"
}

func localTaskControlError(err error) (string, string) {
	if code, summary, ok := mappedLocalRemoteError(err); ok {
		return code, summary
	}
	return "local_operation_failed", "本机 Agent 暂不可用，请稍后重试"
}

func mappedLocalRemoteError(err error) (string, string, bool) {
	var remote *agentclient.RemoteError
	if errors.As(err, &remote) {
		switch remote.Code {
		case "stale_task":
			return remote.Code, "任务状态已更新，请刷新后重试", true
		case "task_instance_mismatch":
			return remote.Code, "任务实例已更新，请刷新后重试", true
		case "invalid_task_state":
			return remote.Code, "当前任务状态不支持此操作", true
		case "task_not_found":
			return remote.Code, "任务不存在或已删除", true
		case "local_task_unavailable":
			return remote.Code, "本机任务服务暂不可用", true
		case "task_delete_failed":
			return remote.Code, "删除任务失败，请稍后重试", true
		case "task_control_failed":
			return remote.Code, "任务操作失败，请稍后重试", true
		case "task_instance_required":
			return remote.Code, "任务实例信息缺失，请刷新后重试", true
		}
	}
	return "", "", false
}

func mapLocalTask(value proto.LocalTask) traymodel.LocalTask {
	result := traymodel.LocalTask{TaskID: value.TaskID, InstanceID: value.InstanceID, Revision: value.Revision, Source: value.Source, Mode: value.Mode,
		Stage: value.Stage, Status: value.Status, Phase: value.Phase, Roots: append([]string(nil), value.Roots...),
		ProgressComplete: value.ProgressComplete, ProgressTotal: value.ProgressTotal, ProgressTotalKnown: value.ProgressTotalKnown,
		ErrorCode: value.SafeErrorCode, ErrorSummary: sanitizeText(value.SafeErrorMessage), CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
		StartedAt: value.StartedAt, CompletedAt: value.CompletedAt, SyncStatus: "本机已保存"}
	var stats proto.LocalTaskDisplayStats
	if json.Unmarshal([]byte(value.StatsJSON), &stats) == nil && (stats.SchemaVersion == 1 || stats.SchemaVersion == proto.LocalTaskDisplayStatsVersion) {
		result.Speed = formatLocalTaskSpeed(stats.Speed)
		result.Failures = max(stats.Failures, 0)
		result.Duration = formatLocalTaskDuration(stats.DurationMS)
		result.IO = mapLocalTaskIO(stats.IO)
		if stats.SyncStatus != "" {
			result.SyncStatus = sanitizeText(stats.SyncStatus)
		}
	}
	return result
}

func mapLocalTaskIO(value proto.TaskIOStats) traymodel.LocalTaskIOStats {
	effectiveReadBPS := value.EffectiveReadBPS
	if effectiveReadBPS < 0 || math.IsNaN(effectiveReadBPS) || math.IsInf(effectiveReadBPS, 0) {
		effectiveReadBPS = 0
	}
	return traymodel.LocalTaskIOStats{
		DiskConcurrency: max(value.DiskConcurrency, 0), EffectiveReadBPS: effectiveReadBPS,
		LeaseWaitMS: max(value.LeaseWaitMS, 0), SequentialBytes: max(value.SequentialBytes, 0),
		SeekCount: max(value.SeekCount, 0), BusyWorkers: max(value.BusyWorkers, 0), IOWaitWorkers: max(value.IOWaitWorkers, 0),
	}
}

func formatLocalTaskSpeed(speed float64) string {
	if speed <= 0 || math.IsNaN(speed) || math.IsInf(speed, 0) {
		return ""
	}
	value := strings.TrimSuffix(strconv.FormatFloat(speed, 'f', 1, 64), ".0")
	return value + " 文件/秒"
}

func formatLocalTaskDuration(durationMS int64) string {
	seconds := max(durationMS, 0) / int64(time.Second/time.Millisecond)
	hours := seconds / 3600
	minutes := seconds % 3600 / 60
	return fmt.Sprintf("%02d:%02d:%02d", hours, minutes, seconds%60)
}

func (s *Service) CreateLocalTask(ctx context.Context, request traymodel.LocalTaskCreate) traymodel.LocalTaskResult {
	var response proto.LocalTaskCreateResponse
	err := s.localCall(ctx, proto.LocalOperationTaskCreate, proto.LocalTaskCreateRequest{TaskID: request.TaskID, Roots: request.Roots, Mode: request.Mode, Rescan: request.Rescan, Extensions: request.Extensions}, &response)
	if err != nil {
		code, summary := localError(err)
		return traymodel.LocalTaskResult{ErrorCode: code, ErrorSummary: summary}
	}
	return traymodel.LocalTaskResult{OK: true, Task: mapLocalTask(response.Task)}
}

func (s *Service) PauseLocalTask(ctx context.Context, request traymodel.LocalTaskControl) traymodel.LocalTaskResult {
	return s.controlLocalTask(ctx, proto.LocalOperationTaskPause, request)
}

func (s *Service) ResumeLocalTask(ctx context.Context, request traymodel.LocalTaskControl) traymodel.LocalTaskResult {
	return s.controlLocalTask(ctx, proto.LocalOperationTaskResume, request)
}

func (s *Service) CancelLocalTask(ctx context.Context, request traymodel.LocalTaskControl) traymodel.LocalTaskResult {
	return s.controlLocalTask(ctx, proto.LocalOperationTaskCancel, request)
}

func (s *Service) DeleteLocalTask(ctx context.Context, request traymodel.LocalTaskControl) traymodel.LocalTaskResult {
	return s.controlLocalTask(ctx, proto.LocalOperationTaskDelete, request)
}

func (s *Service) RetryLocalTask(ctx context.Context, request traymodel.LocalTaskControl) traymodel.LocalTaskResult {
	return s.controlLocalTask(ctx, proto.LocalOperationTaskRetry, request)
}

func (s *Service) controlLocalTask(ctx context.Context, operation string, request traymodel.LocalTaskControl) traymodel.LocalTaskResult {
	control := proto.LocalTaskControlRequest{
		TaskID: request.TaskID, InstanceID: request.InstanceID, ExpectedRevision: request.ExpectedRevision,
	}
	if err := control.Validate(); err != nil {
		return traymodel.LocalTaskResult{ErrorCode: proto.InvalidTaskControlErrorCode, ErrorSummary: "任务控制请求无效"}
	}
	var response proto.LocalTaskControlResponse
	if err := s.localCall(ctx, operation, control, &response); err != nil {
		code, summary := localTaskControlError(err)
		return traymodel.LocalTaskResult{ErrorCode: code, ErrorSummary: summary}
	}
	result := traymodel.LocalTaskResult{OK: true, Deleted: response.Deleted}
	if response.Task != nil {
		result.Task = mapLocalTask(*response.Task)
	}
	return result
}

func (s *Service) StartLocalAnalysis(ctx context.Context, request traymodel.LocalAnalysisStart) traymodel.OperationResult {
	var response proto.LocalTaskCreateResponse
	err := s.localCall(ctx, proto.LocalOperationAnalysisStart, proto.LocalTaskCreateRequest{TaskID: request.TaskID, Roots: request.Roots, Mode: proto.LocalTaskModeScanThenAnalysis, Rescan: request.Rescan, Extensions: request.Extensions}, &response)
	if err != nil {
		code, summary := localError(err)
		return traymodel.OperationResult{ErrorCode: code, ErrorSummary: summary}
	}
	return traymodel.OperationResult{OK: true}
}

func (s *Service) ListLocalTasks(ctx context.Context, request traymodel.PageRequest) traymodel.LocalTaskPage {
	var response proto.LocalTaskListResponse
	err := s.localCall(ctx, proto.LocalOperationTaskList, proto.LocalTaskListRequest{Offset: request.Offset, Limit: request.Limit}, &response)
	if err != nil {
		code, summary := localError(err)
		return traymodel.LocalTaskPage{Tasks: []traymodel.LocalTask{}, ErrorCode: code, ErrorSummary: summary}
	}
	tasks := make([]traymodel.LocalTask, len(response.Tasks))
	for i := range response.Tasks {
		tasks[i] = mapLocalTask(response.Tasks[i])
	}
	return traymodel.LocalTaskPage{OK: true, Tasks: tasks, Offset: response.Offset, NextOffset: response.NextOffset}
}

func mapLocalGroup(value proto.LocalGroup) traymodel.LocalGroup {
	members := make([]traymodel.LocalGroupMember, len(value.Members))
	for i, member := range value.Members {
		members[i] = traymodel.LocalGroupMember{FileID: member.FileID, Path: member.Path, FileName: member.FileName, Size: member.Size, Status: member.Status, Decision: member.Decision}
	}
	return traymodel.LocalGroup{RunID: value.RunID, Generation: value.Generation, GroupID: value.GroupID, Category: value.Category, Verdict: value.Verdict, ReviewStatus: value.ReviewStatus, Members: members}
}

func (s *Service) ListLocalGroups(ctx context.Context, request traymodel.LocalGroupQuery) traymodel.LocalGroupPage {
	var response proto.LocalGroupListResponse
	err := s.localCall(ctx, proto.LocalOperationGroupsList, proto.LocalGroupListRequest{Scope: request.Scope, RunID: request.RunID, Category: request.Category, PathContains: request.PathContains, FileNameContains: request.FileNameContains, ReviewStatus: request.ReviewStatus, Offset: request.Offset, Limit: request.Limit}, &response)
	if err != nil {
		code, summary := localError(err)
		return traymodel.LocalGroupPage{Groups: []traymodel.LocalGroup{}, ErrorCode: code, ErrorSummary: summary}
	}
	groups := make([]traymodel.LocalGroup, len(response.Groups))
	for i := range response.Groups {
		groups[i] = mapLocalGroup(response.Groups[i])
	}
	return traymodel.LocalGroupPage{OK: true, Groups: groups, Offset: response.Offset, NextOffset: response.NextOffset}
}

func (s *Service) SaveLocalReview(ctx context.Context, request traymodel.LocalReviewSave) traymodel.OperationResult {
	decisions := make([]proto.LocalReviewDecision, len(request.Decisions))
	for i, decision := range request.Decisions {
		decisions[i] = proto.LocalReviewDecision{FileID: decision.FileID, Decision: decision.Decision}
	}
	var response proto.LocalReviewSaveResponse
	err := s.localCall(ctx, proto.LocalOperationReviewSave, proto.LocalReviewSaveRequest{RunID: request.RunID, GroupID: request.GroupID, Reviewer: request.Reviewer, Note: request.Note, Decisions: decisions}, &response)
	if err != nil || !response.Saved {
		code, summary := localError(err)
		if err == nil {
			code, summary = "review_not_saved", "审核结果未保存"
		}
		return traymodel.OperationResult{ErrorCode: code, ErrorSummary: summary}
	}
	return traymodel.OperationResult{OK: true}
}

func (s *Service) PrepareLocalDelete(ctx context.Context, request traymodel.LocalDeletePrepare) traymodel.LocalDeletePreview {
	var response proto.LocalDeletePreview
	err := s.localCall(ctx, proto.LocalOperationDeletePrepare, proto.LocalDeletePrepareRequest{RunID: request.RunID, GroupID: request.GroupID}, &response)
	if err != nil {
		code, summary := localError(err)
		return traymodel.LocalDeletePreview{Files: []traymodel.LocalDeleteFile{}, ErrorCode: code, ErrorSummary: summary}
	}
	files := make([]traymodel.LocalDeleteFile, len(response.Files))
	for i, file := range response.Files {
		files[i] = traymodel.LocalDeleteFile{FileID: file.FileID, Path: file.Path, Size: file.Size}
	}
	s.deleteMu.Lock()
	s.deleteTokens[response.BatchID] = localDeleteToken{digest: response.SelectionDigest, token: response.Token, expiresAt: response.ExpiresAt}
	s.deleteMu.Unlock()
	return traymodel.LocalDeletePreview{OK: true, BatchID: response.BatchID, SelectionDigest: response.SelectionDigest, Count: response.Count, TotalSize: response.TotalSize, ExpiresAt: response.ExpiresAt, Files: files}
}

func (s *Service) ExecuteLocalDelete(ctx context.Context, request traymodel.LocalDeleteExecute) traymodel.LocalDeleteBatch {
	s.deleteMu.Lock()
	authorization, ok := s.deleteTokens[request.BatchID]
	if ok {
		delete(s.deleteTokens, request.BatchID)
	}
	s.deleteMu.Unlock()
	if !ok || authorization.digest != request.SelectionDigest || authorization.token == "" {
		return traymodel.LocalDeleteBatch{Items: []traymodel.LocalDeleteItem{}, ErrorCode: "delete_authorization_expired", ErrorSummary: "删除确认已失效，请重新预览"}
	}
	var response proto.LocalDeleteBatch
	err := s.localCall(ctx, proto.LocalOperationDeleteExecute, proto.LocalDeleteExecuteRequest{BatchID: request.BatchID, SelectionDigest: request.SelectionDigest, Token: authorization.token}, &response)
	if err != nil {
		code, summary := localError(err)
		return traymodel.LocalDeleteBatch{Items: []traymodel.LocalDeleteItem{}, ErrorCode: code, ErrorSummary: summary}
	}
	items := make([]traymodel.LocalDeleteItem, len(response.Items))
	for i, item := range response.Items {
		items[i] = traymodel.LocalDeleteItem{FileID: item.FileID, Result: item.Result, ErrorCode: item.ErrorCode, Uncertain: item.Uncertain}
	}
	return traymodel.LocalDeleteBatch{OK: true, BatchID: response.BatchID, Status: response.Status, Requested: response.Requested, Succeeded: response.Succeeded, Failed: response.Failed, Uncertain: response.Uncertain, Items: items}
}

func (s *Service) GetLocalImagePreview(ctx context.Context, fileID int64) traymodel.ImagePreview {
	var response proto.LocalImagePreviewResponse
	err := s.localCall(ctx, proto.LocalOperationPreviewImage, proto.LocalImagePreviewRequest{FileID: fileID, MaxWidth: 1600, MaxHeight: 1200, Format: "jpeg", Quality: 85}, &response)
	if err != nil {
		code, summary := localError(err)
		return traymodel.ImagePreview{ErrorCode: code, ErrorSummary: summary}
	}
	return traymodel.ImagePreview{OK: true, MIME: response.MIME, Width: response.Width, Height: response.Height, DataBase64: base64.StdEncoding.EncodeToString(response.Bytes)}
}

const forceExitWaitTimeout = 15 * time.Second

func (s *Service) GetOverview(ctx context.Context) (traymodel.Overview, error) {
	if s == nil || s.store == nil {
		return traymodel.Overview{}, errors.New("app unavailable")
	}
	settings, err := s.store.LoadTraySettings()
	if err != nil {
		return traymodel.Overview{}, errors.New("settings unavailable")
	}
	overview := traymodel.Overview{
		Workers:         []traymodel.WorkerState{},
		MachineID:       sanitizeText(s.machineID),
		AgentStartMode:  settings.AgentStartMode,
		HelperStartMode: settings.HelperStartMode,
		HelperEnabled:   settings.HelperEnabled,
	}
	if s.agent != nil {
		overview.Agent = sanitizeComponentState(s.agent.Refresh(ctx))
	} else {
		overview.Agent = attentionState("unavailable", "Agent unavailable")
	}
	if s.helper != nil {
		overview.Helper = sanitizeComponentState(s.helper.Refresh(ctx))
	} else {
		overview.Helper = attentionState("unavailable", "Helper unavailable")
	}
	if overview.MachineID == "" {
		overview.Agent.NeedsAttention = true
	}
	if s.workers != nil {
		workers, workerErr := s.workers.Snapshot(ctx)
		if workerErr != nil {
			overview.Agent.NeedsAttention = true
		} else {
			overview.Workers = sanitizeWorkers(workers)
		}
	}
	overview.HelperTaskDrift = false
	if s.loginStart != nil {
		enabled, current, loginErr := s.loginStart.Enabled()
		if loginErr != nil {
			overview.LoginStartDrift = true
			overview.Agent.NeedsAttention = true
		} else {
			overview.LoginStartDrift = enabled != settings.LoginStartTray || (current != "" && !enabled)
		}
	} else {
		overview.LoginStartDrift = true
	}
	overview.Helper = normalizeDisabledHelperState(settings.HelperEnabled, overview.Helper)
	return overview, nil
}

func (s *Service) GetAgentForm(ctx context.Context) (config.AgentForm, error) {
	if s == nil || s.agentConfig == nil {
		return config.AgentForm{}, errors.New("app unavailable")
	}
	value, err := s.agentConfig.LoadAgentForm(ctx)
	return value, safeUIError(err)
}

func (s *Service) ValidateAgent(ctx context.Context, value config.AgentForm) []config.FieldError {
	if s == nil || s.agentConfig == nil {
		return []config.FieldError{{Field: "agent", Code: "unavailable", Message: "验证服务不可用"}}
	}
	return sanitizeFieldErrors(s.agentConfig.ValidateAgentForm(ctx, value))
}

func (s *Service) SaveAgent(ctx context.Context, value config.AgentForm) traymodel.ConfigApplyResult {
	if s == nil {
		return configApplyFailure("unavailable", "配置服务不可用")
	}
	s.agentConfigMu.Lock()
	defer s.agentConfigMu.Unlock()
	return s.saveAgentLocked(ctx, value)
}

func (s *Service) saveAgentLocked(ctx context.Context, value config.AgentForm) traymodel.ConfigApplyResult {
	if fields := s.ValidateAgent(ctx, value); len(fields) != 0 {
		return configApplyFailure("invalid_config", fields[0].Message)
	}
	if s == nil || s.agentConfig == nil {
		return configApplyFailure("unavailable", "配置服务不可用")
	}
	saved, err := s.agentConfig.SaveAgentForm(ctx, value)
	if err != nil {
		if errors.Is(err, config.ErrSaveVerify) {
			return configApplyFailure("save_verify_failed", err.Error())
		}
		return configApplyFailure("save_failed", err.Error())
	}
	if s.agentFingerprint == nil {
		return savedConfigApplyFailure("unavailable", "Agent 摘要更新服务不可用", saved.SHA256, true)
	}
	if updated := sanitizeOperation(s.agentFingerprint.UpdateExpectedSHA256(saved.SHA256)); !updated.OK {
		return savedConfigApplyFailure(updated.ErrorCode, updated.ErrorSummary, saved.SHA256, true)
	}
	needsRestart := saved.RestartRequired
	if s.agent != nil {
		needsRestart = s.agent.Refresh(ctx).NeedsRestart
	}
	return traymodel.ConfigApplyResult{OK: true, Saved: true, SHA256: saved.SHA256, NeedsRestart: needsRestart}
}

func (s *Service) SaveAndRestartAgent(ctx context.Context, value config.AgentForm) traymodel.ConfigApplyResult {
	if s == nil {
		return configApplyFailure("unavailable", "配置服务不可用")
	}
	s.agentConfigMu.Lock()
	defer s.agentConfigMu.Unlock()
	saved := s.saveAgentLocked(ctx, value)
	if !saved.OK {
		return saved
	}
	stopped := s.StopAgent(ctx)
	if !stopped.OK {
		return savedConfigApplyFailure(stopped.ErrorCode, stopped.ErrorSummary, saved.SHA256, true)
	}
	started := s.StartAgent(ctx)
	if !started.OK {
		return savedConfigApplyFailure(started.ErrorCode, started.ErrorSummary, saved.SHA256, true)
	}
	return traymodel.ConfigApplyResult{OK: true, Saved: true, Restarted: true, SHA256: saved.SHA256}
}

func (s *Service) StartAgent(ctx context.Context) traymodel.OperationResult {
	if s == nil {
		return operationFailure("unavailable", "组件服务不可用")
	}
	if s.agentConfig != nil {
		s.agentConfig.PromotePendingEndpoint()
	}
	return callComponent(s, s.agent, ctx, "start")
}
func (s *Service) StopAgent(ctx context.Context) traymodel.OperationResult {
	if s == nil {
		return operationFailure("unavailable", "组件服务不可用")
	}
	return callComponent(s, s.agent, ctx, "stop")
}
func (s *Service) RestartAgent(ctx context.Context) traymodel.OperationResult {
	if s == nil {
		return operationFailure("unavailable", "组件服务不可用")
	}
	return callComponent(s, s.agent, ctx, "restart")
}
func (s *Service) ForceStopAgent(ctx context.Context) traymodel.OperationResult {
	if s == nil {
		return operationFailure("unavailable", "组件服务不可用")
	}
	return callComponent(s, s.agent, ctx, "force")
}

func (s *Service) ForceExitAll(ctx context.Context) traymodel.ForceExitResult {
	if s == nil {
		return forceExitFailure([]string{"service"}, false)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	failed := make([]string, 0, 3)
	timedOut := false
	workers := []traymodel.WorkerState{}
	if s.workers != nil {
		if snapshot, err := s.workers.Snapshot(ctx); err == nil {
			workers = snapshot
		}
	}
	if result := callComponent(s, s.helper, ctx, "force"); !result.OK {
		failed = append(failed, "helper")
		if result.ErrorCode == "force_exit_timeout" {
			timedOut = true
		}
	}
	if result := callComponent(s, s.agent, ctx, "force"); !result.OK {
		failed = append(failed, "agent")
		if result.ErrorCode == "force_exit_timeout" {
			timedOut = true
		}
	}
	waitCtx, cancel := context.WithTimeout(ctx, forceExitWaitTimeout)
	defer cancel()
	for _, worker := range workers {
		if worker.PID <= 0 {
			continue
		}
		if s.processWaiter == nil || s.processWaiter.WaitPIDGone(waitCtx, worker.PID) != nil {
			failed = append(failed, fmt.Sprintf("worker:%d", worker.PID))
			if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				timedOut = true
			}
		}
	}
	if len(failed) != 0 {
		return forceExitFailure(failed, timedOut)
	}
	return traymodel.ForceExitResult{OK: true, FailedComponents: []string{}}
}

func forceExitFailure(failed []string, timedOut bool) traymodel.ForceExitResult {
	code := "force_exit_failed"
	if timedOut {
		code = "force_exit_timeout"
	}
	return traymodel.ForceExitResult{
		FailedComponents: append([]string(nil), failed...),
		ErrorCode:        code,
		ErrorSummary:     "后台进程未全部退出",
	}
}

func (s *Service) GetHelperForm(context.Context) (config.HelperForm, error) {
	if s == nil || s.store == nil {
		return config.HelperForm{}, errors.New("app unavailable")
	}
	value, err := s.store.LoadHelperForm()
	return value, safeUIError(err)
}

func (s *Service) ValidateHelper(_ context.Context, value config.HelperForm) []config.FieldError {
	if s == nil || s.store == nil {
		return []config.FieldError{{Field: "helper", Code: "unavailable", Message: "验证服务不可用"}}
	}
	return sanitizeFieldErrors(s.store.ValidateHelperForm(value))
}

func (s *Service) SaveHelper(ctx context.Context, value config.HelperForm) traymodel.ConfigApplyResult {
	if fields := s.ValidateHelper(ctx, value); len(fields) != 0 {
		return configApplyFailure("invalid_config", fields[0].Message)
	}
	if s == nil || s.store == nil {
		return configApplyFailure("unavailable", "Helper 配置服务不可用")
	}
	digest, err := s.store.SaveHelperForm(value)
	if err != nil {
		if errors.Is(err, config.ErrSaveVerify) {
			return configApplyFailure("save_verify_failed", err.Error())
		}
		return configApplyFailure("save_failed", err.Error())
	}
	if s.helperFingerprint == nil {
		return savedConfigApplyFailure("unavailable", "Helper 摘要更新服务不可用", digest, true)
	}
	if updated := sanitizeOperation(s.helperFingerprint.UpdateExpectedSHA256(digest)); !updated.OK {
		return savedConfigApplyFailure(updated.ErrorCode, updated.ErrorSummary, digest, true)
	}
	needsRestart := false
	if s.helper != nil {
		needsRestart = s.helper.Refresh(ctx).NeedsRestart
	}
	return traymodel.ConfigApplyResult{OK: true, Saved: true, SHA256: digest, NeedsRestart: needsRestart}
}

func (s *Service) StartHelper(ctx context.Context) traymodel.OperationResult {
	settings, result := s.helperSettings()
	if !result.OK {
		return result
	}
	if !settings.HelperEnabled {
		return operationFailure("helper_disabled", "Helper 未启用")
	}
	if result := s.validateHelperStart(); !result.OK {
		return result
	}
	return callComponent(s, s.helper, ctx, "start")
}

func (s *Service) StopHelper(ctx context.Context) traymodel.OperationResult {
	settings, result := s.helperSettings()
	if !result.OK {
		return result
	}
	if settings.HelperStartMode == traymodel.StartAutomatic {
		return callComponent(s, s.helper, ctx, "stop")
	}
	return callComponent(s, s.helper, ctx, "stop")
}

func (s *Service) RestartHelper(ctx context.Context) traymodel.OperationResult {
	_, result := s.helperSettings()
	if !result.OK {
		return result
	}
	if result := s.validateHelperStart(); !result.OK {
		return result
	}
	return callComponent(s, s.helper, ctx, "restart")
}

func (s *Service) validateHelperStart() traymodel.OperationResult {
	if s == nil || s.store == nil {
		return operationFailure("helper_config_invalid", "Helper 配置不可用")
	}
	value, err := s.store.LoadHelperForm()
	if err != nil {
		return operationFailure("helper_config_invalid", "Helper 配置读取失败")
	}
	if fields := s.store.ValidateHelperForm(value); len(fields) != 0 {
		return operationFailure("helper_config_invalid", fields[0].Message)
	}
	return traymodel.OperationResult{OK: true}
}

func (s *Service) ForceStopHelper(ctx context.Context) traymodel.OperationResult {
	if s == nil {
		return operationFailure("unavailable", "组件服务不可用")
	}
	return callComponent(s, s.helper, ctx, "force")
}

func (s *Service) GetTraySettings(context.Context) (traymodel.TraySettings, error) {
	if s == nil || s.store == nil {
		return traymodel.TraySettings{}, errors.New("app unavailable")
	}
	value, err := s.store.LoadTraySettings()
	return value, safeUIError(err)
}

func (s *Service) SaveTraySettings(ctx context.Context, value traymodel.TraySettings) traymodel.OperationResult {
	if err := value.Validate(); err != nil {
		return operationFailure("invalid_config", err.Error())
	}
	if s == nil || s.store == nil || s.loginStart == nil {
		return operationFailure("unavailable", "设置服务不可用")
	}
	current, err := s.store.LoadTraySettings()
	if err != nil {
		return operationFailure("load_failed", err.Error())
	}
	loginChanged := current.LoginStartTray != value.LoginStartTray

	if loginChanged {
		if value.LoginStartTray {
			err = s.loginStart.Enable(s.trayExecutable)
		} else {
			err = s.loginStart.Disable()
		}
		if err != nil {
			s.reloadSettingsActualState(ctx, false)
			return operationFailure("settings_partially_applied", err.Error())
		}
	}

	if err := s.store.SaveTraySettings(value); err != nil {
		s.reloadSettingsActualState(ctx, false)
		if loginChanged {
			return operationFailure("settings_partially_applied", err.Error())
		}
		return operationFailure("save_failed", err.Error())
	}
	if err := s.reloadSettingsActualState(ctx, false); err != nil {
		return operationFailure("settings_partially_applied", err.Error())
	}
	return traymodel.OperationResult{OK: true}
}

func (s *Service) reloadSettingsActualState(ctx context.Context, reloadTask bool) error {
	if _, err := s.store.LoadTraySettings(); err != nil {
		return err
	}
	if _, _, err := s.loginStart.Enabled(); err != nil {
		return err
	}
	if reloadTask && s.task != nil {
		if _, err := s.task.Inspect(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) OpenLocation(ctx context.Context, kind traymodel.LocationKind) traymodel.OperationResult {
	if s == nil || s.pathResolver == nil || s.opener == nil {
		return operationFailure("unavailable", "目录服务不可用")
	}
	switch kind {
	case traymodel.AgentLogs, traymodel.HelperLogs, traymodel.AgentBackup, traymodel.HelperBackup:
	default:
		return operationFailure("invalid_location", "未知目录")
	}
	location, ok := s.locations[kind]
	if !ok || !filepath.IsAbs(location.Path) || !filepath.IsAbs(location.Root) {
		return operationFailure("invalid_location", "未知目录")
	}
	root, err := s.pathResolver.Final(location.Root)
	if err != nil {
		return operationFailure("invalid_location", "目录根不可用")
	}
	path, err := s.pathResolver.Final(location.Path)
	if err != nil || !sameOrBelow(path, root) {
		return operationFailure("invalid_location", "目录超出固定范围")
	}
	if err := s.opener.Open(ctx, path); err != nil {
		return operationFailure("open_failed", err.Error())
	}
	return traymodel.OperationResult{OK: true}
}

func (s *Service) helperSettings() (traymodel.TraySettings, traymodel.OperationResult) {
	if s == nil || s.store == nil {
		return traymodel.TraySettings{}, operationFailure("unavailable", "设置服务不可用")
	}
	settings, err := s.store.LoadTraySettings()
	if err != nil {
		return traymodel.TraySettings{}, operationFailure("settings_unavailable", err.Error())
	}
	return settings, traymodel.OperationResult{OK: true}
}

func callComponent(s *Service, component Component, ctx context.Context, operation string) traymodel.OperationResult {
	if s == nil || component == nil {
		return operationFailure("unavailable", "组件服务不可用")
	}
	var result traymodel.OperationResult
	switch operation {
	case "start":
		result = component.Start(ctx)
	case "stop":
		result = component.Stop(ctx)
	case "restart":
		result = component.Restart(ctx)
	case "force":
		result = component.ForceStopTracked(ctx)
	}
	return sanitizeOperation(result)
}

func sameOrBelow(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	relative, err := filepath.Rel(strings.ToLower(root), strings.ToLower(path))
	return err == nil && !filepath.IsAbs(relative) && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))))
}

func stableCode(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return sanitizeText(value)
}
func safeUIError(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(sanitizeText(err.Error()))
}
func operationFailure(code, summary string) traymodel.OperationResult {
	return traymodel.OperationResult{ErrorCode: stableCode(code, "internal_error"), ErrorSummary: sanitizeText(summary)}
}

func configApplyFailure(code, summary string) traymodel.ConfigApplyResult {
	return traymodel.ConfigApplyResult{ErrorCode: stableCode(code, "internal_error"), ErrorSummary: sanitizeText(summary)}
}

func savedConfigApplyFailure(code, summary, digest string, needsRestart bool) traymodel.ConfigApplyResult {
	result := configApplyFailure(code, summary)
	result.Saved = true
	result.SHA256 = digest
	result.NeedsRestart = needsRestart
	return result
}

func sanitizeOperation(value traymodel.OperationResult) traymodel.OperationResult {
	if value.OK {
		return traymodel.OperationResult{OK: true}
	}
	value.ErrorCode = stableCode(value.ErrorCode, "internal_error")
	value.ErrorSummary = sanitizeText(value.ErrorSummary)
	return value
}
func sanitizeFieldErrors(values []config.FieldError) []config.FieldError {
	result := append([]config.FieldError{}, values...)
	for i := range result {
		result[i].Field = sanitizeText(result[i].Field)
		result[i].Code = sanitizeText(result[i].Code)
		result[i].Message = sanitizeText(result[i].Message)
	}
	return result
}
func sanitizeComponentState(value traymodel.ComponentState) traymodel.ComponentState {
	value.ErrorCode = sanitizeText(value.ErrorCode)
	value.ErrorSummary = sanitizeText(value.ErrorSummary)
	return value
}
func normalizeDisabledHelperState(enabled bool, value traymodel.ComponentState) traymodel.ComponentState {
	if enabled || value.PID > 0 {
		return value
	}
	normalized := traymodel.ComponentState{Lifecycle: traymodel.Stopped}
	if isLowerSHA256(value.SavedConfigSHA256) {
		normalized.SavedConfigSHA256 = value.SavedConfigSHA256
	}
	return normalized
}
func isLowerSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
func sanitizeWorkers(values []traymodel.WorkerState) []traymodel.WorkerState {
	result := append([]traymodel.WorkerState{}, values...)
	for i := range result {
		result[i].CurrentTaskSummary = sanitizeText(result[i].CurrentTaskSummary)
		result[i].LastErrorSummary = sanitizeText(result[i].LastErrorSummary)
	}
	return result
}
func attentionState(code, summary string) traymodel.ComponentState {
	return traymodel.ComponentState{Lifecycle: traymodel.Failed, ErrorCode: sanitizeText(code), ErrorSummary: sanitizeText(summary), NeedsAttention: true}
}
