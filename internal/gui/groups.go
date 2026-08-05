package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

type groupQueryDB interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type GroupHandlers struct {
	db groupQueryDB
}

func NewGroupHandlers(db groupQueryDB) *GroupHandlers {
	return &GroupHandlers{db: db}
}

type GroupListResponse struct {
	Kind   string         `json:"kind"`
	Page   int            `json:"page"`
	Size   int            `json:"size"`
	Total  int64          `json:"total"`
	Groups []GroupSummary `json:"groups"`
}

type GroupSummary struct {
	ID          int64     `json:"id"`
	Kind        string    `json:"kind"`
	MemberCount int64     `json:"member_count"`
	TotalBytes  int64     `json:"total_bytes"`
	WastedBytes int64     `json:"wasted_bytes"`
	RepMachine  string    `json:"rep_machine"`
	RepPath     string    `json:"rep_path"`
	Machines    []string  `json:"machines"`
	CreatedAt   time.Time `json:"created_at"`
}

type GroupDetail struct {
	ID                   int64         `json:"id"`
	Kind                 string        `json:"kind"`
	RepresentativeFileID *int64        `json:"representative_file_id"`
	MemberTotal          int64         `json:"member_total"`
	MemberPage           int           `json:"member_page,omitempty"`
	MemberSize           int           `json:"member_size,omitempty"`
	Members              []GroupMember `json:"members"`
}

type GroupMember struct {
	FileID    int64           `json:"file_id"`
	MachineID string          `json:"machine_id"`
	Path      string          `json:"path"`
	Size      int64           `json:"size"`
	MTime     int64           `json:"mtime"`
	ScoreJSON json.RawMessage `json:"score_json"`
}

const (
	groupSortMembers = "members_desc"
	groupSortNewest  = "newest"
	groupSortReclaim = "reclaim_desc"
)

type groupListQuery struct {
	kind       string
	page       int
	size       int
	query      string
	machine    string
	minMembers int64
	sort       string
}

type groupMemberPagination struct {
	enabled bool
	page    int
	size    int
	offset  int
}

