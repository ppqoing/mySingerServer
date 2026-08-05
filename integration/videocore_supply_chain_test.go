package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	supplyExitValidationFailed = 2
	supplyExitReleaseBlocked   = 3
)

type supplyManifest struct {
	SchemaVersion     int                      `json:"schema_version"`
	SDKID             string                   `json:"sdk_id"`
	Provenance        supplyProvenance         `json:"provenance"`
	Components        []supplyComponent        `json:"components"`
	License           supplyLicense            `json:"license"`
	EvidenceDocuments []supplyEvidenceDocument `json:"evidence_documents"`
	Redistributable   bool                     `json:"redistributable"`
	Blockers          []supplyBlocker          `json:"blockers"`
	Files             []supplyManifestFile     `json:"files"`
}

type supplyProvenance struct {
	SourceURL           *string  `json:"source_url"`
	Version             string   `json:"version"`
	Commit              *string  `json:"commit"`
	ConfigureFlags      []string `json:"configure_flags"`
	SourceArchiveSHA256 *string  `json:"source_archive_sha256"`
	SourceDocument      string   `json:"source_document"`
}

type supplyComponent struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Major   int    `json:"major"`
}

type supplyLicense struct {
	Classification   string `json:"classification"`
	LicenseFile      string `json:"license_file"`
	NoticeFile       string `json:"notice_file"`
	EvidenceComplete bool   `json:"evidence_complete"`
}

