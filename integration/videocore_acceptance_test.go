package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type videoCoreAcceptanceEvidence struct {
	SchemaVersion       int    `json:"schema_version"`
	RunID               string `json:"run_id"`
	Status              string `json:"status"`
	Pass                bool   `json:"pass"`
	StageManifestSHA256 string `json:"stage_manifest_sha256"`
	AC1                 struct {
		Status          string `json:"status"`
		Pass            bool   `json:"pass"`
		EmptyPath       bool   `json:"empty_path"`
		AgentPID        int    `json:"agent_pid"`
		DecoderChildren int    `json:"decoder_children"`
	} `json:"ac1"`
	AC2     map[string]any `json:"ac2"`
	AC3     map[string]any `json:"ac3"`
	Cleanup struct {
		ResidualPIDs []int `json:"residual_pids"`
	} `json:"cleanup"`
	Errors []string `json:"errors"`
}

func TestVideoCoreAcceptanceHarnessContract(t *testing.T) {
	repo := testRepositoryRoot(t)
	root := t.TempDir()
	stageDir := filepath.Join(root, "stage")
	corpusDir := filepath.Join(root, "corpus")
	mustMkdirAll(t, stageDir)
	mustMkdirAll(t, corpusDir)
	manifestBytes := []byte(`{"schema_version":1,"files":[]}`)
	mustWrite(t, filepath.Join(stageDir, "release-manifest.json"), manifestBytes)
	mustWrite(t, filepath.Join(stageDir, "agent.exe"), nil)
	mustWrite(t, filepath.Join(stageDir, "worker.exe"), nil)
	digest := sha256.Sum256(manifestBytes)
	stageDigest := hex.EncodeToString(digest[:])

	secret := "postgres://contract-user:contract-password@127.0.0.1/contract"
	previous, hadPrevious := os.LookupEnv("FS_PG_DSN")
	if err := os.Setenv("FS_PG_DSN", secret); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadPrevious {
			_ = os.Setenv("FS_PG_DSN", previous)
		} else {
			_ = os.Unsetenv("FS_PG_DSN")
		}
	})

	t.Run("valid synthetic evidence passes without leaking the DSN", func(t *testing.T) {
		runner := writeVideoCoreAcceptanceFakeRunner(t, stageDigest, true, secret)
		evidenceDir := filepath.Join(root, "pass-evidence")
		result := runPowerShell(t,
			filepath.Join(repo, "scripts", "verify_videocore_acceptance.ps1"),
			"-StageDir", stageDir,
			"-CorpusDir", corpusDir,
			"-EvidenceDir", evidenceDir,
			"-Runner", runner,
		)
		if result.exitCode != 0 {
			t.Fatalf("acceptance exit=%d, want 0:\n%s", result.exitCode, result.output)
		}
		for _, name := range []string{"acceptance.json", "process-tree.jsonl", "ready.jsonl", "runner.log"} {
			if _, err := os.Stat(filepath.Join(evidenceDir, name)); err != nil {
				t.Fatalf("required evidence %s: %v", name, err)
			}
		}
		var evidence videoCoreAcceptanceEvidence
		decodeJSONFile(t, filepath.Join(evidenceDir, "acceptance.json"), &evidence)
		if evidence.SchemaVersion != 1 || evidence.Status != "pass" || !evidence.Pass ||
			evidence.StageManifestSHA256 != stageDigest || !evidence.AC1.Pass ||
			!evidence.AC1.EmptyPath || evidence.AC1.DecoderChildren != 0 ||
			len(evidence.Cleanup.ResidualPIDs) != 0 {
			t.Fatalf("unexpected acceptance evidence: %+v", evidence)
		}
		all := readTreeText(t, evidenceDir)
		if strings.Contains(all, secret) || strings.Contains(result.output, secret) {
			t.Fatal("acceptance evidence or console leaked FS_PG_DSN")
		}
	})

	t.Run("missing runner is blocked and cannot pass", func(t *testing.T) {
		evidenceDir := filepath.Join(root, "blocked-evidence")
		result := runPowerShell(t,
			filepath.Join(repo, "scripts", "verify_videocore_acceptance.ps1"),
			"-StageDir", stageDir,
			"-CorpusDir", corpusDir,
			"-EvidenceDir", evidenceDir,
		)
		if result.exitCode == 0 || !strings.Contains(result.output, "BLOCKED_NOT_RUN") || strings.Contains(result.output, "ACCEPTANCE PASS") {
			t.Fatalf("missing runner did not fail closed: exit=%d\n%s", result.exitCode, result.output)
		}
	})

	t.Run("missing required evidence cannot pass", func(t *testing.T) {
		runner := writeVideoCoreAcceptanceFakeRunner(t, stageDigest, false, "")
		evidenceDir := filepath.Join(root, "missing-evidence")
		result := runPowerShell(t,
			filepath.Join(repo, "scripts", "verify_videocore_acceptance.ps1"),
			"-StageDir", stageDir,
			"-CorpusDir", corpusDir,
			"-EvidenceDir", evidenceDir,
			"-Runner", runner,
		)
		if result.exitCode == 0 || strings.Contains(result.output, "ACCEPTANCE PASS") {
			t.Fatalf("missing ready.jsonl did not fail closed: exit=%d\n%s", result.exitCode, result.output)
		}
	})

	t.Run("decoder child in process tree cannot pass", func(t *testing.T) {
		runner := writeVideoCoreAcceptanceFakeRunner(t, stageDigest, true, "", true)
		evidenceDir := filepath.Join(root, "decoder-evidence")
		result := runPowerShell(t,
			filepath.Join(repo, "scripts", "verify_videocore_acceptance.ps1"),
			"-StageDir", stageDir,
			"-CorpusDir", corpusDir,
			"-EvidenceDir", evidenceDir,
			"-Runner", runner,
		)
		if result.exitCode == 0 || strings.Contains(result.output, "ACCEPTANCE PASS") {
			t.Fatalf("decoder child evidence did not fail closed: exit=%d\n%s", result.exitCode, result.output)
		}
	})
}