func (handlers *GroupHandlers) handleList(
	response http.ResponseWriter,
	request *http.Request,
) {
	listQuery, err := parseGroupListQuery(request)
	if err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{
			"error": err.Error(),
		})
		return
	}
	if handlers == nil || handlers.db == nil {
		writeGroupUnavailable(response)
		return
	}
	offset64 := int64(listQuery.page-1) * int64(listQuery.size)
	if offset64 > int64(^uint(0)>>1) {
		writeJSON(response, http.StatusBadRequest, map[string]string{
			"error": "page and size produce an invalid offset",
		})
		return
	}

	var total int64
	if err := handlers.db.QueryRow(request.Context(), `
		WITH all_live AS (
			SELECT g.id AS group_id,f.id,f.machine_id,f.path,f.size
			FROM dup_groups AS g
			JOIN dup_members AS m ON m.group_id=g.id
			JOIN files AS f ON f.id=m.file_id
			WHERE g.kind=$1
			  AND f.status <> 'deleted'
		),
		summary AS (
			SELECT group_id,count(*) AS live_member_count
			FROM all_live
			GROUP BY group_id
		),
		matching_groups AS (
			SELECT DISTINCT group_id
			FROM all_live
			WHERE ($2='' OR machine_id=$2)
			  AND ($3='' OR strpos(lower(path),lower($3)) > 0)
		)
		SELECT count(*)
		FROM summary
		JOIN matching_groups USING (group_id)
		WHERE summary.live_member_count >= $4`,
		listQuery.kind,
		listQuery.machine,
		listQuery.query,
		listQuery.minMembers,
	).Scan(&total); err != nil {
		writeGroupInternalError(response, "count groups", err)
		return
	}
	if total < 0 {
		writeGroupInternalError(
			response,
			"count groups",
			fmt.Errorf("negative group count %d", total),
		)
		return
	}

	orderBy, err := groupListOrderBy(listQuery.sort)
	if err != nil {
		writeGroupInternalError(response, "select group sort", err)
		return
	}
	pageOrderBy, err := groupPageOrderBy(listQuery.sort)
	if err != nil {
		writeGroupInternalError(response, "select paged group sort", err)
		return
	}
	rows, err := handlers.db.Query(request.Context(), fmt.Sprintf(`
		WITH all_live AS (
			SELECT
				g.id AS group_id,
				f.id,
				f.machine_id,
				f.path,
				f.size
			FROM dup_groups AS g
			JOIN dup_members AS m ON m.group_id=g.id
			JOIN files AS f ON f.id=m.file_id
			WHERE g.kind=$1
			  AND f.status <> 'deleted'
		),
		summary AS (
			SELECT
				group_id,
				count(*) AS live_member_count,
				array_agg(DISTINCT machine_id ORDER BY machine_id) AS machines,
				sum(size) AS total_bytes,
				GREATEST(sum(size)-max(size),0) AS wasted_bytes
			FROM all_live
			GROUP BY group_id
		),
		matching_groups AS (
			SELECT DISTINCT group_id
			FROM all_live
			WHERE ($2='' OR machine_id=$2)
			  AND ($3='' OR strpos(lower(path),lower($3)) > 0)
		),
		page_groups AS MATERIALIZED (
			SELECT
				g.id,
				g.kind,
				g.representative_file_id,
				summary.live_member_count,
				summary.total_bytes,
				summary.wasted_bytes,
				summary.machines,
				g.created_at
			FROM dup_groups AS g
			JOIN summary ON summary.group_id=g.id
			JOIN matching_groups ON matching_groups.group_id=summary.group_id
			WHERE summary.live_member_count >= $4
			ORDER BY %s
			LIMIT $5 OFFSET $6
		)
		SELECT
			g.id,
			g.kind,
			g.live_member_count,
			g.total_bytes,
			g.wasted_bytes,
			effective_representative.machine_id,
			effective_representative.path,
			g.machines,
			g.created_at
		FROM page_groups AS g
		JOIN LATERAL (
			SELECT
				effective_files.id,
				effective_files.machine_id,
				effective_files.path
			FROM dup_members AS effective_members
			JOIN files AS effective_files
			  ON effective_files.id=effective_members.file_id
			WHERE effective_members.group_id=g.id
			  AND effective_files.status <> 'deleted'
			ORDER BY
			  CASE WHEN effective_files.id=g.representative_file_id THEN 0 ELSE 1 END,
			  effective_files.machine_id,effective_files.path,effective_files.id
			LIMIT 1
		) AS effective_representative ON true
		ORDER BY %s`, orderBy, pageOrderBy),
		listQuery.kind,
		listQuery.machine,
		listQuery.query,
		listQuery.minMembers,
		listQuery.size,
		int(offset64),
	)
	if err != nil {
		writeGroupInternalError(response, "query groups", err)
		return
	}
	defer rows.Close()
	groups := make([]GroupSummary, 0)
	for rows.Next() {
		var group GroupSummary
		if err := rows.Scan(
			&group.ID,
			&group.Kind,
			&group.MemberCount,
			&group.TotalBytes,
			&group.WastedBytes,
			&group.RepMachine,
			&group.RepPath,
			&group.Machines,
			&group.CreatedAt,
		); err != nil {
			writeGroupInternalError(response, "scan group", err)
			return
		}
		if err := validateGroupSummary(group, listQuery.kind); err != nil {
			writeGroupInternalError(response, "validate group", err)
			return
		}
		groups = append(groups, group)
	}
	if err := rows.Err(); err != nil {
		writeGroupInternalError(response, "read groups", err)
		return
	}
	writeJSON(response, http.StatusOK, GroupListResponse{
		Kind:   listQuery.kind,
		Page:   listQuery.page,
		Size:   listQuery.size,
		Total:  total,
		Groups: groups,
	})
}

