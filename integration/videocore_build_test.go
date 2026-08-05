package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type nativeClosureManifest struct {
	SchemaVersion int `json:"schema_version"`
	Files         []struct {
		Name    string   `json:"name"`
		Path    string   `json:"path"`
		SHA256  string   `json:"sha256"`
		Imports []string `json:"imports"`
	} `json:"files"`
}

func TestVideoCoreNativeDependencyClosureHandlesCaseDedupAndCycles(t *testing.T) {
	repo := testRepositoryRoot(t)
	fixture := filepath.Join(t.TempDir(), "repository")
	graph := filepath.Join(fixture, "graph")
	mustMkdirAll(t, graph)
	for _, name := range []string{"Root.DLL", "a.dll", "B.DlL"} {
		mustWrite(t, filepath.Join(graph, name), []byte("synthetic "+name))
	}
	dumpbin := writeFakeDumpbin(t, map[string][]string{
		"root.dll": {"a.DLL", "B.dll", "A.dll", "KERNEL32.dll", "D2D1.dll", "DWrite.dll", "USP10.dll"},
		"a.dll":    {"b.DLL"},
		"b.dll":    {"ROOT.dll", "api-ms-win-core-file-l1-1-0.dll"},
	})
	outFile := filepath.Join(t.TempDir(), "native-dependencies.json")
	result := runPowerShell(t,
		filepath.Join(repo, "scripts", "resolve_native_dependencies.ps1"),
		"-RootDll", filepath.Join(graph, "Root.DLL"),
		"-SearchRoot", graph,
		"-RepositoryRoot", fixture,
		"-Dumpbin", dumpbin,
		"-OutFile", outFile,
	)
	if result.exitCode != 0 {
		t.Fatalf("closure exit=%d, want 0:\n%s", result.exitCode, result.output)
	}
	var manifest nativeClosureManifest
	decodeJSONFile(t, outFile, &manifest)
	if manifest.SchemaVersion != 1 || len(manifest.Files) != 3 {
		t.Fatalf("closure manifest=%+v, want schema 1 and three unique DLLs", manifest)
	}
	names := make([]string, 0, len(manifest.Files))
	for _, file := range manifest.Files {
		names = append(names, strings.ToLower(file.Name))
		if len(file.SHA256) != 64 {
			t.Errorf("%s SHA-256 length=%d, want 64", file.Name, len(file.SHA256))
		}
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "a.dll,b.dll,root.dll" {
		t.Fatalf("closure names=%v", names)
	}
	if !strings.Contains(result.output, "NATIVE DEPENDENCY CLOSURE PASS files=3") {
		t.Fatalf("closure lacks stable success marker:\n%s", result.output)
	}
}

func TestVideoCoreNativeDependencyClosureFailsClosed(t *testing.T) {
	repo := testRepositoryRoot(t)
	tests := []struct {
		name     string
		prepare  func(t *testing.T) (root, search, repository, dumpbin string)
		wantCode string
	}{
		{
			name: "unresolved non-system import",
			prepare: func(t *testing.T) (string, string, string, string) {
				repository := filepath.Join(t.TempDir(), "repository")
				search := filepath.Join(repository, "graph")
				mustMkdirAll(t, search)
				root := filepath.Join(search, "root.dll")
				mustWrite(t, root, []byte("root"))
				return root, search, repository, writeFakeDumpbin(t, map[string][]string{
					"root.dll": {"missing-runtime.dll"},
				})
			},
			wantCode: "NATIVE_DEPENDENCY_UNRESOLVED",
		},
		{
			name: "ambiguous duplicate name",
			prepare: func(t *testing.T) (string, string, string, string) {
				repository := filepath.Join(t.TempDir(), "repository")
				search := filepath.Join(repository, "graph")
				mustMkdirAll(t, filepath.Join(search, "one"))
				mustMkdirAll(t, filepath.Join(search, "two"))
				root := filepath.Join(search, "root.dll")
				mustWrite(t, root, []byte("root"))
				mustWrite(t, filepath.Join(search, "one", "same.dll"), []byte("one"))
				mustWrite(t, filepath.Join(search, "two", "SAME.DLL"), []byte("two"))
				return root, search, repository, writeFakeDumpbin(t, map[string][]string{
					"root.dll": {"same.dll"},
				})
			},
			wantCode: "NATIVE_DEPENDENCY_AMBIGUOUS",
		},
		{
			name: "source outside repository",
			prepare: func(t *testing.T) (string, string, string, string) {
				temp := t.TempDir()
				repository := filepath.Join(temp, "repository")
				search := filepath.Join(temp, "outside")
				mustMkdirAll(t, repository)
				mustMkdirAll(t, search)
				root := filepath.Join(search, "root.dll")
				mustWrite(t, root, []byte("root"))
				return root, search, repository, writeFakeDumpbin(t, map[string][]string{})
			},
			wantCode: "NATIVE_DEPENDENCY_OUTSIDE_REPOSITORY",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, search, repository, dumpbin := test.prepare(t)
			outFile := filepath.Join(t.TempDir(), "must-not-exist.json")
			result := runPowerShell(t,
				filepath.Join(repo, "scripts", "resolve_native_dependencies.ps1"),
				"-RootDll", root,
				"-SearchRoot", search,
				"-RepositoryRoot", repository,
				"-Dumpbin", dumpbin,
				"-OutFile", outFile,
			)
			if result.exitCode == 0 || !strings.Contains(result.output, test.wantCode) {
				t.Fatalf("exit=%d, want failure %s:\n%s", result.exitCode, test.wantCode, result.output)
			}
			if _, err := os.Stat(outFile); !os.IsNotExist(err) {
				t.Fatalf("failed closure published manifest: %v", err)
			}
		})
	}
}

