package gui

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"dedup/internal/proto"
)

type API struct {
	pool       *Pool
	tasks      *TaskRegistry
	pg         *pgxpool.Pool
	analysis   *AnalysisHandlers
	groups     *GroupHandlers
	delete     deleteHTTPService
	config     guiConfigStore
	filesystem filesystemBrowseService
}

const filesystemBrowseTimeout = 10 * time.Second

type filesystemBrowseService interface {
	Browse(context.Context, string, proto.FilesystemBrowseRequest) (proto.FilesystemBrowseResponse, error)
}

func NewAPI(
	pool *Pool,
	tasks *TaskRegistry,
	pg *pgxpool.Pool,
	runners ...AnalysisRunner,
) *API {
	var runner AnalysisRunner
	if len(runners) > 0 {
		runner = runners[0]
	}
	var groupDB groupQueryDB
	if pg != nil {
		groupDB = pg
	}
	return &API{
		pool:     pool,
		tasks:    tasks,
		pg:       pg,
		analysis: NewAnalysisHandlers(runner),
		groups:   NewGroupHandlers(groupDB),
	}
}

func (api *API) SetDeleteService(service *DeleteService) {
	if service == nil {
		api.delete = nil
		return
	}
	api.delete = service
}

func (api *API) SetFilesystemBrowser(service filesystemBrowseService) {
	api.filesystem = service
}

func (api *API) BeginAnalysisShutdown() {
	api.analysis.BeginShutdown()
}

func (api *API) WaitForAnalysis() {
	api.analysis.Wait()
}

func (api *API) SetAnalysisSuccessHook(hook func() error) {
	api.analysis.SetSuccessHook(hook)
}

func (api *API) Routes() *http.ServeMux {
	legacy := http.NewServeMux()
	legacy.HandleFunc("GET /api/agents", api.handleAgents)
	legacy.HandleFunc("POST /api/agents/{machine_id}/filesystem/browse", api.handleFilesystemBrowse)
	legacy.Handle("GET /api/config", newConfigHTTP(api.config))
	legacy.Handle("PUT /api/config", newConfigHTTP(api.config))
	legacy.HandleFunc("POST /api/scan", api.handleScan)
	legacy.HandleFunc("GET /api/tasks", api.handleTasks)
	legacy.HandleFunc("GET /api/dup_groups", api.handleDupGroups)
	legacy.HandleFunc("GET /api/dup_groups/{sha512}", api.handleDupMembers)
	legacy.HandleFunc("GET /api/groups", api.groups.handleList)
	legacy.HandleFunc("GET /api/groups/{id}", api.groups.handleDetail)
	legacy.HandleFunc("GET /groups", api.groups.handlePage)
	api.analysis.Register(legacy)
	legacy.Handle("GET /", http.FileServerFS(webFS()))

	deleteRoutes := http.NewServeMux()
	deleteRoutes.HandleFunc("POST /prepare", api.handleDeletePrepare)
	deleteRoutes.HandleFunc("POST /execute", api.handleDeleteExecute)
	deleteRoutes.HandleFunc("GET /tasks", api.handleDeleteStatus)
	deleteRoutes.HandleFunc("GET /tasks/{$}", api.handleDeleteStatus)
	deleteRoutes.HandleFunc("GET /tasks/{task_id}", api.handleDeleteStatus)

	mux := http.NewServeMux()
	mux.Handle("/api/delete", http.NotFoundHandler())
	mux.Handle("/api/delete/", http.StripPrefix("/api/delete", deleteRoutes))
	mux.Handle("/", legacy)
	return mux
}

type filesystemBrowseHTTPRequest struct {
	Path       string `json:"path"`
	ShowHidden bool   `json:"show_hidden"`
	Cursor     string `json:"cursor"`
	Limit      int    `json:"limit"`
}

type filesystemBrowseHTTPEntry struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Kind       string `json:"kind"`
	Hidden     bool   `json:"hidden"`
	System     bool   `json:"system"`
	Selectable bool   `json:"selectable"`
}

type filesystemBrowseHTTPResponse struct {
	CurrentPath string                      `json:"current_path"`
	ParentPath  string                      `json:"parent_path"`
	Entries     []filesystemBrowseHTTPEntry `json:"entries"`
	NextCursor  string                      `json:"next_cursor"`
}