func (handlers *GroupHandlers) handleDetail(
	response http.ResponseWriter,
	request *http.Request,
) {
	id, err := parsePositiveInt64(request.PathValue("id"))
	if err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{
			"error": "group id must be a positive decimal integer",
		})
		return
	}
	pagination, err := parseGroupMemberPagination(request)
	if err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{
			"error": err.Error(),
		})
		return
	}
	if handlers == nil || handlers.db == nil {
		writeGroupUnavailable(response)
		return
	}

	detail := GroupDetail{Members: make([]GroupMember, 0)}
	err = handlers.db.QueryRow(request.Context(), `
		SELECT
			g.id,
			g.kind,
			effective_representative.id
		FROM dup_groups AS g
		LEFT JOIN LATERAL (
			SELECT effective_files.id
			FROM dup_members AS effective_members
			JOIN files AS effective_files
			  ON effective_files.id=effective_members.file_id
			WHERE effective_members.group_id=g.id
			  AND effective_files.status <> 'deleted'
			ORDER BY
			  CASE WHEN effective_files.id=g.representative_file_id THEN 0 ELSE 1 END,
			  effective_files.machine_id,effective_files.path,effective_files.id
			LIMIT 1
		) AS effective_representative ON true
		WHERE g.id=$1
		  AND g.kind IN ('exact','image','video')`,
		id,
	).Scan(
		&detail.ID,
		&detail.Kind,
		&detail.RepresentativeFileID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(response, http.StatusNotFound, map[string]string{
			"error": "group not found",
		})
		return
	}
	if err != nil {
		writeGroupInternalError(response, "query group detail", err)
		return
	}
	if detail.ID != id || !validDisplayedGroupKind(detail.Kind) {
		writeGroupInternalError(
			response,
			"validate group detail",
			fmt.Errorf("invalid returned group identity id=%d kind=%q",
				detail.ID, detail.Kind),
		)
		return
	}

	var representativeArg any
	if detail.RepresentativeFileID != nil {
		representativeArg = *detail.RepresentativeFileID
	}
	if pagination.enabled {
		if err := handlers.db.QueryRow(request.Context(), `
			SELECT count(*)
			FROM dup_members AS m
			JOIN files AS f ON f.id=m.file_id
			WHERE m.group_id=$1
			  AND f.status <> 'deleted'`, id,
		).Scan(&detail.MemberTotal); err != nil {
			writeGroupInternalError(response, "count group members", err)
			return
		}
		if detail.MemberTotal < 0 {
			writeGroupInternalError(
				response,
				"count group members",
				fmt.Errorf("negative group member count %d", detail.MemberTotal),
			)
			return
		}
		detail.MemberPage = pagination.page
		detail.MemberSize = pagination.size
	}
	memberSQL := `
		SELECT f.id,f.machine_id,f.path,f.size,f.mtime,m.score_json
		FROM dup_members AS m
		JOIN files AS f ON f.id=m.file_id
		WHERE m.group_id=$1
		  AND f.status <> 'deleted'
		ORDER BY
		  CASE WHEN m.file_id=$2 THEN 0 ELSE 1 END,
		  f.machine_id,f.path,f.id`
	memberArgs := []any{id, representativeArg}
	if pagination.enabled {
		memberSQL += "\n\t\tLIMIT $3 OFFSET $4"
		memberArgs = append(memberArgs, pagination.size, pagination.offset)
	}
	rows, err := handlers.db.Query(request.Context(), memberSQL, memberArgs...)
	if err != nil {
		writeGroupInternalError(response, "query group members", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var (
			member GroupMember
			raw    []byte
		)
		if err := rows.Scan(
			&member.FileID,
			&member.MachineID,
			&member.Path,
			&member.Size,
			&member.MTime,
			&raw,
		); err != nil {
			writeGroupInternalError(response, "scan group member", err)
			return
		}
		if member.FileID <= 0 {
			writeGroupInternalError(
				response,
				"validate group member",
				fmt.Errorf("invalid file id %d", member.FileID),
			)
			return
		}
		if len(raw) == 0 {
			raw = []byte("null")
		}
		if !json.Valid(raw) {
			writeGroupInternalError(
				response,
				"validate group member score",
				fmt.Errorf("file %d has corrupt score JSON", member.FileID),
			)
			return
		}
		member.ScoreJSON = append(json.RawMessage(nil), raw...)
		detail.Members = append(detail.Members, member)
	}
	if err := rows.Err(); err != nil {
		writeGroupInternalError(response, "read group members", err)
		return
	}
	if !pagination.enabled {
		detail.MemberTotal = int64(len(detail.Members))
	}
	writeJSON(response, http.StatusOK, detail)
}

func (handlers *GroupHandlers) handlePage(
	response http.ResponseWriter,
	_ *http.Request,
) {
	body, err := fs.ReadFile(webFS(), "groups.html")
	if err != nil {
		http.Error(response, "groups page unavailable", http.StatusInternalServerError)
		return
	}
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(body)
}

func validDisplayedGroupKind(kind string) bool {
	return kind == "exact" || kind == "image" || kind == "video"
}

func parsePositiveDecimal(raw string, defaultValue int) (int, error) {
	if raw == "" {
		return defaultValue, nil
	}
	for _, char := range raw {
		if char < '0' || char > '9' {
			return 0, fmt.Errorf("not decimal")
		}
	}
	value, err := strconv.ParseUint(raw, 10, 31)
	if err != nil || value == 0 {
		return 0, fmt.Errorf("not positive")
	}
	return int(value), nil
}

func parsePositiveInt64(raw string) (int64, error) {
	if raw == "" {
		return 0, fmt.Errorf("empty")
	}
	for _, char := range raw {
		if char < '0' || char > '9' {
			return 0, fmt.Errorf("not decimal")
		}
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("not positive")
	}
	return value, nil
}