func TestVideoCoreExportGateRequiresExactlyDefExports(t *testing.T) {
	repo := testRepositoryRoot(t)
	dll := filepath.Join(t.TempDir(), "videocore.dll")
	mustWrite(t, dll, []byte("synthetic DLL placeholder"))
	def := filepath.Join(repo, "videocore", "exports.def")
	want := []string{
		"vc_abi_version", "vc_version", "vc_runtime_info",
		"vc_cancel_create", "vc_cancel_request", "vc_cancel_free",
		"vc_media_open_w", "vc_media_hash", "vc_media_analyze",
		"vc_media_close",
	}
	for _, test := range []struct {
		name     string
		exports  []string
		wantCode string
	}{
		{name: "exact", exports: want},
		{name: "missing", exports: want[:9], wantCode: "VIDEOCORE_EXPORT_MISMATCH"},
		{name: "extra", exports: append(append([]string{}, want...), "vc_private_leak"), wantCode: "VIDEOCORE_EXPORT_MISMATCH"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dumpbin := writeFakeExportDumpbin(t, test.exports)
			outFile := filepath.Join(t.TempDir(), "exports.txt")
			result := runPowerShell(t,
				filepath.Join(repo, "scripts", "test-videocore-exports.ps1"),
				"-Dll", dll, "-Def", def, "-Dumpbin", dumpbin,
				"-OutFile", outFile,
			)
			if test.wantCode == "" {
				if result.exitCode != 0 || !strings.Contains(result.output, "10/10 exact exports") {
					t.Fatalf("exact exports exit=%d:\n%s", result.exitCode, result.output)
				}
				data, err := os.ReadFile(outFile)
				if err != nil {
					t.Fatal(err)
				}
				lines := strings.Fields(string(data))
				if len(lines) != 10 {
					t.Fatalf("saved export list has %d entries, want 10", len(lines))
				}
			} else if result.exitCode == 0 || !strings.Contains(result.output, test.wantCode) {
				t.Fatalf("mutation exit=%d, want %s:\n%s", result.exitCode, test.wantCode, result.output)
			} else if _, err := os.Stat(outFile); !os.IsNotExist(err) {
				t.Fatalf("failed export gate published list: %v", err)
			}
		})
	}
}