func (api *API) handleFilesystemBrowse(response http.ResponseWriter, request *http.Request) {
	if api.filesystem == nil {
		writeJSON(response, http.StatusServiceUnavailable, map[string]string{"error": "files_disabled"})
		return
	}
	var input filesystemBrowseHTTPRequest
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeFilesystemBrowseRequestError(response)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeFilesystemBrowseRequestError(response)
		return
	}
	browseRequest := proto.FilesystemBrowseRequest{
		Path:       input.Path,
		ShowHidden: input.ShowHidden,
		Cursor:     input.Cursor,
		Limit:      input.Limit,
	}
	validateRequest := browseRequest
	validateRequest.RequestID = "http-validation"
	if err := validateRequest.Validate(); err != nil {
		writeFilesystemBrowseRequestError(response)
		return
	}
	browseContext, cancel := context.WithTimeout(request.Context(), filesystemBrowseTimeout)
	defer cancel()
	browseResponse, err := api.filesystem.Browse(
		browseContext,
		request.PathValue("machine_id"),
		browseRequest,
	)
	if err != nil {
		status, code := filesystemBrowseErrorStatus(err)
		writeJSON(response, status, map[string]string{"error": code})
		return
	}
	if browseResponse.ErrorCode != "" {
		status, code := filesystemBrowseResponseStatus(browseResponse.ErrorCode)
		writeJSON(response, status, map[string]string{"error": code})
		return
	}
	entries := make([]filesystemBrowseHTTPEntry, len(browseResponse.Entries))
	for index, entry := range browseResponse.Entries {
		entries[index] = filesystemBrowseHTTPEntry{
			Name: entry.Name, Path: entry.Path, Kind: entry.Kind,
			Hidden: entry.Hidden, System: entry.System, Selectable: entry.Selectable,
		}
	}
	writeJSON(response, http.StatusOK, filesystemBrowseHTTPResponse{
		CurrentPath: browseResponse.CurrentPath,
		ParentPath:  browseResponse.ParentPath,
		Entries:     entries,
		NextCursor:  browseResponse.NextCursor,
	})
}

func writeFilesystemBrowseRequestError(response http.ResponseWriter) {
	writeJSON(response, http.StatusBadRequest, map[string]string{
		"error": "invalid_filesystem_browse_request",
	})
}

func filesystemBrowseErrorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "browse_timeout"
	case errors.Is(err, ErrFilesystemAgentOffline):
		return http.StatusServiceUnavailable, "agent_offline"
	default:
		return http.StatusBadGateway, "browse_failed"
	}
}

func filesystemBrowseResponseStatus(errorCode string) (int, string) {
	switch errorCode {
	case "access_denied":
		return http.StatusForbidden, errorCode
	case "path_not_found":
		return http.StatusNotFound, errorCode
	case "browse_cancelled":
		return http.StatusGatewayTimeout, errorCode
	case "files_disabled", "browse_unsupported", "agent_disconnected", "browse_busy":
		return http.StatusServiceUnavailable, errorCode
	default:
		return http.StatusBadGateway, "browse_failed"
	}
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func (api *API) handleAgents(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, api.pool.Status())
}

type scanRequest struct {
	TaskID    string   `json:"task_id"`
	MachineID string   `json:"machine_id"`
	Roots     []string `json:"roots"`
	Phase     uint8    `json:"phase"`
	Rescan    bool     `json:"rescan"`
}

func (api *API) handleScan(response http.ResponseWriter, request *http.Request) {
	var input scanRequest
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{
			"error": err.Error(),
		})
		return
	}
	if input.MachineID == "" || len(input.Roots) == 0 {
		writeJSON(response, http.StatusBadRequest, map[string]string{
			"error": "machine_id and roots required",
		})
		return
	}
	for _, root := range input.Roots {
		if strings.TrimSpace(root) == "" {
			writeJSON(response, http.StatusBadRequest, map[string]string{
				"error": "roots cannot contain an empty path",
			})
			return
		}
	}
	if input.Phase == 0 {
		input.Phase = 1
	}
	if input.TaskID == "" {
		input.TaskID = uuid.NewString()
	} else if _, err := uuid.Parse(input.TaskID); err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{
			"error": "task_id must be a UUID",
		})
		return
	}

	task := &proto.ScanTask{
		TaskID: input.TaskID,
		Roots:  input.Roots,
		Phase:  input.Phase,
		Options: proto.ScanOptions{
			Rescan: input.Rescan,
		},
	}
	if err := api.tasks.Register(&TaskInfo{
		TaskID:    input.TaskID,
		MachineID: input.MachineID,
		Phase:     int(input.Phase),
		Roots:     input.Roots,
		Rescan:    input.Rescan,
		Status:    "sent",
		Total:     -1,
		UpdatedAt: time.Now(),
	}); err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, ErrTaskEnvelopeConflict) {
			status = http.StatusConflict
		}
		writeJSON(response, status, map[string]string{
			"error": err.Error(),
		})
		return
	}
	if err := api.pool.Send(input.MachineID, proto.MsgScanTask, task); err != nil {
		api.tasks.MarkSendFailed(input.TaskID, err)
		writeJSON(response, http.StatusBadGateway, map[string]string{
			"error": err.Error(),
		})
		return
	}
	writeJSON(response, http.StatusOK, map[string]string{
		"task_id": input.TaskID,
	})
}

