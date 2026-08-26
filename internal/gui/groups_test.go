package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestGroupsListValidatesKindAndPaginationBeforeDatabase(t *testing.T) {
	api := newGroupTestAPI(&groupAPIFakeDB{panicOnUse: true})
	for _, test := range []struct {
		name string
		url  string
	}{
		{"missing kind", "/api/groups"},
		{"candidate kind", "/api/groups?kind=image_candidate"},
		{"case changed kind", "/api/groups?kind=Image"},
		{"page zero", "/api/groups?kind=image&page=0"},
		{"page negative", "/api/groups?kind=image&page=-1"},
		{"page sign", "/api/groups?kind=image&page=%2B1"},
		{"page text", "/api/groups?kind=image&page=one"},
		{"page overflow", "/api/groups?kind=image&page=999999999999999999999999"},
		{"size zero", "/api/groups?kind=image&size=0"},
		{"size above max", "/api/groups?kind=image&size=501"},
		{"size sign", "/api/groups?kind=image&size=%2B50"},
		{"size text", "/api/groups?kind=image&size=fifty"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := groupRequest(t, api, test.url)
			assertGroupJSONError(t, response, http.StatusBadRequest)
		})
	}
}

func TestGroupsListDefaultsPaginatesAndPreservesInjectionAsData(t *testing.T) {
	createdA := time.Date(2026, 7, 28, 10, 11, 12, 0, time.UTC)
	createdB := createdA.Add(time.Minute)
	injection := `</script><img src=x onerror=alert(1)>`
	db := &groupAPIFakeDB{
		rowResults: []groupAPIRowResult{{values: []any{int64(2)}}},
		rowsResults: []*groupAPIRows{groupAPIRowsFrom(
			[]any{int64(8), "image", int64(4), int64(400), int64(300), injection, `D:\z.jpg`,
				[]string{injection, "machine-a"}, createdA},
			[]any{int64(9), "image", int64(2), int64(200), int64(100), "machine-b", injection,
				[]string{"machine-b"}, createdB},
		)},
	}
	api := newGroupTestAPI(db)

	response := groupRequest(t, api, "/api/groups?kind=image")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var got GroupListResponse
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Kind != "image" || got.Page != 1 || got.Size != 50 ||
		got.Total != 2 || len(got.Groups) != 2 {
		t.Fatalf("response=%#v", got)
	}
	if got.Groups[0].ID != 8 || got.Groups[0].MemberCount != 4 ||
		got.Groups[0].RepMachine != injection ||
		!reflect.DeepEqual(got.Groups[0].Machines,
			[]string{injection, "machine-a"}) {
		t.Fatalf("first group=%#v", got.Groups[0])
	}
	if strings.Contains(response.Body.String(), injection) {
		t.Fatal("json.Encoder did not HTML-escape injection string")
	}
	if len(db.rowCalls) != 1 || len(db.queryCalls) != 1 {
		t.Fatalf("query calls row=%d rows=%d", len(db.rowCalls), len(db.queryCalls))
	}
	if gotArgs := db.queryCalls[0].args; !reflect.DeepEqual(
		gotArgs,
		[]any{"image", "", "", int64(0), 50, 0},
	) {
		t.Fatalf("list args=%#v", gotArgs)
	}
	sql := normalizedGroupSQL(db.queryCalls[0].sql)
	for _, fragment := range []string{
		"f.status <> 'deleted'",
		"FROM dup_members AS effective_members",
		"effective_members.group_id=g.id",
		"CASE WHEN effective_files.id=g.representative_file_id THEN 0 ELSE 1 END",
		"effective_files.machine_id,effective_files.path,effective_files.id",
		"ORDER BY summary.live_member_count DESC,g.id",
		"array_agg(DISTINCT",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("list SQL missing %q:\n%s", fragment, sql)
		}
	}
	pageGroups := strings.Index(sql, "page_groups AS MATERIALIZED")
	pageLimit := strings.Index(sql, "LIMIT $5 OFFSET $6")
	representativeLookup := strings.Index(sql, "JOIN LATERAL")
	if pageGroups < 0 || pageLimit < pageGroups ||
		representativeLookup < 0 || pageLimit > representativeLookup {
		t.Fatalf("list SQL must page before effective representative lookup:\n%s", sql)
	}
}