func TestVideoCoreOnlyRejectsExistingStageBeforeToolResolution(t *testing.T) {
	repo := testRepositoryRoot(t)
	stage := filepath.Join(t.TempDir(), "existing-stage")
	mustMkdirAll(t, stage)
	result := runPowerShell(t,
		filepath.Join(repo, "scripts", "build.ps1"),
		"-VideoCoreOnly",
		"-StageDir", stage,
		"-CMake", filepath.Join(t.TempDir(), "missing-cmake.exe"),
		"-VcpkgRoot", filepath.Join(t.TempDir(), "missing-vcpkg"),
		"-CC", "missing-gcc",
		"-Dlltool", "missing-dlltool",
	)
	if result.exitCode == 0 || !strings.Contains(result.output, "VIDEOCORE_STAGE_EXISTS") {
		t.Fatalf("existing stage exit=%d, want stable fail-closed code before tool resolution:\n%s",
			result.exitCode, result.output)
	}
}

func TestVideoCoreBuildResolversSelectFirstExistingApplicationAsString(t *testing.T) {
	repo := testRepositoryRoot(t)
	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	stage := filepath.Join(root, "existing-stage")
	mustMkdirAll(t, first)
	mustMkdirAll(t, second)
	mustMkdirAll(t, stage)
	for _, name := range []string{
		"task10-cmake.exe", "pwsh.exe", "dlltool.exe", "task10-custom.exe",
	} {
		mustWrite(t, filepath.Join(first, name), []byte("first candidate"))
		mustWrite(t, filepath.Join(second, name), []byte("second candidate"))
	}

	command := fmt.Sprintf(
		"$env:PATH = %s + [IO.Path]::PathSeparator + %s + [IO.Path]::PathSeparator + $env:PATH; "+
			". %s -VideoCoreOnly -StageDir %s; "+
			"function Assert-Resolved([string]$label, $actual, [string]$expected) { "+
			"if ($actual -is [array]) { throw \"$label returned an array\" }; "+
			"if (-not ($actual -is [string])) { throw \"$label did not return a string\" }; "+
			"if (-not (Test-Path -LiteralPath $actual -PathType Leaf)) { throw \"$label returned a non-leaf path\" }; "+
			"if (-not [string]::Equals($actual, $expected, [StringComparison]::OrdinalIgnoreCase)) { throw \"$label selected $actual instead of $expected\" } }; "+
			"Assert-Resolved 'cmake' (Resolve-CMakeExecutable -Requested 'task10-cmake.exe' -Root 'unused') %s; "+
			"Assert-Resolved 'pwsh' (Resolve-Application -Requested 'pwsh' -Label 'PowerShell') %s; "+
			"Assert-Resolved 'dlltool' (Resolve-Application -Requested 'dlltool' -Label 'dlltool') %s; "+
			"Assert-Resolved 'generic' (Resolve-Application -Requested 'task10-custom.exe' -Label 'custom') %s",
		psSingleQuoted(first),
		psSingleQuoted(second),
		psSingleQuoted(filepath.Join(repo, "scripts", "build.ps1")),
		psSingleQuoted(stage),
		psSingleQuoted(filepath.Join(first, "task10-cmake.exe")),
		psSingleQuoted(filepath.Join(first, "pwsh.exe")),
		psSingleQuoted(filepath.Join(first, "dlltool.exe")),
		psSingleQuoted(filepath.Join(first, "task10-custom.exe")),
	)
	output, err := exec.Command("pwsh", "-NoProfile", "-Command", command).CombinedOutput()
	if err != nil {
		t.Fatalf("build application resolution failed: %v\n%s", err, output)
	}
}

