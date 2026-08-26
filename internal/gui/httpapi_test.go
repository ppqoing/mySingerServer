package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"path"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"

	"dedup/internal/config"
	"dedup/internal/proto"
)

type fakeFilesystemBrowseService struct {
	response proto.FilesystemBrowseResponse
	err      error
	machine  string
	request  proto.FilesystemBrowseRequest
}

func (service *fakeFilesystemBrowseService) Browse(
	_ context.Context,
	machineID string,
	request proto.FilesystemBrowseRequest,
) (proto.FilesystemBrowseResponse, error) {
	service.machine = machineID
	service.request = request
	return service.response, service.err
}

func TestFilesystemBrowseHTTPUsesBodyPathAndReturnsFilesDisabled(t *testing.T) {
	service := &fakeFilesystemBrowseService{response: proto.FilesystemBrowseResponse{
		CurrentPath: `D:\Media`,
		Entries: []proto.FilesystemEntry{{
			Name: "cover.jpg", Path: `D:\Media\cover.jpg`,
			Kind: proto.FilesystemEntryFile, Selectable: false,
		}},
	}}
	api := NewAPI(nil, nil, nil)
	api.SetFilesystemBrowser(service)
	request := httptest.NewRequest(http.MethodPost, "/api/agents/machine-a/filesystem/browse?path=E%3A%5CIgnored", strings.NewReader(`{"path":"D:\\Media","show_hidden":false,"limit":200}`))
	response := httptest.NewRecorder()
	api.Routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if service.machine != "machine-a" || service.request.Path != `D:\Media` {
		t.Fatalf("browse=%q %#v", service.machine, service.request)
	}
	var body struct {
		CurrentPath string `json:"current_path"`
		Entries     []struct {
			Name string `json:"name"`
			Path string `json:"path"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.CurrentPath != `D:\Media` || len(body.Entries) != 1 || body.Entries[0].Path != `D:\Media\cover.jpg` {
		t.Fatalf("response=%s", response.Body.String())
	}
}

func TestFilesystemBrowseHTTPIncludesEmptyNavigationFields(t *testing.T) {
	api := NewAPI(nil, nil, nil)
	api.SetFilesystemBrowser(&fakeFilesystemBrowseService{})
	request := httptest.NewRequest(http.MethodPost, "/api/agents/machine-a/filesystem/browse", strings.NewReader(`{"path":""}`))
	response := httptest.NewRecorder()
	api.Routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"current_path", "parent_path", "next_cursor"} {
		if got, exists := body[field]; !exists || string(got) != `""` {
			t.Fatalf("%s=%s exists=%t body=%s", field, got, exists, response.Body.String())
		}
	}
}

func TestFilesystemBrowseHTTPRejectsUnknownJSONField(t *testing.T) {
	api := NewAPI(nil, nil, nil)
	api.SetFilesystemBrowser(&fakeFilesystemBrowseService{})
	response := httptest.NewRecorder()
	api.Routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/agents/machine-a/filesystem/browse", strings.NewReader(`{"path":"D:\\Media","unexpected":true}`)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestFilesystemBrowseHTTPMapsServiceFailures(t *testing.T) {
	tests := []struct {
		name     string
		service  *fakeFilesystemBrowseService
		wantCode int
	}{
		{"offline", &fakeFilesystemBrowseService{err: ErrFilesystemAgentOffline}, http.StatusServiceUnavailable},
		{"timeout", &fakeFilesystemBrowseService{err: context.DeadlineExceeded}, http.StatusGatewayTimeout},
		{"access denied", &fakeFilesystemBrowseService{response: proto.FilesystemBrowseResponse{ErrorCode: "access_denied"}}, http.StatusForbidden},
		{"path not found", &fakeFilesystemBrowseService{response: proto.FilesystemBrowseResponse{ErrorCode: "path_not_found"}}, http.StatusNotFound},
		{"files disabled", &fakeFilesystemBrowseService{response: proto.FilesystemBrowseResponse{ErrorCode: "files_disabled"}}, http.StatusServiceUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := NewAPI(nil, nil, nil)
			api.SetFilesystemBrowser(test.service)
			response := httptest.NewRecorder()
			api.Routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/agents/machine-a/filesystem/browse", strings.NewReader(`{"path":"D:\\Media"}`)))
			if response.Code != test.wantCode {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestFilesystemBrowseHTTPReturnsServiceUnavailableWhenNotConfigured(t *testing.T) {
	api := NewAPI(nil, nil, nil)
	response := httptest.NewRecorder()
	api.Routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/agents/machine-a/filesystem/browse", strings.NewReader(`{"path":"D:\\Media"}`)))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestFilesystemBrowseHTTPDoesNotExposePathInValidationErrors(t *testing.T) {
	api := NewAPI(nil, nil, nil)
	api.SetFilesystemBrowser(&fakeFilesystemBrowseService{err: errors.New("unused")})
	response := httptest.NewRecorder()
	api.Routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/agents/machine-a/filesystem/browse", strings.NewReader(`{"path":"relative-sensitive-path"}`)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "relative-sensitive-path") {
		t.Fatalf("validation error leaked path: %s", response.Body.String())
	}
}

var embeddedStaticTag = regexp.MustCompile(
	`(?is)<\s*(script|link)\b[^>]*>`,
)

var embeddedStaticAttribute = regexp.MustCompile(
	`(?is)\b([a-z][a-z0-9:_-]*)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'=<>]+))`,
)

var embeddedHTTPURL = regexp.MustCompile(`(?i)https?://`)
var embeddedCSSURL = regexp.MustCompile(
	`(?is)url\(\s*(?:"([^"]*)"|'([^']*)'|([^'")]*))\s*\)`,
)
var embeddedCSSImport = regexp.MustCompile(`(?i)@import\b`)

type embeddedStaticReference struct {
	kind string
	url  string
}

func TestEmbeddedHTMLReferenceParserEnumeratesOnlyScriptsAndStylesheets(t *testing.T) {
	html := `
		<script src="//cdn.example/remote.js"></script>
		<SCRIPT SRC="HTTPS://cdn.example/upper.js"></SCRIPT>
		<script src="./extra.js"></script>
		<img src="/assets/impostor.js">
		<a href="/assets/impostor.css">download</a>
		<script src="/assets/app.js"></script>
		<link rel="stylesheet" href="/assets/app.css">
	`
	references := embeddedScriptAndStylesheetReferences(html)
	got := make([]string, 0, len(references))
	for _, reference := range references {
		got = append(got, reference.kind+":"+reference.url)
	}
	want := []string{
		"script://cdn.example/remote.js",
		"script:HTTPS://cdn.example/upper.js",
		"script:./extra.js",
		"script:/assets/app.js",
		"stylesheet:/assets/app.css",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("references=%q, want scripts/styles only %q", got, want)
	}
}

func TestEmbeddedHTMLAssetValidationRejectsEveryInvalidReference(t *testing.T) {
	assets := fstest.MapFS{
		"assets/app.js":    {Data: []byte("console.log('ok')")},
		"assets/app.css":   {Data: []byte("body{}")},
		"assets/empty.css": {Data: nil},
	}
	validScript := `<script src="/assets/app.js"></script>`
	validStyle := `<link rel="stylesheet" href="/assets/app.css">`
	tests := []struct {
		name string
		html string
	}{
		{"protocol relative script", `<script src="//cdn.example/app.js"></script>` + validStyle},
		{"uppercase https script", `<SCRIPT SRC="HTTPS://cdn.example/app.js"></SCRIPT>` + validStyle},
		{"relative script", `<script src="./extra.js"></script>` + validStyle},
		{"script must be javascript", `<script src="/assets/app.css"></script>` + validStyle},
		{"stylesheet must be css", validScript + `<link rel="stylesheet" href="/assets/app.js">`},
		{"referenced script must exist", `<script src="/assets/missing.js"></script>` + validStyle},
		{"referenced stylesheet must be nonempty", validScript + `<link rel="stylesheet" href="/assets/empty.css">`},
		{"React entry cannot contain a plain remote URL", validScript + validStyle + `<p>HTTPS://example.invalid/</p>`},
		{"images and anchors cannot impersonate assets", `
			<img src="/assets/app.js">
			<a href="/assets/app.css">download</a>
		`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateEmbeddedHTMLAssets(assets, test.html, "synthetic.html"); err == nil {
				t.Fatal("invalid script/stylesheet references were accepted")
			}
		})
	}

	validWithImpostors := `
		<img src="/assets/missing.js">
		<a href="/assets/missing.css">download</a>
	` + validScript + validStyle
	if err := validateEmbeddedHTMLAssets(assets, validWithImpostors, "valid.html"); err != nil {
		t.Fatalf("valid script/stylesheet references rejected: %v", err)
	}

	for _, test := range []struct {
		name   string
		assets fstest.MapFS
	}{
		{
			name: "stylesheet missing nested asset",
			assets: fstest.MapFS{
				"assets/app.js":  {Data: []byte("console.log('ok')")},
				"assets/app.css": {Data: []byte(`body{background:url("/assets/missing.png")}`)},
			},
		},
		{
			name: "stylesheet remote nested asset",
			assets: fstest.MapFS{
				"assets/app.js":  {Data: []byte("console.log('ok')")},
				"assets/app.css": {Data: []byte(`body{background:url(https://example.invalid/remote.png)}`)},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateEmbeddedHTMLAssets(
				test.assets, validScript+validStyle, "nested.html",
			); err == nil {
				t.Fatal("invalid nested stylesheet asset was accepted")
			}
		})
	}

	nestedValid := fstest.MapFS{
		"assets/app.js":  {Data: []byte("console.log('ok')")},
		"assets/app.css": {Data: []byte(`body{background:url("/assets/pixel.png")}`)},
		"assets/pixel.png": {
			Data: []byte("synthetic image"),
		},
	}
	if err := validateEmbeddedHTMLAssets(
		nestedValid, validScript+validStyle, "nested-valid.html",
	); err != nil {
		t.Fatalf("valid nested stylesheet asset rejected: %v", err)
	}
}

func TestEmbeddedHTMLRemoteDependencyDetectionIsCaseInsensitive(t *testing.T) {
	for _, html := range []string{
		`<SCRIPT SRC="//cdn.example/app.js"></SCRIPT>`,
		`<link REL="stylesheet" HREF="HTTPS://cdn.example/app.css">`,
		`<script src="hTtP://cdn.example/app.js"></script>`,
	} {
		if !hasRemoteScriptOrStylesheet(html) {
			t.Fatalf("remote dependency was not detected in %s", html)
		}
	}
	if hasRemoteScriptOrStylesheet(`
		<p>Documentation: HTTPS://example.invalid/</p>
		<script src="/assets/app.js"></script>
		<link rel="stylesheet" href="/assets/app.css">
	`) {
		t.Fatal("plain text URL was treated as a remote script or stylesheet")
	}
}

func TestEmbeddedReactEntriesServeOnlyLocalAssets(t *testing.T) {
	api := NewAPI(nil, nil, nil)
	for _, route := range []string{"/", "/groups"} {
		request := httptest.NewRequest(http.MethodGet, route, nil)
		response := httptest.NewRecorder()
		api.Routes().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", route, response.Code, response.Body.String())
		}
		if !strings.Contains(response.Body.String(), `id="root"`) {
			t.Fatalf("%s is not a React entry: %s", route, response.Body.String())
		}
	}

	entries := []string{"index.html", "groups.html"}
	for _, entry := range entries {
		body := readEmbeddedStaticFile(t, entry)
		if err := validateEmbeddedHTMLAssets(webFS(), body, entry); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEmbeddedStaticPagesKeepLegacyFallbacksAndNoRemoteDependencies(t *testing.T) {
	api := NewAPI(nil, nil, nil)
	for _, route := range []string{"/legacy.html", "/legacy-groups.html"} {
		request := httptest.NewRequest(http.MethodGet, route, nil)
		response := httptest.NewRecorder()
		api.Routes().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", route, response.Code, response.Body.String())
		}
	}

	for _, page := range []string{
		"index.html", "groups.html", "legacy.html", "legacy-groups.html",
	} {
		body := readEmbeddedStaticFile(t, page)
		if hasRemoteScriptOrStylesheet(body) {
			t.Fatalf("embedded page %s references a remote script or stylesheet", page)
		}
	}
}

func embeddedScriptAndStylesheetReferences(html string) []embeddedStaticReference {
	tags := embeddedStaticTag.FindAllStringSubmatch(html, -1)
	references := make([]embeddedStaticReference, 0, len(tags))
	for _, tag := range tags {
		attributes := make(map[string]string)
		for _, attribute := range embeddedStaticAttribute.FindAllStringSubmatch(tag[0], -1) {
			value := attribute[2]
			if value == "" {
				value = attribute[3]
			}
			if value == "" {
				value = attribute[4]
			}
			attributes[strings.ToLower(attribute[1])] = value
		}

		switch strings.ToLower(tag[1]) {
		case "script":
			if url, ok := attributes["src"]; ok {
				references = append(references, embeddedStaticReference{
					kind: "script",
					url:  url,
				})
			}
		case "link":
			if !containsHTMLRelToken(attributes["rel"], "stylesheet") {
				continue
			}
			if url, ok := attributes["href"]; ok {
				references = append(references, embeddedStaticReference{
					kind: "stylesheet",
					url:  url,
				})
			}
		}
	}
	return references
}

func containsHTMLRelToken(value string, want string) bool {
	for _, token := range strings.Fields(value) {
		if strings.EqualFold(token, want) {
			return true
		}
	}
	return false
}

func validateEmbeddedHTMLAssets(fileSystem fs.FS, html string, entry string) error {
	if embeddedHTTPURL.MatchString(html) {
		return fmt.Errorf("%s contains an HTTP(S) URL", entry)
	}
	foundScript := false
	foundStylesheet := false
	for _, reference := range embeddedScriptAndStylesheetReferences(html) {
		url := strings.TrimSpace(reference.url)
		if !strings.HasPrefix(url, "/assets/") {
			return fmt.Errorf("%s has non-local %s URL %q", entry, reference.kind, reference.url)
		}
		switch reference.kind {
		case "script":
			if !strings.HasSuffix(strings.ToLower(url), ".js") {
				return fmt.Errorf("%s script URL is not JavaScript: %q", entry, reference.url)
			}
			foundScript = true
		case "stylesheet":
			if !strings.HasSuffix(strings.ToLower(url), ".css") {
				return fmt.Errorf("%s stylesheet URL is not CSS: %q", entry, reference.url)
			}
			foundStylesheet = true
		default:
			return fmt.Errorf("%s has unknown static reference kind %q", entry, reference.kind)
		}

		content, err := fs.ReadFile(fileSystem, strings.TrimPrefix(url, "/"))
		if err != nil {
			return fmt.Errorf("%s references unreadable %s %q: %w",
				entry, reference.kind, reference.url, err)
		}
		if len(content) == 0 {
			return fmt.Errorf("%s references empty %s %q", entry, reference.kind, reference.url)
		}
		if reference.kind == "stylesheet" {
			if err := validateEmbeddedCSSAssets(
				fileSystem, string(content), reference.url,
			); err != nil {
				return err
			}
		}
	}
	if !foundScript || !foundStylesheet {
		return fmt.Errorf("%s local assets: JavaScript=%t CSS=%t",
			entry, foundScript, foundStylesheet)
	}
	return nil
}

func validateEmbeddedCSSAssets(fileSystem fs.FS, css string, entry string) error {
	if embeddedCSSImport.MatchString(css) {
		return fmt.Errorf("%s contains a forbidden CSS @import", entry)
	}
	for _, match := range embeddedCSSURL.FindAllStringSubmatch(css, -1) {
		url := strings.TrimSpace(match[1])
		if url == "" {
			url = strings.TrimSpace(match[2])
		}
		if url == "" {
			url = strings.TrimSpace(match[3])
		}
		if strings.HasPrefix(url, "data:") || strings.HasPrefix(url, "#") {
			continue
		}
		if !strings.HasPrefix(url, "/assets/") ||
			strings.Contains(url, `\`) ||
			strings.Contains(url, "://") ||
			strings.HasPrefix(url, "//") {
			return fmt.Errorf("%s contains invalid CSS asset URL %q", entry, url)
		}
		clean := path.Clean(strings.TrimPrefix(url, "/"))
		if clean != strings.TrimPrefix(url, "/") ||
			!strings.HasPrefix(clean, "assets/") {
			return fmt.Errorf("%s CSS asset resolves outside assets: %q", entry, url)
		}
		content, err := fs.ReadFile(fileSystem, clean)
		if err != nil {
			return fmt.Errorf("%s references unreadable CSS asset %q: %w", entry, url, err)
		}
		if len(content) == 0 {
			return fmt.Errorf("%s references empty CSS asset %q", entry, url)
		}
	}
	return nil
}

func hasRemoteScriptOrStylesheet(html string) bool {
	for _, reference := range embeddedScriptAndStylesheetReferences(html) {
		url := strings.ToLower(strings.TrimSpace(reference.url))
		if strings.HasPrefix(url, "http://") ||
			strings.HasPrefix(url, "https://") ||
			strings.HasPrefix(url, "//") {
			return true
		}
	}
	return false
}

func readEmbeddedStaticFile(t *testing.T, name string) string {
	t.Helper()
	content, err := fs.ReadFile(webFS(), name)
	if err != nil {
		t.Fatalf("read embedded %s: %v", name, err)
	}
	return string(content)
}

func TestGroupsUnavailableDatabaseIs503AndLegacyRoutesRemainRegistered(t *testing.T) {
	api := NewAPI(nil, nil, nil)
	for _, url := range []string{
		"/api/groups?kind=exact",
		"/api/groups/1",
		"/api/dup_groups",
		"/api/dup_groups/" + strings.Repeat("a", 128),
	} {
		request := httptest.NewRequest(http.MethodGet, url, nil)
		response := httptest.NewRecorder()
		api.Routes().ServeHTTP(response, request)
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s status=%d body=%s",
				url, response.Code, response.Body.String())
		}
	}
}

func TestDeleteHTTPNewAPIPreservesLegacyRouteRegistration(t *testing.T) {
	api := NewAPI(nil, nil, nil)
	deleteResponse := deleteHTTPResponse(
		api,
		http.MethodPost,
		"/api/delete/prepare",
		"application/json",
		`{"member_ids":[1]}`,
	)
	if deleteResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("delete status=%d body=%s", deleteResponse.Code, deleteResponse.Body.String())
	}
	legacyResponse := deleteHTTPResponse(
		api,
		http.MethodGet,
		"/api/groups?kind=exact",
		"",
		"",
	)
	if legacyResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("legacy status=%d body=%s", legacyResponse.Code, legacyResponse.Body.String())
	}
}

func TestScanAPIReusesProvidedTaskIDForResume(t *testing.T) {
	serverSide, guiSide := net.Pipe()
	defer serverSide.Close()
	defer guiSide.Close()
	agent := &AgentConn{
		ep: config.AgentEndpoint{Addr: "pipe"}, conn: proto.NewConn(guiSide),
		machineID: machineA, identityState: IdentityClaimed, online: true,
	}
	pool := &Pool{
		byAddr:      map[string]*AgentConn{"pipe": agent},
		byMachineID: map[string]*AgentConn{machineA: agent},
	}
	registry := NewTaskRegistry(nil, testLogger())
	api := NewAPI(pool, registry, nil)

	received := make(chan proto.ScanTask, 1)
	go func() {
		msgType, body, err := proto.NewConn(serverSide).ReadFrame()
		if err != nil {
			return
		}
		message, err := proto.Decode(msgType, body)
		if err == nil {
			received <- *message.(*proto.ScanTask)
		}
	}()
	body := bytes.NewBufferString(`{
		"task_id":"b7b0ba1c-1ec1-4be4-b769-cbe40607fe25",
		"machine_id":"node-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"roots":["D:\\media"],
		"phase":1
	}`)
	request := httptest.NewRequest(http.MethodPost, "/api/scan", body)
	response := httptest.NewRecorder()
	api.Routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	var result map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["task_id"] != "b7b0ba1c-1ec1-4be4-b769-cbe40607fe25" {
		t.Fatalf("response = %#v", result)
	}
	task := <-received
	if task.TaskID != result["task_id"] || task.Roots[0] != `D:\media` {
		t.Fatalf("wire task = %#v", task)
	}
	if got := registry.List(); len(got) != 1 || got[0].TaskID != task.TaskID {
		t.Fatalf("registry = %#v", got)
	}
}

func TestScanAPIReportsOfflineAgentAndRecordsFailure(t *testing.T) {
	pool := NewPool([]config.AgentEndpoint{{
		Addr: "127.0.0.1:1",
	}}, testLogger(), func(string, *AgentConn, any) {})
	registry := NewTaskRegistry(nil, testLogger())
	api := NewAPI(pool, registry, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/scan",
		bytes.NewBufferString(`{"machine_id":"machine-a","roots":["D:\\media"],"phase":1}`))
	response := httptest.NewRecorder()
	api.Routes().ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	tasks := registry.List()
	if len(tasks) != 1 || tasks[0].Status != "failed" {
		t.Fatalf("tasks = %#v", tasks)
	}
}

func TestScanAPIRejectsMalformedResumeTaskID(t *testing.T) {
	api := NewAPI(NewPool(nil, testLogger(), nil),
		NewTaskRegistry(nil, testLogger()), nil)
	request := httptest.NewRequest(http.MethodPost, "/api/scan",
		bytes.NewBufferString(`{
			"task_id":"not-a-uuid",
			"machine_id":"machine-a",
			"roots":["D:\\media"]
		}`))
	response := httptest.NewRecorder()
	api.Routes().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestScanAPIRejectsTaskIDReusedWithDifferentEnvelope(t *testing.T) {
	registry := NewTaskRegistry(nil, testLogger())
	const taskID = "b7b0ba1c-1ec1-4be4-b769-cbe40607fe25"
	if err := registry.Register(&TaskInfo{
		TaskID: taskID, MachineID: "machine-a", Phase: 1,
		Roots: []string{`D:\one`}, Status: "sent",
	}); err != nil {
		t.Fatal(err)
	}
	api := NewAPI(NewPool(nil, testLogger(), nil), registry, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/scan",
		bytes.NewBufferString(`{
			"task_id":"b7b0ba1c-1ec1-4be4-b769-cbe40607fe25",
			"machine_id":"machine-a",
			"roots":["D:\\two"],
			"phase":1
		}`))
	response := httptest.NewRecorder()
	api.Routes().ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
}


func cancelRequest(api *API, taskID string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/cancel", nil)
	response := httptest.NewRecorder()
	api.Routes().ServeHTTP(response, request)
	return response
}

func TestTaskCancelHTTPValidatesTaskState(t *testing.T) {
	registry := NewTaskRegistry(nil, testLogger())
	pool := NewPool(nil, testLogger(), nil)
	api := NewAPI(pool, registry, nil)

	if response := cancelRequest(api, "missing"); response.Code != http.StatusNotFound {
		t.Fatalf("missing task status = %d body=%s", response.Code, response.Body.String())
	}
	for _, status := range []string{"done", "failed"} {
		if err := registry.Register(&TaskInfo{
			TaskID: "terminal-" + status, MachineID: machineA, Phase: 1,
			Roots: []string{`D:\media`}, Status: status,
		}); err != nil {
			t.Fatal(err)
		}
		if response := cancelRequest(api, "terminal-"+status); response.Code != http.StatusConflict {
			t.Fatalf("terminal %s status = %d body=%s", status, response.Code, response.Body.String())
		}
	}
}

func TestTaskCancelHTTPUnavailableWithoutServices(t *testing.T) {
	response := cancelRequest(NewAPI(nil, nil, nil), "task-1")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestTaskCancelHTTPOfflineAgentRollsBackAndReturns503(t *testing.T) {
	registry := NewTaskRegistry(nil, testLogger())
	if err := registry.Register(&TaskInfo{
		TaskID: "task-offline", MachineID: "machine-offline", Phase: 1,
		Roots: []string{`D:\media`}, Status: "running",
	}); err != nil {
		t.Fatal(err)
	}
	// 空连接池：machine-offline 不在线。
	api := NewAPI(NewPool(nil, testLogger(), nil), registry, nil)
	response := cancelRequest(api, "task-offline")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if got := registry.List()[0]; got.Status != "running" {
		t.Fatalf("offline cancel did not roll back: %#v", got)
	}
}

func TestTaskCancelHTTPSendsMessageAndStaysIdempotent(t *testing.T) {
	serverSide, guiSide := net.Pipe()
	defer serverSide.Close()
	defer guiSide.Close()
	agent := &AgentConn{
		ep: config.AgentEndpoint{Addr: "pipe"}, conn: proto.NewConn(guiSide),
		machineID: machineA, identityState: IdentityClaimed, online: true,
	}
	pool := &Pool{
		byAddr:      map[string]*AgentConn{"pipe": agent},
		byMachineID: map[string]*AgentConn{machineA: agent},
	}
	registry := NewTaskRegistry(nil, testLogger())
	if err := registry.Register(&TaskInfo{
		TaskID: "task-cancel-http", MachineID: machineA, Phase: 1,
		Roots: []string{`D:\media`}, Status: "running",
	}); err != nil {
		t.Fatal(err)
	}
	api := NewAPI(pool, registry, nil)

	received := make(chan proto.ScanTaskCancel, 1)
	go func() {
		connection := proto.NewConn(serverSide)
		for {
			msgType, body, err := connection.ReadFrame()
			if err != nil {
				return
			}
			message, err := proto.Decode(msgType, body)
			if err != nil {
				continue
			}
			if cancel, ok := message.(*proto.ScanTaskCancel); ok {
				received <- *cancel
			}
		}
	}()

	response := cancelRequest(api, "task-cancel-http")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	select {
	case cancel := <-received:
		if cancel.TaskID != "task-cancel-http" {
			t.Fatalf("wire cancel = %#v", cancel)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("agent did not receive MsgScanTaskCancel")
	}
	if got := registry.List()[0]; got.Status != "cancelling" {
		t.Fatalf("status after cancel = %#v", got)
	}

	// 重复取消幂等：返回 200 但不再重发消息。
	response = cancelRequest(api, "task-cancel-http")
	if response.Code != http.StatusOK {
		t.Fatalf("repeat cancel status = %d body=%s", response.Code, response.Body.String())
	}
	select {
	case cancel := <-received:
		t.Fatalf("idempotent repeat re-sent the message: %#v", cancel)
	case <-time.After(200 * time.Millisecond):
	}
}

type fakePreviewService struct {
	response proto.LocalResponse
	err      error
	machine  string
	request  proto.LocalImagePreviewRequest
}

func (service *fakePreviewService) Preview(
	_ context.Context,
	machineID string,
	request proto.LocalImagePreviewRequest,
) (proto.LocalResponse, error) {
	service.machine = machineID
	service.request = request
	return service.response, service.err
}

const previewTestSHA512 = "abcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcd" +
	"abcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcd"

func newPreviewTestAPI(service previewService, db *groupAPIFakeDB) *API {
	api := NewAPI(nil, nil, nil)
	api.SetPreviewBroker(service)
	api.previewDB = db
	return api
}

func TestFilePreviewHTTPValidatesParametersBeforeTouchingDatabase(t *testing.T) {
	tests := []struct {
		name   string
		target string
	}{
		{"non-numeric file id", "/api/files/abc/preview?machine=machine-a"},
		{"zero file id", "/api/files/0/preview?machine=machine-a"},
		{"negative file id", "/api/files/-3/preview?machine=machine-a"},
		{"missing machine", "/api/files/1/preview"},
		{"bad width", "/api/files/1/preview?machine=machine-a&w=0"},
		{"bad height", "/api/files/1/preview?machine=machine-a&h=8193"},
		{"non-numeric height", "/api/files/1/preview?machine=machine-a&h=big"},
		{"bad format", "/api/files/1/preview?machine=machine-a&format=png"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &fakePreviewService{}
			api := newPreviewTestAPI(service, &groupAPIFakeDB{panicOnUse: true})
			response := httptest.NewRecorder()
			api.Routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.target, nil))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if service.machine != "" {
				t.Fatalf("preview reached transport: %#v", service.request)
			}
		})
	}
}

func TestFilePreviewHTTPReturnsUnavailableWhenNotConfigured(t *testing.T) {
	api := NewAPI(nil, nil, nil)
	response := httptest.NewRecorder()
	api.Routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/files/1/preview?machine=machine-a", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestFilePreviewHTTPMapsDatabaseMissDeletedAndMissingSHA(t *testing.T) {
	tests := []struct {
		name string
		row  groupAPIRowResult
	}{
		{"unknown file", groupAPIRowResult{err: pgx.ErrNoRows}},
		{"deleted file", groupAPIRowResult{values: []any{previewTestSHA512, "deleted"}}},
		{"missing sha", groupAPIRowResult{values: []any{"", "done"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &fakePreviewService{}
			api := newPreviewTestAPI(service, &groupAPIFakeDB{rowResults: []groupAPIRowResult{test.row}})
			response := httptest.NewRecorder()
			api.Routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/files/1/preview?machine=machine-a", nil))
			if response.Code != http.StatusNotFound {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if service.machine != "" {
				t.Fatalf("preview reached transport for %s", test.name)
			}
		})
	}
}

func TestFilePreviewHTTPReportsOfflineAgent(t *testing.T) {
	service := &fakePreviewService{err: ErrPreviewAgentOffline}
	api := newPreviewTestAPI(service, &groupAPIFakeDB{
		rowResults: []groupAPIRowResult{{values: []any{previewTestSHA512, "done"}}},
	})
	response := httptest.NewRecorder()
	api.Routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/files/1/preview?machine=machine-a", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "agent_offline" {
		t.Fatalf("body=%s", response.Body.String())
	}
}

func TestFilePreviewHTTPStreamsAgentBytesWithCacheHeaders(t *testing.T) {
	payload, err := proto.EncodeLocalPayload(proto.LocalImagePreviewResponse{
		MIME: "image/webp", Width: 64, Height: 48, Bytes: []byte{9, 8, 7, 6},
	})
	if err != nil {
		t.Fatal(err)
	}
	service := &fakePreviewService{response: proto.LocalResponse{OK: true, Payload: payload}}
	api := newPreviewTestAPI(service, &groupAPIFakeDB{
		rowResults: []groupAPIRowResult{{values: []any{previewTestSHA512, "done"}}},
	})
	response := httptest.NewRecorder()
	api.Routes().ServeHTTP(response, httptest.NewRequest(
		http.MethodGet, "/api/files/41/preview?machine=machine-a&w=128&h=96&format=webp", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "image/webp" {
		t.Fatalf("Content-Type=%q", got)
	}
	if got := response.Header().Get("Cache-Control"); got != "private, max-age=300" {
		t.Fatalf("Cache-Control=%q", got)
	}
	if !bytes.Equal(response.Body.Bytes(), []byte{9, 8, 7, 6}) {
		t.Fatalf("body=%v", response.Body.Bytes())
	}
	if service.machine != "machine-a" || service.request.Sha512 != previewTestSHA512 ||
		service.request.FileID != 0 || service.request.MaxWidth != 128 ||
		service.request.MaxHeight != 96 || service.request.Format != "webp" {
		t.Fatalf("agent request=%q %#v", service.machine, service.request)
	}
}

func TestFilePreviewHTTPMapsAgentErrorCodesToSafeStatuses(t *testing.T) {
	tests := []struct {
		name      string
		errorCode string
		wantCode  int
	}{
		{"stale", "stale_preview", http.StatusConflict},
		{"not available", "preview_not_available", http.StatusNotFound},
		{"too large", "preview_too_large", http.StatusRequestEntityTooLarge},
		{"memory limit", "preview_memory_limit", http.StatusInsufficientStorage},
		{"disconnected", "agent_disconnected", http.StatusServiceUnavailable},
		{"generic failure", "preview_failed", http.StatusBadGateway},
		{"legacy unauthorized agent", "unauthorized", http.StatusBadGateway},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &fakePreviewService{response: proto.LocalResponse{ErrorCode: test.errorCode}}
			api := newPreviewTestAPI(service, &groupAPIFakeDB{
				rowResults: []groupAPIRowResult{{values: []any{previewTestSHA512, "done"}}},
			})
			response := httptest.NewRecorder()
			api.Routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/files/1/preview?machine=machine-a", nil))
			if response.Code != test.wantCode {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), test.errorCode) && test.errorCode != "agent_disconnected" {
				t.Fatalf("internal error code leaked: %s", response.Body.String())
			}
		})
	}
}