func writeVideoCoreAcceptanceFakeRunner(t *testing.T, stageDigest string, includeReady bool, leakedText string, includeDecoder ...bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-acceptance-runner.ps1")
	readyWrite := ""
	if includeReady {
		readyWrite = `[IO.File]::WriteAllText((Join-Path $EvidenceDir "ready.jsonl"), '{"pid":202,"event":"replacement_ready"}' + [Environment]::NewLine, [Text.UTF8Encoding]::new($false))`
	}
	decoderWrite := ""
	if len(includeDecoder) > 0 && includeDecoder[0] {
		decoderWrite = `$rows += [ordered]@{pid=301;parent_pid=101;image_path='C:\Windows\System32\ffmpeg.exe';creation_time_utc='2026-08-01T00:00:02Z'}`
	}
	acceptanceJSON := fmt.Sprintf(`{"schema_version":1,"run_id":"synthetic-contract","status":"pass","pass":true,"stage_manifest_sha256":"%s","ac1":{"status":"pass","pass":true,"empty_path":true,"agent_pid":101,"decoder_children":0},"ac2":{"status":"pass","pass":true,"agent_pid_before":101,"agent_pid_after":101,"old_worker_pid":201,"old_worker_exited":true,"replacement_worker_pid":202,"replacement_ready":true,"fault_task_failed":true,"followup_done":true},"ac3":{"status":"pass","pass":true,"agent_pid_before":101,"agent_pid_after":101,"old_worker_pid":202,"old_worker_exited":true,"replacement_worker_pid":203,"replacement_ready":true,"fault_task_failed":true,"followup_done":true,"watchdog_ms":120000},"cleanup":{"tracked_pids":[101,201,202,203],"residual_pids":[]},"errors":[]}`, stageDigest)
	source := fmt.Sprintf(`param(
    [Parameter(Mandatory=$true)][string]$StageDir,
    [Parameter(Mandatory=$true)][string]$CorpusDir,
    [Parameter(Mandatory=$true)][string]$EvidenceDir
)
$ErrorActionPreference = "Stop"
foreach ($value in @($StageDir, $CorpusDir, $EvidenceDir)) {
    if (-not [IO.Path]::IsPathFullyQualified($value)) { throw "FAKE_RUNNER_PATH_NOT_ABSOLUTE" }
}
[IO.Directory]::CreateDirectory($EvidenceDir) | Out-Null
$acceptance = '%s' | ConvertFrom-Json
$emptyPathRequired = $env:VIDEOCORE_ACCEPTANCE_FORCE_EMPTY_PATH -eq '1'
$acceptance.ac1.empty_path = $emptyPathRequired
[IO.File]::WriteAllText((Join-Path $EvidenceDir "acceptance.json"), ($acceptance | ConvertTo-Json -Depth 16 -Compress), [Text.UTF8Encoding]::new($false))
$rows = @(
    [ordered]@{pid=101;parent_pid=0;image_path=(Join-Path $StageDir 'agent.exe');creation_time_utc='2026-08-01T00:00:00Z';path_empty=$emptyPathRequired},
    [ordered]@{pid=202;parent_pid=101;image_path=(Join-Path $StageDir 'worker.exe');creation_time_utc='2026-08-01T00:00:01Z';path_empty=$emptyPathRequired}
)
%s
$treeText = (($rows | ForEach-Object { $_ | ConvertTo-Json -Compress }) -join [Environment]::NewLine) + [Environment]::NewLine
[IO.File]::WriteAllText((Join-Path $EvidenceDir "process-tree.jsonl"), $treeText, [Text.UTF8Encoding]::new($false))
%s
Write-Output '%s'
`, strings.ReplaceAll(acceptanceJSON, "'", "''"), decoderWrite, readyWrite, strings.ReplaceAll(leakedText, "'", "''"))
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readTreeText(t *testing.T, root string) string {
	t.Helper()
	var combined strings.Builder
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		combined.Write(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return combined.String()
}