func parseGroupListQuery(request *http.Request) (groupListQuery, error) {
	values := request.URL.Query()
	result := groupListQuery{
		kind:    values.Get("kind"),
		query:   values.Get("q"),
		machine: values.Get("machine"),
		sort:    values.Get("sort"),
	}
	if !validDisplayedGroupKind(result.kind) {
		return groupListQuery{}, fmt.Errorf("kind must be exact, image, or video")
	}
	var err error
	result.page, err = parsePositiveDecimal(values.Get("page"), 1)
	if err != nil {
		return groupListQuery{}, fmt.Errorf("page must be a positive decimal integer")
	}
	result.size, err = parsePositiveDecimal(values.Get("size"), 50)
	if err != nil || result.size > 500 {
		return groupListQuery{}, fmt.Errorf("size must be a decimal integer in 1..500")
	}
	if utf8.RuneCountInString(result.query) > 256 {
		return groupListQuery{}, fmt.Errorf("q must be at most 256 Unicode code points")
	}
	if utf8.RuneCountInString(result.machine) > 128 {
		return groupListQuery{}, fmt.Errorf("machine must be at most 128 Unicode code points")
	}
	if raw := values.Get("min_members"); raw != "" {
		result.minMembers, err = parsePositiveInt64(raw)
		if err != nil {
			return groupListQuery{}, fmt.Errorf("min_members must be a positive decimal integer")
		}
	}
	if result.sort == "" {
		result.sort = groupSortMembers
	}
	if _, err := groupListOrderBy(result.sort); err != nil {
		return groupListQuery{}, fmt.Errorf("sort must be members_desc, newest, or reclaim_desc")
	}
	return result, nil
}

func groupListOrderBy(sortName string) (string, error) {
	switch sortName {
	case groupSortMembers:
		return "summary.live_member_count DESC,g.id", nil
	case groupSortNewest:
		return "g.created_at DESC,g.id", nil
	case groupSortReclaim:
		return "summary.wasted_bytes DESC,g.id", nil
	default:
		return "", fmt.Errorf("unknown group sort %q", sortName)
	}
}

func groupPageOrderBy(sortName string) (string, error) {
	switch sortName {
	case groupSortMembers:
		return "g.live_member_count DESC,g.id", nil
	case groupSortNewest:
		return "g.created_at DESC,g.id", nil
	case groupSortReclaim:
		return "g.wasted_bytes DESC,g.id", nil
	default:
		return "", fmt.Errorf("unknown paged group sort %q", sortName)
	}
}

func parseGroupMemberPagination(
	request *http.Request,
) (groupMemberPagination, error) {
	values := request.URL.Query()
	_, hasPage := values["member_page"]
	_, hasSize := values["member_size"]
	if !hasPage && !hasSize {
		return groupMemberPagination{}, nil
	}
	if !hasPage || !hasSize {
		return groupMemberPagination{}, fmt.Errorf("member_page and member_size must be provided together")
	}
	page, err := parsePositiveInt64(values.Get("member_page"))
	if err != nil {
		return groupMemberPagination{}, fmt.Errorf("member_page must be a positive decimal integer")
	}
	size, err := parsePositiveDecimal(values.Get("member_size"), 0)
	if err != nil || size > 500 {
		return groupMemberPagination{}, fmt.Errorf("member_size must be a decimal integer in 1..500")
	}
	maxInt64 := int64(^uint64(0) >> 1)
	if page-1 > maxInt64/int64(size) {
		return groupMemberPagination{}, fmt.Errorf("member_page and member_size produce an invalid offset")
	}
	offset := (page - 1) * int64(size)
	maxInt := int64(^uint(0) >> 1)
	if page > maxInt || offset > maxInt {
		return groupMemberPagination{}, fmt.Errorf("member_page and member_size produce an invalid offset")
	}
	return groupMemberPagination{
		enabled: true,
		page:    int(page),
		size:    size,
		offset:  int(offset),
	}, nil
}

func validateGroupSummary(group GroupSummary, requestedKind string) error {
	if group.ID <= 0 || group.Kind != requestedKind ||
		!validDisplayedGroupKind(group.Kind) ||
		group.MemberCount <= 0 ||
		group.TotalBytes < 0 || group.WastedBytes < 0 ||
		group.WastedBytes > group.TotalBytes ||
		len(group.Machines) == 0 {
		return fmt.Errorf("invalid group summary %#v", group)
	}
	if !sort.StringsAreSorted(group.Machines) {
		return fmt.Errorf("group %d machines are not sorted", group.ID)
	}
	for index := 1; index < len(group.Machines); index++ {
		if group.Machines[index-1] == group.Machines[index] {
			return fmt.Errorf("group %d machines are not distinct", group.ID)
		}
	}
	return nil
}

func writeGroupUnavailable(response http.ResponseWriter) {
	writeJSON(response, http.StatusServiceUnavailable, map[string]string{
		"error": "central database unavailable",
	})
}

func writeGroupInternalError(
	response http.ResponseWriter,
	operation string,
	err error,
) {
	writeJSON(response, http.StatusInternalServerError, map[string]string{
		"error": fmt.Sprintf("%s: %v", operation, err),
	})
}