func TestVideoCoreNativeVerifierSelectsOneExistingPowerShell(t *testing.T) {
	repo := testRepositoryRoot(t)
	verifier := filepath.Join(repo, "scripts", "verify_videocore_native.ps1")
	command := fmt.Sprintf(
		". %s; $resolved = Resolve-VerificationApplication -Name 'pwsh'; "+
			"if ($resolved -is [array]) { throw 'resolver returned an array' }; "+
			"if (-not ($resolved -is [string])) { throw 'resolver did not return a string' }; "+
			"if (-not (Test-Path -LiteralPath $resolved -PathType Leaf)) { throw 'resolver returned a non-leaf path' }; "+
			"Write-Output $resolved",
		psSingleQuoted(verifier),
	)
	output, err := exec.Command("pwsh", "-NoProfile", "-Command", command).CombinedOutput()
	if err != nil {
		t.Fatalf("unique PowerShell resolution failed: %v\n%s", err, output)
	}
	lines := strings.Fields(string(output))
	if len(lines) == 0 {
		t.Fatal("unique PowerShell resolution returned no path")
	}
}

func TestVideoCoreBuildStaticContract(t *testing.T) {
	repo := testRepositoryRoot(t)
	data, err := os.ReadFile(filepath.Join(repo, "scripts", "build.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	if err := validateVideoCoreBuildContract(source); err != nil {
		t.Fatalf("repository build contract: %v", err)
	}

	buildMarker := `& $cmakeExe --build $videoCoreBuild --config Release`
	ctestMarker := `& $ctestExe --test-dir $videoCoreBuild -C Release --output-on-failure`
	mutations := []struct {
		name   string
		source string
	}{
		{name: "VideoCore only no longer covers full build", source: strings.Replace(source,
			`$useVideoCore = $VideoCoreOnly -or (-not $MediacoreOnly)`,
			`$useVideoCore = $VideoCoreOnly`, 1)},
		{name: "CTest before native build", source: swapBuildContractMarkers(t, source, buildMarker, ctestMarker)},
		{name: "helper GUI subsystem linker flag removed", source: strings.Replace(
			source,
			`"-ldflags=-H=windowsgui"`,
			`"-ldflags="`,
			1,
		)},
		{name: "helper GUI subsystem linker flag moved to unused variable", source: strings.Replace(
			source,
			"& $Go -C $repo build -trimpath \"-ldflags=-H=windowsgui\" `\n            -o (Join-Path $out \"helper.exe\") ./cmd/helper",
			"$helperGUIFlag = \"-ldflags=-H=windowsgui\"\n        & $Go -C $repo build -trimpath `\n            -o (Join-Path $out \"helper.exe\") ./cmd/helper",
			1,
		)},
		{name: "helper build command moved into block comment", source: strings.Replace(
			source,
			"& $Go -C $repo build -trimpath \"-ldflags=-H=windowsgui\" `\n            -o (Join-Path $out \"helper.exe\") ./cmd/helper",
			"<#\n        & $Go -C $repo build -trimpath \"-ldflags=-H=windowsgui\" `\n            -o (Join-Path $out \"helper.exe\") ./cmd/helper\n        #>",
			1,
		)},
		{name: "helper target replaced", source: strings.Replace(source, `./cmd/helper`, `./cmd/agent`, 1)},
		{name: "non-CGO applications built with CGO", source: strings.Replace(source,
			`$env:CGO_ENABLED = "0"`, `$env:CGO_ENABLED = "1"`, 1)},
		{name: "worker built without CGO", source: strings.Replace(source,
			`$env:CGO_ENABLED = "1"`, `$env:CGO_ENABLED = "0"`, 1)},
		{name: "old import library allowed", source: strings.Replace(source,
			`"mediacore.dll", "libmediacore.a", "ffmpeg.exe", "ffprobe.exe", "ffplay.exe"`,
			`"mediacore.dll", "ffmpeg.exe", "ffprobe.exe", "ffplay.exe"`, 1)},
		{name: "tools directory allowed", source: strings.Replace(source,
			`Join-Path $out "tools"`, `Join-Path $out "legacy-tools"`, 1)},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			if mutation.source == source {
				t.Fatal("mutation did not change script source")
			}
			if err := validateVideoCoreBuildContract(mutation.source); err == nil {
				t.Fatal("mutated build contract was accepted")
			}
		})
	}
}