func (api *API) handleTasks(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, api.tasks.List())
}

type DupGroup struct {
	SHA512      string `json:"sha512"`
	MemberCount int64  `json:"member_count"`
	TotalBytes  int64  `json:"total_bytes"`
	WastedBytes int64  `json:"wasted_bytes"`
	Machines    int64  `json:"machines"`
}

func (api *API) handleDupGroups(response http.ResponseWriter, request *http.Request) {
	if api.pg == nil {
		writeJSON(response, http.StatusServiceUnavailable, map[string]string{
			"error": "central database unavailable",
		})
		return
	}
	query := request.URL.Query()
	limit, _ := strconv.Atoi(query.Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	offset, _ := strconv.Atoi(query.Get("offset"))
	if offset < 0 {
		offset = 0
	}
	rows, err := api.pg.Query(request.Context(), `
		SELECT
		    sha512,
		    count(*) AS members,
		    sum(size) AS total_bytes,
		    (count(*) - 1) * max(size) AS wasted_bytes,
		    count(DISTINCT machine_id) AS machines
		FROM files
		WHERE sha512 IS NOT NULL AND status <> 'deleted'
		GROUP BY sha512
		HAVING count(*) > 1
		ORDER BY wasted_bytes DESC
		LIMIT $1 OFFSET $2;`, limit, offset)
	if err != nil {
		writeJSON(response, http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
		return
	}
	defer rows.Close()
	out := make([]DupGroup, 0)
	for rows.Next() {
		var group DupGroup
		if err := rows.Scan(
			&group.SHA512,
			&group.MemberCount,
			&group.TotalBytes,
			&group.WastedBytes,
			&group.Machines,
		); err != nil {
			writeJSON(response, http.StatusInternalServerError, map[string]string{
				"error": err.Error(),
			})
			return
		}
		out = append(out, group)
	}
	if err := rows.Err(); err != nil {
		writeJSON(response, http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
		return
	}
	writeJSON(response, http.StatusOK, out)
}

type DupMember struct {
	MachineID string `json:"machine_id"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	MTime     int64  `json:"mtime"`
}

func (api *API) handleDupMembers(response http.ResponseWriter, request *http.Request) {
	if api.pg == nil {
		writeJSON(response, http.StatusServiceUnavailable, map[string]string{
			"error": "central database unavailable",
		})
		return
	}
	hash := request.PathValue("sha512")
	decoded, err := hex.DecodeString(hash)
	if err != nil || len(decoded) != 64 {
		writeJSON(response, http.StatusBadRequest, map[string]string{
			"error": "sha512 must be 128 hexadecimal characters",
		})
		return
	}
	rows, err := api.pg.Query(request.Context(), `
		SELECT machine_id, path, size, mtime
		FROM files
		WHERE sha512 = $1 AND status <> 'deleted'
		ORDER BY machine_id, path;`, strings.ToLower(hash))
	if err != nil {
		writeJSON(response, http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
		return
	}
	defer rows.Close()
	out := make([]DupMember, 0)
	for rows.Next() {
		var member DupMember
		if err := rows.Scan(
			&member.MachineID,
			&member.Path,
			&member.Size,
			&member.MTime,
		); err != nil {
			writeJSON(response, http.StatusInternalServerError, map[string]string{
				"error": err.Error(),
			})
			return
		}
		out = append(out, member)
	}
	if err := rows.Err(); err != nil {
		writeJSON(response, http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
		return
	}
	writeJSON(response, http.StatusOK, out)
}