func TestGroupsListAcceptsBoundariesAndComputesOffset(t *testing.T) {
	db := &groupAPIFakeDB{
		rowResults:  []groupAPIRowResult{{values: []any{int64(0)}}},
		rowsResults: []*groupAPIRows{groupAPIRowsFrom()},
	}
	api := newGroupTestAPI(db)
	response := groupRequest(
		t,
		api,
		"/api/groups?kind=video&page=3&size=500",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var got GroupListResponse
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Page != 3 || got.Size != 500 || got.Total != 0 ||
		got.Groups == nil || len(got.Groups) != 0 {
		t.Fatalf("response=%#v", got)
	}
	if args := db.queryCalls[0].args; !reflect.DeepEqual(
		args,
		[]any{"video", "", "", int64(0), 500, 1000},
	) {
		t.Fatalf("list args=%#v", args)
	}
}

func TestGroupsListFilterScaleContract(t *testing.T) {
	created := time.Date(2026, 7, 31, 9, 10, 11, 0, time.UTC)
	for _, test := range []struct {
		name    string
		sort    string
		orderBy string
	}{
		{"reclaim", "reclaim_desc", "ORDER BY summary.wasted_bytes DESC,g.id"},
		{"newest", "newest", "ORDER BY g.created_at DESC,g.id"},
		{"members", "members_desc", "ORDER BY summary.live_member_count DESC,g.id"},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := &groupAPIFakeDB{
				rowResults: []groupAPIRowResult{{values: []any{int64(1)}}},
				rowsResults: []*groupAPIRows{groupAPIRowsFrom(
					[]any{int64(2481), "image", int64(3), int64(3000), int64(2000), "agent-a", `D:\\poster.jpg`,
						[]string{"agent-a", "agent-b"}, created},
				)},
			}
			response := groupRequest(t, newGroupTestAPI(db),
				"/api/groups?kind=image&page=2&size=100&q=poster&machine=agent-a&min_members=3&sort="+test.sort)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if gotArgs := db.queryCalls[0].args; !reflect.DeepEqual(
				gotArgs,
				[]any{"image", "agent-a", "poster", int64(3), 100, 100},
			) {
				t.Fatalf("list args=%#v", gotArgs)
			}
			var got struct {
				Groups []struct {
					TotalBytes  int64 `json:"total_bytes"`
					WastedBytes int64 `json:"wasted_bytes"`
				} `json:"groups"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Groups) != 1 || got.Groups[0].TotalBytes != 3000 ||
				got.Groups[0].WastedBytes != 2000 {
				t.Fatalf("byte totals=%#v", got.Groups)
			}

			sql := normalizedGroupSQL(db.queryCalls[0].sql)
			for _, fragment := range []string{
				"all_live AS", "matching_groups AS", test.orderBy,
			} {
				if !strings.Contains(sql, fragment) {
					t.Fatalf("list SQL missing %q:\n%s", fragment, sql)
				}
			}
		})
	}
}

func TestGroupsListFilterRejectsInvalidInputBeforeDatabase(t *testing.T) {
	api := newGroupTestAPI(&groupAPIFakeDB{panicOnUse: true})
	for _, test := range []struct {
		name string
		url  string
	}{
		{"query longer than 256 runes", "/api/groups?kind=image&q=" + strings.Repeat("界", 257)},
		{"machine longer than 128 runes", "/api/groups?kind=image&machine=" + strings.Repeat("机", 129)},
		{"minimum members zero", "/api/groups?kind=image&min_members=0"},
		{"minimum members signed", "/api/groups?kind=image&min_members=%2B3"},
		{"minimum members non decimal", "/api/groups?kind=image&min_members=three"},
		{"minimum members overflows", "/api/groups?kind=image&min_members=999999999999999999999999"},
		{"unknown sort", "/api/groups?kind=image&sort=path_asc"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := groupRequest(t, api, test.url)
			assertGroupJSONError(t, response, http.StatusBadRequest)
		})
	}
}

func TestGroupsStatsValidatesFiltersBeforeDatabase(t *testing.T) {
	api := newGroupTestAPI(&groupAPIFakeDB{panicOnUse: true})
	for _, test := range []struct {
		name string
		url  string
	}{
		{"candidate kind", "/api/groups/stats?kind=image_candidate"},
		{"case changed kind", "/api/groups/stats?kind=Image"},
		{"query longer than 256 runes", "/api/groups/stats?kind=image&q=" + strings.Repeat("界", 257)},
		{"machine longer than 128 runes", "/api/groups/stats?kind=image&machine=" + strings.Repeat("机", 129)},
		{"minimum members zero", "/api/groups/stats?kind=image&min_members=0"},
		{"minimum members signed", "/api/groups/stats?kind=image&min_members=%2B3"},
		{"minimum members non decimal", "/api/groups/stats?kind=image&min_members=three"},
		{"minimum members overflows", "/api/groups/stats?kind=image&min_members=999999999999999999999999"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := groupRequest(t, api, test.url)
			assertGroupJSONError(t, response, http.StatusBadRequest)
		})
	}
}

func TestGroupsStatsUnavailableWithoutDatabase(t *testing.T) {
	// A 503 (not 400) proves /api/groups/stats routed to handleStats: the
	// {id} detail route would reject "stats" as a non-numeric id first.
	response := groupRequest(t, NewAPI(nil, nil, nil), "/api/groups/stats?kind=exact")
	assertGroupJSONError(t, response, http.StatusServiceUnavailable)
	response = groupRequest(t, NewAPI(nil, nil, nil), "/api/groups/stats")
	assertGroupJSONError(t, response, http.StatusServiceUnavailable)
}

func TestGroupsStatsAggregatesOverLiveFilter(t *testing.T) {
	db := &groupAPIFakeDB{
		rowResults: []groupAPIRowResult{{values: []any{int64(3), int64(9000), int64(5000)}}},
	}
	api := newGroupTestAPI(db)
	response := groupRequest(
		t,
		api,
		"/api/groups/stats?kind=image&q=poster&machine=agent-a&min_members=2",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var got GroupStatsResponse
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Kind != "image" || got.Groups != 3 ||
		got.TotalBytes != 9000 || got.WastedBytes != 5000 {
		t.Fatalf("stats=%#v", got)
	}
	if len(db.rowCalls) != 1 || len(db.queryCalls) != 0 {
		t.Fatalf("query calls row=%d rows=%d", len(db.rowCalls), len(db.queryCalls))
	}
	if gotArgs := db.rowCalls[0].args; !reflect.DeepEqual(
		gotArgs,
		[]any{"image", "agent-a", "poster", int64(2)},
	) {
		t.Fatalf("stats args=%#v", gotArgs)
	}
	sql := normalizedGroupSQL(db.rowCalls[0].sql)
	for _, fragment := range []string{
		"all_live AS",
		"matching_groups AS",
		"f.status <> 'deleted'",
		"GREATEST(sum(size)-max(size),0) AS wasted_bytes",
		"COALESCE(sum(summary.total_bytes),0)",
		"COALESCE(sum(summary.wasted_bytes),0)",
		"summary.live_member_count >= $4",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("stats SQL missing %q:\n%s", fragment, sql)
		}
	}
}

func TestGroupsStatsEmptySetReturnsZeroes(t *testing.T) {
	db := &groupAPIFakeDB{
		rowResults: []groupAPIRowResult{{values: []any{int64(0), int64(0), int64(0)}}},
	}
	api := newGroupTestAPI(db)
	response := groupRequest(t, api, "/api/groups/stats")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var got GroupStatsResponse
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Kind != "" || got.Groups != 0 || got.TotalBytes != 0 || got.WastedBytes != 0 {
		t.Fatalf("empty stats=%#v", got)
	}
	if gotArgs := db.rowCalls[0].args; !reflect.DeepEqual(
		gotArgs,
		[]any{"", "", "", int64(0)},
	) {
		t.Fatalf("stats args=%#v", gotArgs)
	}
	if sql := normalizedGroupSQL(db.rowCalls[0].sql); !strings.Contains(
		sql, "kind IN ('exact','image','video')",
	) {
		t.Fatalf("stats SQL does not restrict all-kinds aggregation to display kinds:\n%s", sql)
	}
}

func TestGroupsDetailValidatesIDAndReportsMissing(t *testing.T) {
	invalidAPI := newGroupTestAPI(&groupAPIFakeDB{panicOnUse: true})
	for _, id := range []string{"0", "-1", "+1", "abc", "1.5", "999999999999999999999999"} {
		response := groupRequest(t, invalidAPI, "/api/groups/"+id)
		assertGroupJSONError(t, response, http.StatusBadRequest)
	}

	missingAPI := newGroupTestAPI(&groupAPIFakeDB{
		rowResults: []groupAPIRowResult{{err: pgx.ErrNoRows}},
	})
	response := groupRequest(t, missingAPI, "/api/groups/123")
	assertGroupJSONError(t, response, http.StatusNotFound)
	if sql := normalizedGroupSQL(
		missingAPI.groups.db.(*groupAPIFakeDB).rowCalls[0].sql,
	); !strings.Contains(sql, "kind IN ('exact','image','video')") {
		t.Fatalf("detail SQL does not hide candidate kinds:\n%s", sql)
	}

	candidateAPI := newGroupTestAPI(&groupAPIFakeDB{
		// PostgreSQL returns ErrNoRows because the ID belongs to
		// image_candidate and the handler query filters display kinds.
		rowResults: []groupAPIRowResult{{err: pgx.ErrNoRows}},
	})
	response = groupRequest(t, candidateAPI, "/api/groups/456")
	assertGroupJSONError(t, response, http.StatusNotFound)
}

func TestGroupsDetailPreservesObjectArrayNullAndOrdersRepresentativeFirst(t *testing.T) {
	injection := `</script><img src=x onerror=alert(1)>`
	repID := int64(9)
	db := &groupAPIFakeDB{
		rowResults: []groupAPIRowResult{{
			values: []any{int64(123), "image", &repID},
		}},
		rowsResults: []*groupAPIRows{groupAPIRowsFrom(
			[]any{int64(9), "machine-a", `D:\rep.jpg`, int64(42), int64(7),
				[]byte(`{"role":"representative"}`)},
			[]any{int64(10), injection, injection, int64(43), int64(8),
				[]byte(`["score","` + injection + `"]`)},
			[]any{int64(11), "machine-c", `C:\null.jpg`, int64(44), int64(9),
				[]byte(nil)},
		)},
	}
	api := newGroupTestAPI(db)
	response := groupRequest(t, api, "/api/groups/123")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var got GroupDetail
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != 123 || got.Kind != "image" ||
		got.RepresentativeFileID == nil || *got.RepresentativeFileID != 9 ||
		len(got.Members) != 3 {
		t.Fatalf("detail=%#v", got)
	}
	var object map[string]any
	if err := json.Unmarshal(got.Members[0].ScoreJSON, &object); err != nil ||
		object["role"] != "representative" {
		t.Fatalf("representative score=%s err=%v", got.Members[0].ScoreJSON, err)
	}
	var array []any
	if err := json.Unmarshal(got.Members[1].ScoreJSON, &array); err != nil ||
		len(array) != 2 || array[1] != injection {
		t.Fatalf("array score=%s err=%v", got.Members[1].ScoreJSON, err)
	}
	if string(got.Members[2].ScoreJSON) != "null" {
		t.Fatalf("null score=%s", got.Members[2].ScoreJSON)
	}
	if strings.Contains(response.Body.String(), injection) {
		t.Fatal("detail JSON did not HTML-escape injection data")
	}
	detailSQL := normalizedGroupSQL(db.rowCalls[0].sql)
	for _, fragment := range []string{
		"FROM dup_members AS effective_members",
		"effective_members.group_id=g.id",
		"CASE WHEN effective_files.id=g.representative_file_id THEN 0 ELSE 1 END",
	} {
		if !strings.Contains(detailSQL, fragment) {
			t.Fatalf("detail identity SQL missing %q:\n%s", fragment, detailSQL)
		}
	}
	memberSQL := normalizedGroupSQL(db.queryCalls[0].sql)
	for _, fragment := range []string{
		"f.status <> 'deleted'",
		"CASE WHEN m.file_id=$2 THEN 0 ELSE 1 END",
		"f.machine_id,f.path,f.id",
	} {
		if !strings.Contains(memberSQL, fragment) {
			t.Fatalf("detail member SQL missing %q:\n%s", fragment, memberSQL)
		}
	}
}

func TestGroupsDetailMemberPaginationScaleContract(t *testing.T) {
	representativeID := int64(9)
	db := &groupAPIFakeDB{
		rowResults: []groupAPIRowResult{
			{values: []any{int64(2481), "image", &representativeID}},
			{values: []any{int64(248)}},
		},
		rowsResults: []*groupAPIRows{groupAPIRowsFrom(
			[]any{int64(109), "agent-b", `D:\\member.jpg`, int64(1000), int64(7), []byte(`null`)},
		)},
	}
	response := groupRequest(t, newGroupTestAPI(db),
		"/api/groups/2481?member_page=2&member_size=100")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var got GroupDetail
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.MemberTotal != 248 || got.MemberPage != 2 || got.MemberSize != 100 {
		t.Fatalf("member paging=%#v", got)
	}
	if len(db.rowCalls) != 2 {
		t.Fatalf("row calls=%d", len(db.rowCalls))
	}
	if gotArgs := db.queryCalls[0].args; !reflect.DeepEqual(
		gotArgs,
		[]any{int64(2481), int64(9), 100, 100},
	) {
		t.Fatalf("member args=%#v", gotArgs)
	}
	if sql := normalizedGroupSQL(db.queryCalls[0].sql); !strings.Contains(
		sql, "LIMIT $3 OFFSET $4",
	) {
		t.Fatalf("member pagination SQL=%s", sql)
	}
}

func TestGroupsDetailMemberPaginationPreservesAllMembersWhenAbsent(t *testing.T) {
	db := &groupAPIFakeDB{
		rowResults: []groupAPIRowResult{{
			values: []any{int64(17), "video", (*int64)(nil)},
		}},
		rowsResults: []*groupAPIRows{groupAPIRowsFrom(
			[]any{int64(2), "agent-a", `D:\\first.mp4`, int64(2000), int64(1), []byte(`null`)},
			[]any{int64(3), "agent-b", `D:\\second.mp4`, int64(3000), int64(2), []byte(`null`)},
		)},
	}
	response := groupRequest(t, newGroupTestAPI(db), "/api/groups/17")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var got GroupDetail
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.MemberTotal != 2 || got.MemberPage != 0 || got.MemberSize != 0 ||
		len(got.Members) != 2 {
		t.Fatalf("unpaged detail=%#v", got)
	}
	if len(db.rowCalls) != 1 {
		t.Fatalf("unpaged row calls=%d", len(db.rowCalls))
	}
	if sql := normalizedGroupSQL(db.queryCalls[0].sql); strings.Contains(sql, "LIMIT") ||
		strings.Contains(sql, "OFFSET") {
		t.Fatalf("unpaged member SQL=%s", sql)
	}
}

func TestGroupsDetailMemberPaginationValidatesBeforeDatabase(t *testing.T) {
	api := newGroupTestAPI(&groupAPIFakeDB{panicOnUse: true})
	for _, test := range []struct {
		name string
		url  string
	}{
		{"page without size", "/api/groups/1?member_page=1"},
		{"size without page", "/api/groups/1?member_size=100"},
		{"page zero", "/api/groups/1?member_page=0&member_size=100"},
		{"page signed", "/api/groups/1?member_page=%2B1&member_size=100"},
		{"size zero", "/api/groups/1?member_page=1&member_size=0"},
		{"size empty", "/api/groups/1?member_page=1&member_size="},
		{"size above max", "/api/groups/1?member_page=1&member_size=501"},
		{"offset overflows", "/api/groups/1?member_page=9223372036854775807&member_size=500"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := groupRequest(t, api, test.url)
			assertGroupJSONError(t, response, http.StatusBadRequest)
		})
	}
}

func TestGroupsFailClosedWithoutPartialSuccess(t *testing.T) {
	tests := []struct {
		name string
		db   *groupAPIFakeDB
		url  string
	}{
		{
			name: "list count query",
			db: &groupAPIFakeDB{
				rowResults: []groupAPIRowResult{{err: errors.New("count failed")}},
			},
			url: "/api/groups?kind=exact",
		},
		{
			name: "list query",
			db: &groupAPIFakeDB{
				rowResults:  []groupAPIRowResult{{values: []any{int64(1)}}},
				queryErrors: []error{errors.New("query failed")},
			},
			url: "/api/groups?kind=image",
		},
		{
			name: "list scan",
			db: &groupAPIFakeDB{
				rowResults: []groupAPIRowResult{{values: []any{int64(1)}}},
				rowsResults: []*groupAPIRows{{
					scans:   []func([]any) error{func([]any) error { return nil }},
					scanErr: errors.New("scan failed"),
				}},
			},
			url: "/api/groups?kind=video",
		},
		{
			name: "list rows",
			db: &groupAPIFakeDB{
				rowResults: []groupAPIRowResult{{values: []any{int64(0)}}},
				rowsResults: []*groupAPIRows{{
					rowsErr: errors.New("rows failed"),
				}},
			},
			url: "/api/groups?kind=exact",
		},
		{
			name: "stats query",
			db: &groupAPIFakeDB{
				rowResults: []groupAPIRowResult{{err: errors.New("stats failed")}},
			},
			url: "/api/groups/stats?kind=image",
		},
		{
			name: "detail invalid returned kind",
			db: &groupAPIFakeDB{
				rowResults: []groupAPIRowResult{{
					values: []any{int64(1), "image_candidate", (*int64)(nil)},
				}},
				rowsResults: []*groupAPIRows{groupAPIRowsFrom()},
			},
			url: "/api/groups/1",
		},
		{
			name: "detail corrupt score",
			db: &groupAPIFakeDB{
				rowResults: []groupAPIRowResult{{
					values: []any{int64(1), "image", (*int64)(nil)},
				}},
				rowsResults: []*groupAPIRows{groupAPIRowsFrom(
					[]any{int64(3), "m", "p", int64(1), int64(1),
						[]byte(`{"broken":`)},
				)},
			},
			url: "/api/groups/1",
		},
		{
			name: "detail rows",
			db: &groupAPIFakeDB{
				rowResults: []groupAPIRowResult{{
					values: []any{int64(1), "image", (*int64)(nil)},
				}},
				rowsResults: []*groupAPIRows{{rowsErr: errors.New("rows failed")}},
			},
			url: "/api/groups/1",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := groupRequest(t, newGroupTestAPI(test.db), test.url)
			assertGroupJSONError(t, response, http.StatusInternalServerError)
			var body map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if len(body) != 1 {
				t.Fatalf("partial success body=%v", body)
			}
		})
	}
}

func TestGroupsSetRepresentativeValidatesBeforeDatabase(t *testing.T) {
	api := newGroupTestAPI(&groupAPIFakeDB{panicOnUse: true})
	for _, test := range []struct {
		name string
		url  string
		body string
	}{
		{"zero group id", "/api/groups/0/representative", `{"file_id":7}`},
		{"non-numeric group id", "/api/groups/abc/representative", `{"file_id":7}`},
		{"malformed json", "/api/groups/5/representative", `{"file_id":`},
		{"unknown field", "/api/groups/5/representative", `{"file_id":7,"extra":1}`},
		{"trailing data", "/api/groups/5/representative", `{"file_id":7} {}`},
		{"zero file id", "/api/groups/5/representative", `{"file_id":0}`},
		{"negative file id", "/api/groups/5/representative", `{"file_id":-3}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := groupPostRequest(t, api, test.url, test.body)
			assertGroupJSONError(t, response, http.StatusBadRequest)
		})
	}
	// 无库时 503 而不是 404：证明路由命中且校验已通过。
	response := groupPostRequest(t, NewAPI(nil, nil, nil), "/api/groups/5/representative", `{"file_id":7}`)
	assertGroupJSONError(t, response, http.StatusServiceUnavailable)
}

func TestGroupsSetRepresentativeLookupFailures(t *testing.T) {
	for _, test := range []struct {
		name string
		db   *groupAPIFakeDB
		want int
	}{
		{
			name: "group missing",
			db: &groupAPIFakeDB{
				rowResults: []groupAPIRowResult{{err: pgx.ErrNoRows}},
			},
			want: http.StatusNotFound,
		},
		{
			name: "file missing",
			db: &groupAPIFakeDB{
				rowResults: []groupAPIRowResult{{values: []any{1}}, {err: pgx.ErrNoRows}},
			},
			want: http.StatusNotFound,
		},
		{
			name: "not a live member",
			db: &groupAPIFakeDB{
				rowResults: []groupAPIRowResult{
					{values: []any{1}}, {values: []any{1}}, {err: pgx.ErrNoRows},
				},
			},
			want: http.StatusBadRequest,
		},
		{
			name: "group vanished before update",
			db: &groupAPIFakeDB{
				rowResults: []groupAPIRowResult{
					{values: []any{1}}, {values: []any{1}}, {values: []any{1}},
					{err: pgx.ErrNoRows},
				},
			},
			want: http.StatusNotFound,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := groupPostRequest(
				t, newGroupTestAPI(test.db), "/api/groups/5/representative", `{"file_id":7}`,
			)
			assertGroupJSONError(t, response, test.want)
		})
	}
}

func TestGroupsSetRepresentativeUpdatesStoredRepresentative(t *testing.T) {
	db := &groupAPIFakeDB{
		rowResults: []groupAPIRowResult{
			{values: []any{1}}, {values: []any{1}}, {values: []any{1}},
			{values: []any{int64(5)}},
		},
	}
	response := groupPostRequest(
		t, newGroupTestAPI(db), "/api/groups/5/representative", `{"file_id":7}`,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body map[string]bool
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body["ok"] {
		t.Fatalf("body=%v", body)
	}
	if len(db.rowCalls) != 4 {
		t.Fatalf("row calls=%d", len(db.rowCalls))
	}
	updateSQL := normalizedGroupSQL(db.rowCalls[3].sql)
	if !strings.Contains(updateSQL, "UPDATE dup_groups") ||
		!strings.Contains(updateSQL, "representative_file_id=$2") {
		t.Fatalf("update SQL=%s", updateSQL)
	}
	if args := db.rowCalls[3].args; !reflect.DeepEqual(args, []any{int64(5), int64(7)}) {
		t.Fatalf("update args=%#v", args)
	}
	memberSQL := normalizedGroupSQL(db.rowCalls[2].sql)
	if !strings.Contains(memberSQL, "f.status <> 'deleted'") {
		t.Fatalf("membership SQL does not exclude deleted files:\n%s", memberSQL)
	}
}

func TestGroupsSelectByStrategyValidatesBeforeDatabase(t *testing.T) {
	api := newGroupTestAPI(&groupAPIFakeDB{panicOnUse: true})
	for _, test := range []struct {
		name string
		body string
	}{
		{"malformed json", `{"kind":`},
		{"unknown field", `{"kind":"exact","strategy":"newest","extra":1}`},
		{"missing kind", `{"strategy":"newest"}`},
		{"candidate kind", `{"kind":"image_candidate","strategy":"newest"}`},
		{"missing strategy", `{"kind":"exact"}`},
		{"unknown strategy", `{"kind":"exact","strategy":"smallest"}`},
		{"query too long", `{"kind":"exact","strategy":"newest","q":"` + strings.Repeat("界", 257) + `"}`},
		{"machine too long", `{"kind":"exact","strategy":"newest","machine":"` + strings.Repeat("机", 129) + `"}`},
		{"negative min members", `{"kind":"exact","strategy":"newest","min_members":-1}`},
		{"negative limit", `{"kind":"exact","strategy":"newest","limit":-1}`},
		{"limit above max", `{"kind":"exact","strategy":"newest","limit":50001}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := groupPostRequest(t, api, "/api/groups/select-by-strategy", test.body)
			assertGroupJSONError(t, response, http.StatusBadRequest)
		})
	}
	response := groupPostRequest(
		t, NewAPI(nil, nil, nil), "/api/groups/select-by-strategy",
		`{"kind":"exact","strategy":"newest"}`,
	)
	assertGroupJSONError(t, response, http.StatusServiceUnavailable)
}

func TestGroupsSelectByStrategyQueriesAndProtectsRepresentative(t *testing.T) {
	for _, test := range []struct {
		name      string
		strategy  string
		orderFrag string
	}{
		{"newest", "newest", "ORDER BY mtime DESC, id"},
		{"oldest", "oldest", "ORDER BY mtime ASC, id"},
		{"largest", "largest", "ORDER BY size DESC, id"},
		{"shortest path", "shortest_path", "ORDER BY length(path) ASC, id"},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := &groupAPIFakeDB{
				rowResults: []groupAPIRowResult{{values: []any{int64(2)}}},
				rowsResults: []*groupAPIRows{groupAPIRowsFrom(
					[]any{int64(11), int64(3)},
					[]any{int64(12), int64(3)},
				)},
			}
			response := groupPostRequest(
				t, newGroupTestAPI(db), "/api/groups/select-by-strategy",
				`{"kind":"image","q":"poster","machine":"agent-a","min_members":2,"strategy":"`+test.strategy+`","limit":10}`,
			)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var got GroupSelectByStrategyResponse
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Groups != 2 || len(got.FileIDs) != 2 ||
				got.FileIDs[0] != 11 || got.FileIDs[1] != 12 || got.Truncated {
				t.Fatalf("selection=%#v", got)
			}
			if len(db.rowCalls) != 1 || len(db.queryCalls) != 1 {
				t.Fatalf("calls row=%d query=%d", len(db.rowCalls), len(db.queryCalls))
			}
			wantArgs := []any{"image", "agent-a", "poster", int64(2)}
			if !reflect.DeepEqual(db.rowCalls[0].args, wantArgs) {
				t.Fatalf("count args=%#v", db.rowCalls[0].args)
			}
			if !reflect.DeepEqual(
				db.queryCalls[0].args,
				[]any{"image", "agent-a", "poster", int64(2), 10},
			) {
				t.Fatalf("select args=%#v", db.queryCalls[0].args)
			}
			sql := normalizedGroupSQL(db.queryCalls[0].sql)
			for _, fragment := range []string{
				"all_live AS",
				"matching_groups AS",
				"f.status <> 'deleted'",
				"summary.live_member_count >= $4",
				// effective 代表与策略保留者都被排除在选择之外
				"CASE WHEN id=representative_file_id THEN 0 ELSE 1 END",
				"representative_rank <> 1",
				"keep_rank <> 1",
				test.orderFrag,
				"LIMIT $5",
			} {
				if !strings.Contains(sql, fragment) {
					t.Fatalf("strategy SQL missing %q:\n%s", fragment, sql)
				}
			}
		})
	}
}

func TestGroupsSelectByStrategyDefaultsTruncatesAndStaysFailClosed(t *testing.T) {
	t.Run("default limit and truncation", func(t *testing.T) {
		db := &groupAPIFakeDB{
			rowResults: []groupAPIRowResult{{values: []any{int64(1)}}},
			rowsResults: []*groupAPIRows{groupAPIRowsFrom(
				[]any{int64(11), int64(50001)},
			)},
		}
		response := groupPostRequest(
			t, newGroupTestAPI(db), "/api/groups/select-by-strategy",
			`{"kind":"exact","strategy":"oldest"}`,
		)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		var got GroupSelectByStrategyResponse
		if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if !got.Truncated || len(got.FileIDs) != 1 {
			t.Fatalf("selection=%#v", got)
		}
		if args := db.queryCalls[0].args; args[4] != groupSelectStrategyMaxLimit {
			t.Fatalf("default limit arg=%#v", args[4])
		}
	})

	t.Run("empty selection stays a JSON array", func(t *testing.T) {
		db := &groupAPIFakeDB{
			rowResults:  []groupAPIRowResult{{values: []any{int64(0)}}},
			rowsResults: []*groupAPIRows{groupAPIRowsFrom()},
		}
		response := groupPostRequest(
			t, newGroupTestAPI(db), "/api/groups/select-by-strategy",
			`{"kind":"video","strategy":"largest"}`,
		)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		if !strings.Contains(response.Body.String(), `"file_ids":[]`) {
			t.Fatalf("empty selection not an array: %s", response.Body.String())
		}
	})

	t.Run("count failure", func(t *testing.T) {
		db := &groupAPIFakeDB{
			rowResults: []groupAPIRowResult{{err: errors.New("count failed")}},
		}
		response := groupPostRequest(
			t, newGroupTestAPI(db), "/api/groups/select-by-strategy",
			`{"kind":"exact","strategy":"newest"}`,
		)
		assertGroupJSONError(t, response, http.StatusInternalServerError)
	})

	t.Run("selection failure", func(t *testing.T) {
		db := &groupAPIFakeDB{
			rowResults:  []groupAPIRowResult{{values: []any{int64(1)}}},
			queryErrors: []error{errors.New("query failed")},
		}
		response := groupPostRequest(
			t, newGroupTestAPI(db), "/api/groups/select-by-strategy",
			`{"kind":"exact","strategy":"newest"}`,
		)
		assertGroupJSONError(t, response, http.StatusInternalServerError)
	})
}

func TestGroupsDedicatedCompatibilityEntryServesReactHTML(t *testing.T) {
	api := NewAPI(nil, nil, nil)
	response := groupRequest(t, api, "/groups")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/html") {
		t.Fatalf("Content-Type=%q", contentType)
	}
	if !strings.Contains(response.Body.String(), `id="root"`) {
		t.Fatalf("groups compatibility entry is not React HTML: %s", response.Body.String())
	}
	if location := response.Header().Get("Location"); location != "" {
		t.Fatalf("groups compatibility entry redirected to %q", location)
	}
}

func TestPostgresGroupsDeletedRepresentativeFallbackAndLiveFilteringWhenEnabled(
	t *testing.T,
) {
	dsn := os.Getenv("DEDUP_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("DEDUP_TEST_PG_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()

	schema := "task11_groups_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatal(err)
	}

	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	scoped, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		scoped.Close()
		if _, dropErr := admin.Exec(
			context.Background(),
			"DROP SCHEMA "+quotedSchema+" CASCADE",
		); dropErr != nil {
			t.Errorf("drop schema: %v", dropErr)
			return
		}
		var residual int
		if auditErr := admin.QueryRow(
			context.Background(),
			`SELECT count(*) FROM pg_namespace WHERE nspname=$1`,
			schema,
		).Scan(&residual); auditErr != nil {
			t.Errorf("audit schema cleanup: %v", auditErr)
		} else if residual != 0 {
			t.Errorf("schema cleanup residual=%d", residual)
		}
	}()

	centralSQL, err := os.ReadFile(filepath.Join("..", "..", "deploy", "central.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scoped.Exec(ctx, string(centralSQL)); err != nil {
		t.Fatal(err)
	}

	insertFile := func(machine, path, status string) int64 {
		t.Helper()
		var id int64
		if err := scoped.QueryRow(
			ctx,
			`INSERT INTO files(machine_id,path,size,mtime,status)
			 VALUES($1,$2,100,1,$3) RETURNING id`,
			machine, path, status,
		).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	insertGroup := func(kind string, representative int64, members ...int64) int64 {
		t.Helper()
		var groupID int64
		if err := scoped.QueryRow(
			ctx,
			`INSERT INTO dup_groups(kind,representative_file_id,member_count)
			 VALUES($1,$2,$3) RETURNING id`,
			kind, representative, len(members),
		).Scan(&groupID); err != nil {
			t.Fatal(err)
		}
		for index, fileID := range members {
			if _, err := scoped.Exec(
				ctx,
				`INSERT INTO dup_members(group_id,file_id,score_json)
				 VALUES($1,$2,$3::jsonb)`,
				groupID, fileID, fmt.Sprintf(`{"rank":%d}`, index),
			); err != nil {
				t.Fatal(err)
			}
		}
		return groupID
	}

	deletedRepresentative := insertFile("z-deleted", `Z:\deleted.bin`, "deleted")
	exactFallback := insertFile("a-live", `A:\first.bin`, "done")
	exactSecond := insertFile("b-live", `B:\second.bin`, "done")
	exactID := insertGroup(
		"exact",
		deletedRepresentative,
		deletedRepresentative,
		exactSecond,
		exactFallback,
	)

	imageDeletedA := insertFile("a-image", `A:\deleted.jpg`, "deleted")
	imageDeletedB := insertFile("b-image", `B:\deleted.jpg`, "deleted")
	insertGroup("image", imageDeletedA, imageDeletedA, imageDeletedB)

	videoOther := insertFile("a-video", `A:\other.mp4`, "done")
	videoRepresentative := insertFile("z-video", `Z:\representative.mp4`, "done")
	videoID := insertGroup(
		"video",
		videoRepresentative,
		videoOther,
		videoRepresentative,
	)

	candidateFile := insertFile("candidate", `C:\candidate.jpg`, "done")
	candidateID := insertGroup(
		"image_candidate",
		candidateFile,
		candidateFile,
	)

	api := NewAPI(nil, nil, scoped)
	exactResponse := groupRequest(t, api, "/api/groups?kind=exact")
	if exactResponse.Code != http.StatusOK {
		t.Fatalf("exact list status=%d body=%s",
			exactResponse.Code, exactResponse.Body.String())
	}
	var exactList GroupListResponse
	if err := json.Unmarshal(exactResponse.Body.Bytes(), &exactList); err != nil {
		t.Fatal(err)
	}
	if exactList.Total != 1 || len(exactList.Groups) != 1 {
		t.Fatalf("exact list=%#v", exactList)
	}
	exact := exactList.Groups[0]
	if exact.ID != exactID || exact.MemberCount != 2 ||
		exact.RepMachine != "a-live" || exact.RepPath != `A:\first.bin` ||
		!reflect.DeepEqual(exact.Machines, []string{"a-live", "b-live"}) {
		t.Fatalf("exact summary=%#v", exact)
	}

	exactDetailResponse := groupRequest(
		t, api, fmt.Sprintf("/api/groups/%d", exactID),
	)
	if exactDetailResponse.Code != http.StatusOK {
		t.Fatalf("exact detail status=%d body=%s",
			exactDetailResponse.Code, exactDetailResponse.Body.String())
	}
	var exactDetail GroupDetail
	if err := json.Unmarshal(exactDetailResponse.Body.Bytes(), &exactDetail); err != nil {
		t.Fatal(err)
	}
	if exactDetail.RepresentativeFileID == nil ||
		*exactDetail.RepresentativeFileID != exactFallback ||
		len(exactDetail.Members) != 2 ||
		exactDetail.Members[0].FileID != exactFallback ||
		exactDetail.Members[1].FileID != exactSecond {
		t.Fatalf("exact detail=%#v", exactDetail)
	}

	imageResponse := groupRequest(t, api, "/api/groups?kind=image")
	if imageResponse.Code != http.StatusOK {
		t.Fatalf("image list status=%d body=%s",
			imageResponse.Code, imageResponse.Body.String())
	}
	var imageList GroupListResponse
	if err := json.Unmarshal(imageResponse.Body.Bytes(), &imageList); err != nil {
		t.Fatal(err)
	}
	if imageList.Total != 0 || len(imageList.Groups) != 0 ||
		imageList.Groups == nil {
		t.Fatalf("all-deleted image group leaked: %#v", imageList)
	}

	videoResponse := groupRequest(t, api, "/api/groups?kind=video")
	if videoResponse.Code != http.StatusOK {
		t.Fatalf("video list status=%d body=%s",
			videoResponse.Code, videoResponse.Body.String())
	}
	var videoList GroupListResponse
	if err := json.Unmarshal(videoResponse.Body.Bytes(), &videoList); err != nil {
		t.Fatal(err)
	}
	if videoList.Total != 1 || len(videoList.Groups) != 1 ||
		videoList.Groups[0].RepMachine != "z-video" {
		t.Fatalf("stored live representative not preserved: %#v", videoList)
	}
	videoDetailResponse := groupRequest(
		t, api, fmt.Sprintf("/api/groups/%d", videoID),
	)
	if videoDetailResponse.Code != http.StatusOK {
		t.Fatalf("video detail status=%d body=%s",
			videoDetailResponse.Code, videoDetailResponse.Body.String())
	}
	var videoDetail GroupDetail
	if err := json.Unmarshal(videoDetailResponse.Body.Bytes(), &videoDetail); err != nil {
		t.Fatal(err)
	}
	if len(videoDetail.Members) != 2 ||
		videoDetail.Members[0].FileID != videoRepresentative ||
		videoDetail.Members[1].FileID != videoOther {
		t.Fatalf("stored representative not ordered first: %#v", videoDetail)
	}

	candidateResponse := groupRequest(
		t, api, fmt.Sprintf("/api/groups/%d", candidateID),
	)
	assertGroupJSONError(t, candidateResponse, http.StatusNotFound)

	stats := func(url string) GroupStatsResponse {
		t.Helper()
		response := groupRequest(t, api, url)
		if response.Code != http.StatusOK {
			t.Fatalf("stats %s status=%d body=%s",
				url, response.Code, response.Body.String())
		}
		var got GroupStatsResponse
		if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := stats("/api/groups/stats?kind=exact"); got.Kind != "exact" ||
		got.Groups != 1 || got.TotalBytes != 200 || got.WastedBytes != 100 {
		t.Fatalf("exact stats=%#v", got)
	}
	if got := stats("/api/groups/stats?kind=image"); got.Groups != 0 ||
		got.TotalBytes != 0 || got.WastedBytes != 0 {
		t.Fatalf("all-deleted image stats leaked: %#v", got)
	}
	if got := stats("/api/groups/stats"); got.Kind != "" ||
		got.Groups != 2 || got.TotalBytes != 400 || got.WastedBytes != 200 {
		t.Fatalf("all-kinds stats=%#v (candidate kinds must stay hidden)", got)
	}
	if got := stats("/api/groups/stats?machine=a-live"); got.Groups != 1 ||
		got.TotalBytes != 200 || got.WastedBytes != 100 {
		t.Fatalf("machine-filtered stats=%#v", got)
	}
	if got := stats("/api/groups/stats?q=first"); got.Groups != 1 ||
		got.TotalBytes != 200 || got.WastedBytes != 100 {
		t.Fatalf("path-filtered stats=%#v", got)
	}
	if got := stats("/api/groups/stats?kind=exact&min_members=3"); got.Groups != 0 ||
		got.TotalBytes != 0 || got.WastedBytes != 0 {
		t.Fatalf("min_members stats=%#v", got)
	}
}

func TestPostgresGroupRepresentativeAndStrategySelectionWhenEnabled(t *testing.T) {
	dsn := os.Getenv("DEDUP_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("DEDUP_TEST_PG_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()

	schema := "task_batch3_groups_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	scoped, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		scoped.Close()
		if _, dropErr := admin.Exec(
			context.Background(),
			"DROP SCHEMA "+quotedSchema+" CASCADE",
		); dropErr != nil {
			t.Errorf("drop schema: %v", dropErr)
		}
	}()

	centralSQL, err := os.ReadFile(filepath.Join("..", "..", "deploy", "central.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scoped.Exec(ctx, string(centralSQL)); err != nil {
		t.Fatal(err)
	}

	insertFile := func(machine, path string, size, mtime int64, status string) int64 {
		t.Helper()
		var id int64
		if err := scoped.QueryRow(
			ctx,
			`INSERT INTO files(machine_id,path,size,mtime,status)
			 VALUES($1,$2,$3,$4,$5) RETURNING id`,
			machine, path, size, mtime, status,
		).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}

	keep := insertFile("a-live", `A:\keep.bin`, 100, 10, "done")
	newer := insertFile("b-live", `B:\newer.bin`, 200, 20, "done")
	old := insertFile("a-live", `A:\old.bin`, 50, 5, "done")
	deleted := insertFile("a-live", `A:\deleted.bin`, 60, 6, "deleted")
	other := insertFile("a-video", `A:\other.mp4`, 300, 30, "done")
	videoRep := insertFile("z-video", `Z:\representative.mp4`, 300, 30, "done")

	var groupID int64
	if err := scoped.QueryRow(
		ctx,
		`INSERT INTO dup_groups(kind,representative_file_id,member_count)
		 VALUES('exact',$1,4) RETURNING id`,
		keep,
	).Scan(&groupID); err != nil {
		t.Fatal(err)
	}
	for _, fileID := range []int64{keep, newer, old, deleted} {
		if _, err := scoped.Exec(
			ctx,
			`INSERT INTO dup_members(group_id,file_id) VALUES($1,$2)`,
			groupID, fileID,
		); err != nil {
			t.Fatal(err)
		}
	}
	var videoGroupID int64
	if err := scoped.QueryRow(
		ctx,
		`INSERT INTO dup_groups(kind,representative_file_id,member_count)
		 VALUES('video',$1,2) RETURNING id`,
		videoRep,
	).Scan(&videoGroupID); err != nil {
		t.Fatal(err)
	}
	for _, fileID := range []int64{other, videoRep} {
		if _, err := scoped.Exec(
			ctx,
			`INSERT INTO dup_members(group_id,file_id) VALUES($1,$2)`,
			videoGroupID, fileID,
		); err != nil {
			t.Fatal(err)
		}
	}

	api := NewAPI(nil, nil, scoped)
	strategyURL := "/api/groups/select-by-strategy"
	selectByStrategy := func(body string) GroupSelectByStrategyResponse {
		t.Helper()
		response := groupPostRequest(t, api, strategyURL, body)
		if response.Code != http.StatusOK {
			t.Fatalf("select-by-strategy %s status=%d body=%s",
				body, response.Code, response.Body.String())
		}
		var got GroupSelectByStrategyResponse
		if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	// newest：保留 mtime 最大者（newer）与代表（keep），选中 old；deleted 不参与。
	if got := selectByStrategy(`{"kind":"exact","strategy":"newest"}`); got.Groups != 1 ||
		got.Truncated || len(got.FileIDs) != 1 || got.FileIDs[0] != old {
		t.Fatalf("newest selection=%#v", got)
	}
	// oldest：保留 mtime 最小者（old）与代表（keep），选中 newer。
	if got := selectByStrategy(`{"kind":"exact","strategy":"oldest"}`); len(got.FileIDs) != 1 ||
		got.FileIDs[0] != newer {
		t.Fatalf("oldest selection=%#v", got)
	}
	// shortest_path：A:\old.bin 最短保留，选中 newer。
	if got := selectByStrategy(`{"kind":"exact","strategy":"shortest_path"}`); len(got.FileIDs) != 1 ||
		got.FileIDs[0] != newer {
		t.Fatalf("shortest_path selection=%#v", got)
	}
	// 筛选透传：machine/q/min_members 与 kind 共同作用。
	if got := selectByStrategy(
		`{"kind":"exact","strategy":"newest","machine":"b-live"}`,
	); got.Groups != 1 || len(got.FileIDs) != 1 {
		t.Fatalf("machine-filtered selection=%#v", got)
	}
	if got := selectByStrategy(
		`{"kind":"exact","strategy":"newest","min_members":5}`,
	); got.Groups != 0 || len(got.FileIDs) != 0 {
		t.Fatalf("min_members selection=%#v", got)
	}
	// 代表与策略保留者并列保护：video 组策略保留者 other（并列取小 id），
	// 代表 videoRep 同受保护，无可选成员。
	if got := selectByStrategy(`{"kind":"video","strategy":"largest"}`); got.Groups != 1 ||
		len(got.FileIDs) != 0 {
		t.Fatalf("video selection=%#v", got)
	}

	// 指定保留副本的错误分支。
	representativeURL := func(id int64) string {
		return fmt.Sprintf("/api/groups/%d/representative", id)
	}
	if response := groupPostRequest(
		t, api, representativeURL(999999), fmt.Sprintf(`{"file_id":%d}`, old),
	); response.Code != http.StatusNotFound {
		t.Fatalf("unknown group status=%d", response.Code)
	}
	if response := groupPostRequest(
		t, api, representativeURL(groupID), `{"file_id":999999}`,
	); response.Code != http.StatusNotFound {
		t.Fatalf("unknown file status=%d", response.Code)
	}
	if response := groupPostRequest(
		t, api, representativeURL(groupID), fmt.Sprintf(`{"file_id":%d}`, other),
	); response.Code != http.StatusBadRequest {
		t.Fatalf("foreign member status=%d", response.Code)
	}
	if response := groupPostRequest(
		t, api, representativeURL(groupID), fmt.Sprintf(`{"file_id":%d}`, deleted),
	); response.Code != http.StatusBadRequest {
		t.Fatalf("deleted member status=%d", response.Code)
	}

	// 成功路径：指定 old 为保留副本后，详情代表变更且策略选择不再选中 old。
	response := groupPostRequest(
		t, api, representativeURL(groupID), fmt.Sprintf(`{"file_id":%d}`, old),
	)
	if response.Code != http.StatusOK {
		t.Fatalf("set representative status=%d body=%s", response.Code, response.Body.String())
	}
	detailResponse := groupRequest(t, api, fmt.Sprintf("/api/groups/%d", groupID))
	if detailResponse.Code != http.StatusOK {
		t.Fatalf("detail status=%d", detailResponse.Code)
	}
	var detail GroupDetail
	if err := json.Unmarshal(detailResponse.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.RepresentativeFileID == nil || *detail.RepresentativeFileID != old {
		t.Fatalf("representative after update=%#v", detail.RepresentativeFileID)
	}
	// newest 保留 newer，代表 old 受保护，选中 keep。
	if got := selectByStrategy(`{"kind":"exact","strategy":"newest"}`); len(got.FileIDs) != 1 ||
		got.FileIDs[0] != keep {
		t.Fatalf("selection after representative change=%#v", got)
	}
}

func newGroupTestAPI(db groupQueryDB) *API {
	api := NewAPI(nil, nil, nil)
	api.groups = NewGroupHandlers(db)
	return api
}

func groupRequest(t *testing.T, api *API, url string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, url, nil)
	response := httptest.NewRecorder()
	api.Routes().ServeHTTP(response, request)
	return response
}

func groupPostRequest(
	t *testing.T,
	api *API,
	url string,
	body string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, url, strings.NewReader(body))
	response := httptest.NewRecorder()
	api.Routes().ServeHTTP(response, request)
	return response
}

func assertGroupJSONError(
	t *testing.T,
	response *httptest.ResponseRecorder,
	status int,
) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status=%d body=%s, want %d",
			response.Code, response.Body.String(), status)
	}
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("Content-Type=%q", contentType)
	}
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"] == "" {
		t.Fatalf("error body=%v", body)
	}
}

type groupAPICall struct {
	sql  string
	args []any
}

type groupAPIFakeDB struct {
	panicOnUse bool

	rowResults  []groupAPIRowResult
	rowsResults []*groupAPIRows
	queryErrors []error

	rowCalls   []groupAPICall
	queryCalls []groupAPICall
}

func (db *groupAPIFakeDB) QueryRow(
	_ context.Context,
	sql string,
	args ...any,
) pgx.Row {
	if db.panicOnUse {
		panic("group database used before request validation")
	}
	db.rowCalls = append(db.rowCalls, groupAPICall{
		sql: sql, args: append([]any(nil), args...),
	})
	if len(db.rowResults) == 0 {
		return groupAPIRow{err: errors.New("unexpected QueryRow")}
	}
	result := db.rowResults[0]
	db.rowResults = db.rowResults[1:]
	return groupAPIRow(result)
}

func (db *groupAPIFakeDB) Query(
	_ context.Context,
	sql string,
	args ...any,
) (pgx.Rows, error) {
	if db.panicOnUse {
		panic("group database used before request validation")
	}
	db.queryCalls = append(db.queryCalls, groupAPICall{
		sql: sql, args: append([]any(nil), args...),
	})
	if len(db.queryErrors) > 0 {
		err := db.queryErrors[0]
		db.queryErrors = db.queryErrors[1:]
		if err != nil {
			return nil, err
		}
	}
	if len(db.rowsResults) == 0 {
		return nil, errors.New("unexpected Query")
	}
	rows := db.rowsResults[0]
	db.rowsResults = db.rowsResults[1:]
	return rows, nil
}

type groupAPIRowResult struct {
	values []any
	err    error
}

type groupAPIRow groupAPIRowResult

func (row groupAPIRow) Scan(dest ...any) error {
	if row.err != nil {
		return row.err
	}
	return assignGroupScan(dest, row.values)
}

type groupAPIRows struct {
	pgx.Rows
	scans   []func([]any) error
	index   int
	scanErr error
	rowsErr error
}

func groupAPIRowsFrom(rows ...[]any) *groupAPIRows {
	result := &groupAPIRows{}
	for _, values := range rows {
		values := append([]any(nil), values...)
		result.scans = append(result.scans, func(dest []any) error {
			return assignGroupScan(dest, values)
		})
	}
	return result
}

func (rows *groupAPIRows) Next() bool { return rows.index < len(rows.scans) }

func (rows *groupAPIRows) Scan(dest ...any) error {
	if rows.scanErr != nil {
		return rows.scanErr
	}
	err := rows.scans[rows.index](dest)
	rows.index++
	return err
}

func (rows *groupAPIRows) Err() error { return rows.rowsErr }
func (rows *groupAPIRows) Close()     {}

func assignGroupScan(dest, values []any) error {
	if len(dest) != len(values) {
		return fmt.Errorf("scan destinations=%d values=%d", len(dest), len(values))
	}
	for index, value := range values {
		switch target := dest[index].(type) {
		case *int:
			*target = value.(int)
		case *int64:
			*target = value.(int64)
		case *string:
			*target = value.(string)
		case **int64:
			if value == nil {
				*target = nil
			} else {
				source := value.(*int64)
				if source == nil {
					*target = nil
				} else {
					cloned := *source
					*target = &cloned
				}
			}
		case *[]string:
			*target = append([]string(nil), value.([]string)...)
		case *[]byte:
			*target = append([]byte(nil), value.([]byte)...)
		case *time.Time:
			*target = value.(time.Time)
		default:
			return fmt.Errorf("unsupported scan target %T", dest[index])
		}
	}
	return nil
}

func normalizedGroupSQL(sql string) string {
	return strings.Join(strings.Fields(sql), " ")
}