type supplyBlocker struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type supplyManifestFile struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type supplyEvidenceDocument struct {
	Role   string `json:"role"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type supplyEvidence struct {
	SchemaVersion int               `json:"schema_version"`
	Mode          string            `json:"mode"`
	Status        string            `json:"status"`
	ExitCode      int               `json:"exit_code"`
	SDKID         string            `json:"sdk_id"`
	Components    []supplyComponent `json:"components"`
	Digests       struct {
		ManifestSHA256 string `json:"manifest_sha256"`
		SchemaSHA256   string `json:"schema_sha256"`
	} `json:"digests"`
	SchemaValidation struct {
		Status string   `json:"status"`
		Errors []string `json:"errors"`
	} `json:"schema_validation"`
	FileIntegrity struct {
		Status          string               `json:"status"`
		ExpectedCount   int                  `json:"expected_count"`
		DiscoveredCount int                  `json:"discovered_count"`
		Verified        []supplyManifestFile `json:"verified"`
	} `json:"file_integrity"`
	EvidenceDocuments []supplyEvidenceDocument `json:"evidence_documents"`
	Redistribution    struct {
		Status          string          `json:"status"`
		Redistributable bool            `json:"redistributable"`
		Blockers        []supplyBlocker `json:"blockers"`
	} `json:"redistribution"`
	Errors []struct {
		Code    string `json:"code"`
		Path    string `json:"path"`
		Message string `json:"message"`
	} `json:"errors"`
}

func TestVideoCoreSupplyChainRepositoryManifestPinsAllInputs(t *testing.T) {
	repoRoot := supplyRepoRoot(t)
	evidencePath := filepath.Join(t.TempDir(), "local-evidence.json")
	result := runSupplyVerifier(
		t,
		filepath.Join(repoRoot, "third_party", "ffmpeg", "manifest.json"),
		filepath.Join(repoRoot, "third_party", "ffmpeg"),
		"Local",
		evidencePath,
	)
	if result.ExitCode != 0 {
		t.Fatalf("Local repository verification exit=%d, want 0:\n%s", result.ExitCode, result.Output)
	}
	if !strings.Contains(result.Output, "RELEASE BLOCKED") {
		t.Fatalf("Local verification must clearly report RELEASE BLOCKED:\n%s", result.Output)
	}
	evidence := loadSupplyEvidence(t, evidencePath)
	if evidence.Status != "release_blocked" {
		t.Fatalf("evidence status=%q, want release_blocked", evidence.Status)
	}
	if evidence.SDKID != "N-125444-g6d72600a30-20260703" {
		t.Fatalf("repository sdk_id=%q, want pinned FFmpeg SDK id", evidence.SDKID)
	}
	if len(evidence.Components) != 7 || len(evidence.Digests.ManifestSHA256) != 64 ||
		len(evidence.Digests.SchemaSHA256) != 64 {
		t.Fatalf("evidence does not pin components and manifest/schema digests: %+v", evidence)
	}
	if len(evidence.EvidenceDocuments) != 3 {
		t.Fatalf("evidence does not contain three hashed authority documents: %+v", evidence.EvidenceDocuments)
	}
	roles := map[string]bool{}
	for _, document := range evidence.EvidenceDocuments {
		roles[document.Role] = len(document.SHA256) == 64 && document.Size > 0
	}
	for _, role := range []string{"source", "license", "notice"} {
		if !roles[role] {
			t.Fatalf("evidence document role %q is absent or unhashed: %+v", role, evidence.EvidenceDocuments)
		}
	}
	if evidence.SchemaValidation.Status != "pass" || evidence.FileIntegrity.Status != "pass" {
		t.Fatalf("Local schema/integrity did not pass: %+v", evidence)
	}
	if evidence.FileIntegrity.ExpectedCount == 0 ||
		evidence.FileIntegrity.ExpectedCount != evidence.FileIntegrity.DiscoveredCount ||
		len(evidence.FileIntegrity.Verified) != evidence.FileIntegrity.ExpectedCount {
		t.Fatalf("required inputs are not closed and individually verified: %+v", evidence.FileIntegrity)
	}
	for _, file := range evidence.FileIntegrity.Verified {
		if len(file.SHA256) != 64 || file.Size <= 0 {
			t.Fatalf("invalid independent file evidence for %q: %+v", file.Path, file)
		}
	}
}

func TestVideoCoreSupplyChainReleaseFailsWhenAuthoritativeEvidenceIsIncomplete(t *testing.T) {
	repoRoot := supplyRepoRoot(t)
	evidencePath := filepath.Join(t.TempDir(), "release-evidence.json")
	result := runSupplyVerifier(
		t,
		filepath.Join(repoRoot, "third_party", "ffmpeg", "manifest.json"),
		filepath.Join(repoRoot, "third_party", "ffmpeg"),
		"Release",
		evidencePath,
	)
	if result.ExitCode != supplyExitReleaseBlocked {
		t.Fatalf("Release verification exit=%d, want %d:\n%s", result.ExitCode, supplyExitReleaseBlocked, result.Output)
	}
	if !strings.Contains(result.Output, "RELEASE BLOCKED") {
		t.Fatalf("Release failure must clearly report RELEASE BLOCKED:\n%s", result.Output)
	}
	evidence := loadSupplyEvidence(t, evidencePath)
	if evidence.Status != "release_blocked" || evidence.ExitCode != supplyExitReleaseBlocked {
		t.Fatalf("Release evidence must record the hard gate: %+v", evidence)
	}
	if len(evidence.Redistribution.Blockers) == 0 {
		t.Fatal("Release evidence did not retain authoritative-evidence blockers")
	}
}

func TestVideoCoreSupplyChainDetectsManifestAndFileIntegrityFailures(t *testing.T) {
	testCases := []struct {
		name     string
		mutate   func(t *testing.T, fixture *supplyFixture)
		wantCode string
	}{
		{
			name: "missing required manifest field",
			mutate: func(t *testing.T, fixture *supplyFixture) {
				fixture.Manifest.SDKID = ""
			},
			wantCode: "MANIFEST_SCHEMA_INVALID",
		},
		{
			name: "hash drift",
			mutate: func(t *testing.T, fixture *supplyFixture) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(fixture.Root, "bin", "avcodec-63.dll"), []byte("drifted"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantCode: "HASH_MISMATCH",
		},
		{
			name: "unlisted runtime DLL",
			mutate: func(t *testing.T, fixture *supplyFixture) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(fixture.Root, "bin", "extra-1.dll"), []byte("unlisted"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantCode: "UNLISTED_REQUIRED_FILE",
		},
		{
			name: "duplicate manifest path",
			mutate: func(t *testing.T, fixture *supplyFixture) {
				fixture.Manifest.Files = append(fixture.Manifest.Files, fixture.Manifest.Files[0])
			},
			wantCode: "DUPLICATE_FILE_PATH",
		},
		{
			name: "missing source document",
			mutate: func(t *testing.T, fixture *supplyFixture) {
				t.Helper()
				if err := os.Remove(filepath.Join(fixture.Root, "SOURCE.md")); err != nil {
					t.Fatal(err)
				}
			},
			wantCode: "SOURCE_DOCUMENT_MISSING",
		},
		{
			name: "evidence document drift",
			mutate: func(t *testing.T, fixture *supplyFixture) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(fixture.Root, "SOURCE.md"), []byte("drifted source evidence\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantCode: "EVIDENCE_DOCUMENT_HASH_MISMATCH",
		},
		{
			name: "path traversal",
			mutate: func(t *testing.T, fixture *supplyFixture) {
				fixture.Manifest.Files[0].Path = "include/../outside.h"
			},
			wantCode: "FILE_PATH_INVALID",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newSupplyFixture(t, true)
			testCase.mutate(t, fixture)
			fixture.writeManifest(t)
			evidencePath := filepath.Join(t.TempDir(), "evidence.json")
			result := runSupplyVerifier(t, fixture.ManifestPath, fixture.Root, "Local", evidencePath)
			if result.ExitCode != supplyExitValidationFailed {
				t.Fatalf("verification exit=%d, want %d:\n%s", result.ExitCode, supplyExitValidationFailed, result.Output)
			}
			assertSupplyFailureNoEvidence(t, result, evidencePath, testCase.wantCode)
		})
	}
}

func TestVideoCoreSupplyChainRejectsForbiddenLicenseClassifications(t *testing.T) {
	for _, classification := range []string{"unknown", "nonfree"} {
		t.Run(classification, func(t *testing.T) {
			fixture := newSupplyFixture(t, true)
			fixture.Manifest.License.Classification = classification
			fixture.writeManifest(t)
			evidencePath := filepath.Join(t.TempDir(), "evidence.json")
			result := runSupplyVerifier(t, fixture.ManifestPath, fixture.Root, "Local", evidencePath)
			if result.ExitCode != supplyExitValidationFailed {
				t.Fatalf("verification exit=%d, want %d:\n%s", result.ExitCode, supplyExitValidationFailed, result.Output)
			}
			assertSupplyFailureNoEvidence(t, result, evidencePath, "LICENSE_FORBIDDEN")
		})
	}
}

func TestVideoCoreSupplyChainRejectsSelfAssertedReleaseAuthority(t *testing.T) {
	fixture := newSupplyFixture(t, true)
	evidencePath := filepath.Join(t.TempDir(), "evidence.json")
	result := runSupplyVerifier(t, fixture.ManifestPath, fixture.Root, "Release", evidencePath)
	if result.ExitCode != supplyExitReleaseBlocked {
		t.Fatalf("self-asserted synthetic authority exit=%d, want %d:\n%s",
			result.ExitCode, supplyExitReleaseBlocked, result.Output)
	}
	if !strings.Contains(result.Output, "RELEASE BLOCKED") {
		t.Fatalf("self-asserted evidence must remain blocked:\n%s", result.Output)
	}
	evidence := loadSupplyEvidence(t, evidencePath)
	if evidence.Status != "release_blocked" || evidence.ExitCode != supplyExitReleaseBlocked {
		t.Fatalf("self-asserted evidence escaped the release gate: %+v", evidence)
	}
	foundAuthorityGate := false
	for _, blocker := range evidence.Redistribution.Blockers {
		if blocker.Code == "AUTHORITATIVE_REVIEW_GATE_REQUIRED" {
			foundAuthorityGate = true
		}
	}
	if !foundAuthorityGate {
		t.Fatalf("self-asserted evidence lacks the independent authority blocker: %+v", evidence.Redistribution.Blockers)
	}
}

func TestVideoCoreSupplyChainEvidenceCannotOverwriteProtectedInputs(t *testing.T) {
	targets := []string{
		"manifest.json",
		"manifest.schema.json",
		"include/libavcodec/avcodec.h",
		"lib/avcodec.lib",
		"lib/libavcodec.dll.a",
		"bin/avcodec-63.dll",
		"bin/ffmpeg.exe",
	}
	for _, relativePath := range targets {
		t.Run(strings.ReplaceAll(relativePath, "/", "_"), func(t *testing.T) {
			fixture := newSupplyFixture(t, false)
			target := filepath.Join(fixture.Root, filepath.FromSlash(relativePath))
			before, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			result := runSupplyVerifier(t, fixture.ManifestPath, fixture.Root, "Local", target)
			if result.ExitCode != 4 || !strings.Contains(result.Output, "EVIDENCE_PATH_PROTECTED") {
				t.Fatalf("protected evidence destination exit=%d, want 4 and stable code:\n%s",
					result.ExitCode, result.Output)
			}
			after, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatalf("protected input %s was modified", relativePath)
			}
		})
	}
}

func TestVideoCoreSupplyChainEvidenceWriteFailureExitsFour(t *testing.T) {
	fixture := newSupplyFixture(t, false)
	evidenceDirectory := t.TempDir()
	result := runSupplyVerifier(t, fixture.ManifestPath, fixture.Root, "Local", evidenceDirectory)
	if result.ExitCode != 4 {
		t.Fatalf("evidence write failure exit=%d, want 4:\n%s", result.ExitCode, result.Output)
	}
}

func TestVideoCoreSupplyChainSDKInputAliasCannotTargetEvidence(t *testing.T) {
	fixture := newSupplyFixture(t, false)
	sdkInput := filepath.Join(fixture.Root, "bin", "avcodec-63.dll")
	evidencePath := filepath.Join(t.TempDir(), "external-evidence-target.json")
	sentinel := []byte("external evidence target sentinel\n")
	if err := os.WriteFile(evidencePath, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(sdkInput); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(evidencePath, sdkInput); err != nil {
		t.Fatalf("create SDK input hard-link alias to Evidence target: %v", err)
	}

	result := runSupplyVerifier(t, fixture.ManifestPath, fixture.Root, "Local", evidencePath)
	if result.ExitCode != supplyExitValidationFailed {
		t.Fatalf("SDK input alias exit=%d, want %d:\n%s",
			result.ExitCode, supplyExitValidationFailed, result.Output)
	}
	if !strings.Contains(result.Output, "EVIDENCE_INPUT_IDENTITY_COLLISION") {
		t.Fatalf("SDK input alias failure lacks stable collision code:\n%s", result.Output)
	}
	for _, path := range []string{evidencePath, sdkInput} {
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(sentinel) {
			t.Fatalf("SDK input alias allowed Evidence write to modify %s", path)
		}
	}
}

func TestVideoCoreSupplyChainMissingManifestCannotBypassEvidenceProtection(t *testing.T) {
	fixture := newSupplyFixture(t, false)
	protectedInput := filepath.Join(fixture.Root, "include", "libavcodec", "avcodec.h")
	before, err := os.ReadFile(protectedInput)
	if err != nil {
		t.Fatal(err)
	}

	result := runSupplyVerifier(
		t,
		filepath.Join(fixture.Root, "missing-manifest.json"),
		fixture.Root,
		"Local",
		protectedInput,
	)
	after, err := os.ReadFile(protectedInput)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("missing manifest allowed Evidence to modify an SDK input")
	}
	if result.ExitCode != 4 || !strings.Contains(result.Output, "EVIDENCE_PATH_PROTECTED") {
		t.Fatalf("missing manifest bypass exit=%d, want 4 and protected-path code:\n%s",
			result.ExitCode, result.Output)
	}
}

func TestVideoCoreSupplyChainMissingManifestAliasesCannotBypassEvidenceProtection(t *testing.T) {
	testCases := []struct {
		name string
		args func(t *testing.T, fixture *supplyFixture, protectedInput string) (manifest, root, evidence string)
	}{
		{
			name: "junction root with target evidence",
			args: func(t *testing.T, fixture *supplyFixture, protectedInput string) (string, string, string) {
				t.Helper()
				junction := filepath.Join(t.TempDir(), "ffmpeg-junction")
				output, err := exec.Command(
					"cmd.exe", "/d", "/c", "mklink", "/J", junction, fixture.Root,
				).CombinedOutput()
				if err != nil {
					t.Fatalf("create test junction: %v\n%s", err, output)
				}
				return filepath.Join(junction, "missing-manifest.json"), junction, protectedInput
			},
		},
		{
			name: "extended namespace evidence",
			args: func(t *testing.T, fixture *supplyFixture, protectedInput string) (string, string, string) {
				t.Helper()
				return filepath.Join(fixture.Root, "missing-manifest.json"), fixture.Root, `\\?\` + protectedInput
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newSupplyFixture(t, false)
			protectedInput := filepath.Join(fixture.Root, "include", "libavcodec", "avcodec.h")
			before, err := os.ReadFile(protectedInput)
			if err != nil {
				t.Fatal(err)
			}
			manifest, root, evidence := testCase.args(t, fixture, protectedInput)
			result := runSupplyVerifier(t, manifest, root, "Local", evidence)
			after, err := os.ReadFile(protectedInput)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatalf("%s allowed Evidence alias to modify an SDK input", testCase.name)
			}
			if result.ExitCode != 4 {
				t.Fatalf("%s exit=%d, want 4:\n%s", testCase.name, result.ExitCode, result.Output)
			}
		})
	}
}

func TestVideoCoreSupplyChainHiddenUnlistedRuntimeDLLIsInClosure(t *testing.T) {
	fixture := newSupplyFixture(t, false)
	hiddenDLL := filepath.Join(fixture.Root, "bin", "hidden-extra.dll")
	if err := os.WriteFile(hiddenDLL, []byte("hidden unlisted DLL"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("attrib.exe", "+H", hiddenDLL).CombinedOutput(); err != nil {
		t.Fatalf("mark hidden DLL: %v\n%s", err, output)
	}

	evidencePath := filepath.Join(t.TempDir(), "evidence.json")
	result := runSupplyVerifier(t, fixture.ManifestPath, fixture.Root, "Local", evidencePath)
	if result.ExitCode != supplyExitValidationFailed {
		t.Fatalf("hidden unlisted DLL exit=%d, want %d:\n%s",
			result.ExitCode, supplyExitValidationFailed, result.Output)
	}
	assertSupplyFailureNoEvidence(t, result, evidencePath, "UNLISTED_REQUIRED_FILE")
}

func TestVideoCoreSupplyChainRejectsReparseControlPaths(t *testing.T) {
	for _, testCase := range []struct {
		name string
		run  func(t *testing.T, fixture *supplyFixture, junction string) supplyVerifierResult
	}{
		{
			name: "ffmpeg root junction",
			run: func(t *testing.T, fixture *supplyFixture, junction string) supplyVerifierResult {
				return runSupplyVerifier(
					t,
					filepath.Join(junction, "manifest.json"),
					junction,
					"Local",
					filepath.Join(t.TempDir(), "evidence.json"),
				)
			},
		},
		{
			name: "manifest and schema junction",
			run: func(t *testing.T, fixture *supplyFixture, junction string) supplyVerifierResult {
				return runSupplyVerifier(
					t,
					filepath.Join(junction, "manifest.json"),
					fixture.Root,
					"Local",
					filepath.Join(t.TempDir(), "evidence.json"),
				)
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newSupplyFixture(t, false)
			junction := filepath.Join(t.TempDir(), "ffmpeg-junction")
			output, err := exec.Command(
				"cmd.exe", "/d", "/c", "mklink", "/J", junction, fixture.Root,
			).CombinedOutput()
			if err != nil {
				t.Fatalf("create test junction: %v\n%s", err, output)
			}
			result := testCase.run(t, fixture, junction)
			if result.ExitCode != 4 || !strings.Contains(result.Output, "REPARSE_POINT_FORBIDDEN") {
				t.Fatalf("reparse control path exit=%d, want 4 and stable code:\n%s",
					result.ExitCode, result.Output)
			}
		})
	}
}

type supplyFixture struct {
	Root         string
	ManifestPath string
	Manifest     supplyManifest
}

func newSupplyFixture(t *testing.T, complete bool) *supplyFixture {
	t.Helper()
	repoRoot := supplyRepoRoot(t)
	root := t.TempDir()
	for _, directory := range []string{
		filepath.Join(root, "include", "libavcodec"),
		filepath.Join(root, "lib"),
		filepath.Join(root, "bin"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string][]byte{
		"include/libavcodec/avcodec.h": []byte("synthetic header"),
		"lib/avcodec.lib":              []byte("synthetic MSVC import library"),
		"lib/libavcodec.dll.a":         []byte("synthetic GNU import library"),
		"bin/avcodec-63.dll":           []byte("synthetic runtime DLL"),
	}
	kinds := map[string]string{
		"include/libavcodec/avcodec.h": "header",
		"lib/avcodec.lib":              "msvc-import-lib",
		"lib/libavcodec.dll.a":         "gnu-import-lib",
		"bin/avcodec-63.dll":           "runtime-dll",
	}
	manifestFiles := make([]supplyManifestFile, 0, len(files))
	for _, path := range []string{
		"include/libavcodec/avcodec.h",
		"lib/avcodec.lib",
		"lib/libavcodec.dll.a",
		"bin/avcodec-63.dll",
	} {
		content := files[path]
		fullPath := filepath.Join(root, filepath.FromSlash(path))
		if err := os.WriteFile(fullPath, content, 0o600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(content)
		manifestFiles = append(manifestFiles, supplyManifestFile{
			Path: path, Kind: kinds[path], Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:]),
		})
	}
	if err := copySupplyFile(
		filepath.Join(repoRoot, "third_party", "ffmpeg", "manifest.schema.json"),
		filepath.Join(root, "manifest.schema.json"),
	); err != nil {
		t.Fatal(err)
	}
	documentContents := map[string][]byte{
		"LICENSE.txt": []byte("Synthetic test license evidence.\n"),
		"NOTICE.md":   []byte("# Synthetic test notice\n"),
		"SOURCE.md":   []byte("# Synthetic test source offer\n"),
	}
	evidenceDocuments := make([]supplyEvidenceDocument, 0, 3)
	for _, document := range []struct {
		role string
		path string
	}{
		{role: "source", path: "SOURCE.md"},
		{role: "license", path: "LICENSE.txt"},
		{role: "notice", path: "NOTICE.md"},
	} {
		content := documentContents[document.path]
		if err := os.WriteFile(filepath.Join(root, document.path), content, 0o600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(content)
		evidenceDocuments = append(evidenceDocuments, supplyEvidenceDocument{
			Role: document.role, Path: document.path, Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:]),
		})
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "ffmpeg.exe"), []byte("synthetic executable"), 0o600); err != nil {
		t.Fatal(err)
	}

	sourceURL := "https://example.invalid/ffmpeg-source.tar.xz"
	commit := strings.Repeat("1", 40)
	archiveSHA := strings.Repeat("2", 64)
	repositoryManifestContent, err := os.ReadFile(filepath.Join(repoRoot, "third_party", "ffmpeg", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var repositoryManifest supplyManifest
	if err := json.Unmarshal(repositoryManifestContent, &repositoryManifest); err != nil {
		t.Fatal(err)
	}
	manifest := supplyManifest{
		SchemaVersion: 1,
		SDKID:         "N-125444-g6d72600a30-20260703",
		Provenance: supplyProvenance{
			SourceURL: &sourceURL, Version: "synthetic-1.0", Commit: &commit,
			ConfigureFlags:      append([]string(nil), repositoryManifest.Provenance.ConfigureFlags...),
			SourceArchiveSHA256: &archiveSHA,
			SourceDocument:      "SOURCE.md",
		},
		Components: append([]supplyComponent(nil), repositoryManifest.Components...),
		License: supplyLicense{
			Classification: "gpl-3.0-or-later",
			LicenseFile:    "LICENSE.txt", NoticeFile: "NOTICE.md", EvidenceComplete: complete,
		},
		EvidenceDocuments: evidenceDocuments,
		Redistributable:   complete,
		Blockers:          []supplyBlocker{},
		Files:             manifestFiles,
	}
	if !complete {
		manifest.Redistributable = false
		manifest.License.EvidenceComplete = false
		manifest.Blockers = []supplyBlocker{{Code: "TEST_EVIDENCE_INCOMPLETE", Message: "synthetic blocker"}}
	}
	fixture := &supplyFixture{
		Root: root, ManifestPath: filepath.Join(root, "manifest.json"), Manifest: manifest,
	}
	fixture.writeManifest(t)
	return fixture
}

func (fixture *supplyFixture) writeManifest(t *testing.T) {
	t.Helper()
	content, err := json.MarshalIndent(fixture.Manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	content = append(content, '\n')
	if err := os.WriteFile(fixture.ManifestPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

type supplyVerifierResult struct {
	ExitCode int
	Output   string
}

func runSupplyVerifier(t *testing.T, manifestPath, ffmpegRoot, mode, evidencePath string) supplyVerifierResult {
	t.Helper()
	repoRoot := supplyRepoRoot(t)
	pwsh, err := exec.LookPath("pwsh.exe")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(
		pwsh, "-NoProfile", "-File",
		filepath.Join(repoRoot, "scripts", "verify_videocore_supply_chain.ps1"),
		"-Manifest", manifestPath,
		"-FFmpegRoot", ffmpegRoot,
		"-Mode", mode,
		"-Evidence", evidencePath,
	)
	command.Dir = repoRoot
	output, err := command.CombinedOutput()
	exitCode := 0
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			t.Fatalf("start supply-chain verifier: %v\n%s", err, output)
		}
		exitCode = exitError.ExitCode()
	}
	return supplyVerifierResult{ExitCode: exitCode, Output: string(output)}
}

func loadSupplyEvidence(t *testing.T, path string) supplyEvidence {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read evidence %s: %v", path, err)
	}
	var evidence supplyEvidence
	if err := json.Unmarshal(content, &evidence); err != nil {
		t.Fatalf("parse evidence %s: %v\n%s", path, err, content)
	}
	return evidence
}

func assertSupplyFailureNoEvidence(t *testing.T, result supplyVerifierResult, evidencePath, wantCode string) {
	t.Helper()
	if !strings.Contains(result.Output, wantCode) {
		t.Fatalf("verifier output missing error code %q:\n%s", wantCode, result.Output)
	}
	if _, err := os.Stat(evidencePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("validation failure wrote Evidence %s: %v", evidencePath, err)
	}
}

func copySupplyFile(source, destination string) error {
	content, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := os.WriteFile(destination, content, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", destination, err)
	}
	return nil
}

func supplyRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}