func swapBuildContractMarkers(t *testing.T, source, left, right string) string {
	t.Helper()
	if strings.Count(source, left) != 1 || strings.Count(source, right) != 1 {
		t.Fatalf("cannot swap non-unique contract markers %q and %q", left, right)
	}
	const placeholder = "__TASK18_BUILD_ORDER_PLACEHOLDER__"
	source = strings.Replace(source, left, placeholder, 1)
	source = strings.Replace(source, right, left, 1)
	return strings.Replace(source, placeholder, right, 1)
}

func powerShellLogicalCommands(source string) []string {
	lines := strings.Split(strings.ReplaceAll(source, "\r\n", "\n"), "\n")
	commands := make([]string, 0)
	inBlockComment := false
	for lineIndex := 0; lineIndex < len(lines); lineIndex++ {
		line := lines[lineIndex]
		for {
			if inBlockComment {
				commentEnd := strings.Index(line, "#>")
				if commentEnd < 0 {
					line = ""
					break
				}
				line = line[commentEnd+2:]
				inBlockComment = false
			}
			commentStart := strings.Index(line, "<#")
			if commentStart < 0 {
				break
			}
			commentEnd := strings.Index(line[commentStart+2:], "#>")
			if commentEnd < 0 {
				line = line[:commentStart]
				inBlockComment = true
				break
			}
			commentEnd += commentStart + 2
			line = line[:commentStart] + line[commentEnd+2:]
		}
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "& ") {
			continue
		}
		command := strings.TrimSpace(strings.TrimSuffix(line, "`"))
		for strings.HasSuffix(line, "`") && lineIndex+1 < len(lines) {
			lineIndex++
			line = strings.TrimSpace(lines[lineIndex])
			command += " " + strings.TrimSpace(strings.TrimSuffix(line, "`"))
		}
		commands = append(commands, command)
	}
	return commands
}

func validateVideoCoreBuildContract(source string) error {
	const helperBuildCommand = `& $Go -C $repo build -trimpath "-ldflags=-H=windowsgui" -o (Join-Path $out "helper.exe") ./cmd/helper`
	helperCommandCount := 0
	for _, command := range powerShellLogicalCommands(source) {
		if command == helperBuildCommand {
			helperCommandCount++
		}
	}
	if helperCommandCount != 1 {
		return fmt.Errorf("complete Helper build logical command count=%d, want 1", helperCommandCount)
	}

	ordered := []struct {
		label  string
		marker string
	}{
		{label: "full-build VideoCore selection", marker: `$useVideoCore = $VideoCoreOnly -or (-not $MediacoreOnly)`},
		{label: "VideoCore configure", marker: `-S $videoCoreSource`},
		{label: "VideoCore build", marker: `& $cmakeExe --build $videoCoreBuild --config Release`},
		{label: "VideoCore CTest", marker: `& $ctestExe --test-dir $videoCoreBuild -C Release --output-on-failure`},
		{label: "exact export gate", marker: `& $pwshExe -NoProfile -File $exportGate`},
		{label: "MinGW import library", marker: `& $videoDlltoolExe`},
		{label: "recursive dependency closure", marker: `& $pwshExe -NoProfile -File $resolver`},
		{label: "non-CGO boundary", marker: `$env:CGO_ENABLED = "0"`},
		{label: "Agent target", marker: `./cmd/agent`},
		{label: "GUI target", marker: `./cmd/gui`},
		{label: "Helper GUI subsystem linker flag", marker: `"-ldflags=-H=windowsgui"`},
		{label: "Helper target", marker: `./cmd/helper`},
		{label: "CGO Worker boundary", marker: `$env:CGO_ENABLED = "1"`},
		{label: "Worker target", marker: `./cmd/worker`},
		{label: "release manifest", marker: `Set-Content -LiteralPath (Join-Path $out "release-manifest.json")`},
	}
	previous := -1
	for _, checkpoint := range ordered {
		if strings.Count(source, checkpoint.marker) != 1 {
			return fmt.Errorf("%s marker count=%d, want 1", checkpoint.label, strings.Count(source, checkpoint.marker))
		}
		position := strings.Index(source, checkpoint.marker)
		if position <= previous {
			return fmt.Errorf("%s is out of order", checkpoint.label)
		}
		previous = position
	}

	for _, required := range []string{
		"VIDEOCORE_STAGE_REQUIRED",
		"test-videocore-exports.ps1",
		"libvideocore.a",
		"resolve_native_dependencies.ps1",
		"VIDEOCORE_FORBIDDEN_STAGE_ARTIFACT",
	} {
		if !strings.Contains(source, required) {
			return fmt.Errorf("missing full-build contract %q", required)
		}
	}

	listStart := strings.Index(source, `$forbiddenStageNames = @(`)
	listEnd := strings.Index(source, `foreach ($name in $forbiddenStageNames)`)
	if listStart < 0 || listEnd <= listStart {
		return fmt.Errorf("forbidden stage list is missing")
	}
	forbiddenBlock := source[listStart:listEnd]
	for _, name := range []string{
		"mediacore.dll", "libmediacore.a", "ffmpeg.exe", "ffprobe.exe", "ffplay.exe",
	} {
		if !strings.Contains(forbiddenBlock, `"`+name+`"`) {
			return fmt.Errorf("forbidden stage list lacks %s", name)
		}
	}
	if !strings.Contains(source, `Test-Path -LiteralPath (Join-Path $out "tools")`) {
		return fmt.Errorf("forbidden stage contract lacks tools directory check")
	}
	return nil
}

