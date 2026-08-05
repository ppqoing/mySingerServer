package integration

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type videoCoreCompatEvidence struct {
	SchemaVersion       int      `json:"schema_version"`
	Status              string   `json:"status"`
	Pass                bool     `json:"pass"`
	ExitCode            int      `json:"exit_code"`
	ManifestSHA256      string   `json:"manifest_sha256"`
	GoldenSHA256        string   `json:"golden_sha256"`
	StageManifestSHA256 string   `json:"stage_manifest_sha256"`
	Fixtures            int      `json:"fixtures"`
	Differences         int      `json:"differences"`
	DiffFile            string   `json:"diff_file"`
	Errors              []string `json:"errors"`
}

type videoCoreCompatDiff struct {
	SchemaVersion int    `json:"schema_version"`
	Status        string `json:"status"`
	Differences   int    `json:"differences"`
	Diffs         []struct {
		FixturePath string `json:"fixture_path"`
		JSONPath    string `json:"json_path"`
		Expected    any    `json:"expected"`
		Actual      any    `json:"actual"`
	} `json:"diffs"`
}

func TestVideoCoreCompatibilityGate(t *testing.T) {
	repo := testRepositoryRoot(t)
	root := t.TempDir()
	manifestPath := filepath.Join(root, "manifest.json")
	goldenPath := filepath.Join(root, "legacy-golden.json")
	stageDir := filepath.Join(root, "stage")
	mustMkdirAll(t, stageDir)
	mustWrite(t, filepath.Join(stageDir, "release-manifest.json"), []byte(`{"schema_version":1,"files":[]}`))
	mustWrite(t, manifestPath, []byte(`{"schemaVersion":1,"images":[{"path":"images/synthetic.jpg","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","mediaType":"image","codec":"jpeg","durationMicros":0,"rotation":0,"sar":"1:1","scenarios":["synthetic"]}],"videos":[]}`))
	golden := []byte(`{"schemaVersion":1,"fixtures":[{"path":"images/synthetic.jpg","sha512":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","image":{"width":16,"height":8,"pdqHex":"01","quality":100,"pHashPartsHex":["02"],"sobelFloatBitsHex":["03"]}}]}`)
	mustWrite(t, goldenPath, golden)

	t.Run("exact result passes with zero differences", func(t *testing.T) {
		runner := writeVideoCoreCompatFakeRunner(t, golden)
		evidencePath := filepath.Join(root, "pass", "compat.json")
		result := runPowerShell(t,
			filepath.Join(repo, "scripts", "verify_videocore_compat.ps1"),
			"-Manifest", manifestPath,
			"-Golden", goldenPath,
			"-StageDir", stageDir,
			"-Evidence", evidencePath,
			"-Runner", runner,
		)
		if result.exitCode != 0 {
			t.Fatalf("compat exit=%d, want 0:\n%s", result.exitCode, result.output)
		}
		var evidence videoCoreCompatEvidence
		decodeJSONFile(t, evidencePath, &evidence)
		if evidence.SchemaVersion != 1 || evidence.Status != "pass" || !evidence.Pass ||
			evidence.ExitCode != 0 || evidence.Fixtures != 1 || evidence.Differences != 0 {
			t.Fatalf("unexpected pass evidence: %+v", evidence)
		}
		for name, digest := range map[string]string{
			"manifest":       evidence.ManifestSHA256,
			"golden":         evidence.GoldenSHA256,
			"stage manifest": evidence.StageManifestSHA256,
		} {
			if len(digest) != 64 {
				t.Fatalf("%s digest=%q, want SHA-256", name, digest)
			}
		}
		var diff videoCoreCompatDiff
		decodeJSONFile(t, filepath.Join(filepath.Dir(evidencePath), "compat-diff.json"), &diff)
		if diff.Status != "pass" || diff.Differences != 0 || len(diff.Diffs) != 0 {
			t.Fatalf("unexpected zero-diff report: %+v", diff)
		}
	})

	t.Run("one changed feature byte fails with an exact diff", func(t *testing.T) {
		actual := []byte(strings.Replace(string(golden), `"pdqHex":"01"`, `"pdqHex":"ff"`, 1))
		runner := writeVideoCoreCompatFakeRunner(t, actual)
		evidencePath := filepath.Join(root, "fail", "compat.json")
		result := runPowerShell(t,
			filepath.Join(repo, "scripts", "verify_videocore_compat.ps1"),
			"-Manifest", manifestPath,
			"-Golden", goldenPath,
			"-StageDir", stageDir,
			"-Evidence", evidencePath,
			"-Runner", runner,
		)
		if result.exitCode != 1 {
			t.Fatalf("mutated compat exit=%d, want 1:\n%s", result.exitCode, result.output)
		}
		var evidence videoCoreCompatEvidence
		decodeJSONFile(t, evidencePath, &evidence)
		if evidence.Status != "fail" || evidence.Pass || evidence.ExitCode != 1 || evidence.Differences != 1 {
			t.Fatalf("unexpected failure evidence: %+v", evidence)
		}
		var diff videoCoreCompatDiff
		decodeJSONFile(t, filepath.Join(filepath.Dir(evidencePath), "compat-diff.json"), &diff)
		if diff.Differences != 1 || len(diff.Diffs) != 1 {
			t.Fatalf("unexpected diff count: %+v", diff)
		}
		got := diff.Diffs[0]
		if got.FixturePath != "images/synthetic.jpg" || got.JSONPath != "$.fixtures[0].image.pdqHex" ||
			got.Expected != "01" || got.Actual != "ff" {
			t.Fatalf("diff is not exact: %+v", got)
		}
		if !strings.Contains(result.output, got.JSONPath) {
			t.Fatalf("console output lacks exact JSON path:\n%s", result.output)
		}
	})

	t.Run("JSON scalar type change is an exact difference", func(t *testing.T) {
		actual := []byte(strings.Replace(string(golden), `"quality":100`, `"quality":"100"`, 1))
		runner := writeVideoCoreCompatFakeRunner(t, actual)
		evidencePath := filepath.Join(root, "type-fail", "compat.json")
		result := runPowerShell(t,
			filepath.Join(repo, "scripts", "verify_videocore_compat.ps1"),
			"-Manifest", manifestPath,
			"-Golden", goldenPath,
			"-StageDir", stageDir,
			"-Evidence", evidencePath,
			"-Runner", runner,
		)
		if result.exitCode != 1 {
			t.Fatalf("type-mutated compat exit=%d, want 1:\n%s", result.exitCode, result.output)
		}
		var diff videoCoreCompatDiff
		decodeJSONFile(t, filepath.Join(filepath.Dir(evidencePath), "compat-diff.json"), &diff)
		if diff.Differences != 1 || len(diff.Diffs) != 1 ||
			diff.Diffs[0].JSONPath != "$.fixtures[0].image.quality" {
			t.Fatalf("JSON type mutation was not reported exactly: %+v", diff)
		}
	})
}

func writeVideoCoreCompatFakeRunner(t *testing.T, actual []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-compat-runner.ps1")
	payload := base64.StdEncoding.EncodeToString(actual)
	source := fmt.Sprintf(`param(
    [Parameter(Mandatory=$true)][string]$Manifest,
    [Parameter(Mandatory=$true)][string]$StageDir,
    [Parameter(Mandatory=$true)][string]$OutFile
)
$ErrorActionPreference = "Stop"
foreach ($value in @($Manifest, $StageDir, $OutFile)) {
    if (-not [IO.Path]::IsPathFullyQualified($value)) { throw "FAKE_RUNNER_PATH_NOT_ABSOLUTE" }
}
$json = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('%s'))
[IO.Directory]::CreateDirectory([IO.Path]::GetDirectoryName($OutFile)) | Out-Null
[IO.File]::WriteAllText($OutFile, $json, [Text.UTF8Encoding]::new($false))
`, payload)
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

var _ = json.Valid