func TestCGOScriptDefaultsToVideoCore(t *testing.T) {
	repo := testRepositoryRoot(t)
	data, err := os.ReadFile(filepath.Join(repo, "scripts", "test-cgo.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	if !strings.Contains(source, `[string]$Mode = "VideoCore"`) {
		t.Fatalf("test-cgo.ps1 does not default to VideoCore")
	}
	if !strings.Contains(source, `internal\wproc\videocore\libvideocore.a`) {
		t.Fatalf("test-cgo.ps1 lacks VideoCore import-library contract")
	}
}

type psResult struct {
	exitCode int
	output   string
}

func runPowerShell(t *testing.T, script string, args ...string) psResult {
	t.Helper()
	commandArgs := append([]string{"-NoProfile", "-File", script}, args...)
	command := exec.Command("pwsh", commandArgs...)
	output, err := command.CombinedOutput()
	result := psResult{output: string(output)}
	if err == nil {
		return result
	}
	if exitError, ok := err.(*exec.ExitError); ok {
		result.exitCode = exitError.ExitCode()
		return result
	}
	t.Fatalf("start pwsh: %v", err)
	return psResult{}
}

func testRepositoryRoot(t *testing.T) string {
	t.Helper()
	working, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(working)
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func decodeJSONFile(t *testing.T, path string, out any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

func psSingleQuoted(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func writeFakeDumpbin(t *testing.T, imports map[string][]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-dumpbin.ps1")
	var script strings.Builder
	script.WriteString("param([string]$Mode, [string]$Path)\n")
	script.WriteString("$leaf = [IO.Path]::GetFileName($Path).ToLowerInvariant()\n")
	script.WriteString("switch ($leaf) {\n")
	keys := make([]string, 0, len(imports))
	for key := range imports {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		script.WriteString("  " + psSingleQuoted(strings.ToLower(key)) + " { @(")
		values := []string{"Image has the following dependencies:"}
		values = append(values, imports[key]...)
		for index, value := range values {
			if index != 0 {
				script.WriteString(", ")
			}
			script.WriteString(psSingleQuoted("    " + value))
		}
		script.WriteString("); break }\n")
	}
	script.WriteString("  default { @('Image has the following dependencies:') }\n}\n")
	mustWrite(t, path, []byte(script.String()))
	return path
}

func writeFakeExportDumpbin(t *testing.T, exports []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-export-dumpbin.ps1")
	var script strings.Builder
	script.WriteString("param([string]$Mode, [string]$Path)\n")
	script.WriteString("'    ordinal hint RVA      name'\n")
	for index, name := range exports {
		script.WriteString(fmt.Sprintf("'%11d %4X 00001000 %s'\n", index+1, index, name))
	}
	script.WriteString("'  Summary'\n")
	mustWrite(t, path, []byte(script.String()))
	return path
}
