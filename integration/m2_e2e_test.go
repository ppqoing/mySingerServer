//go:build windows

package integration_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type m2CorpusManifest struct {
	Version string            `json:"version"`
	Seed    string            `json:"seed"`
	Counts  map[string]int    `json:"counts"`
	Sources map[string]string `json:"sources"`
	Files   []struct {
		Path           string `json:"path"`
		SHA512         string `json:"sha512"`
		Class          string `json:"class"`
		Classification string `json:"classification"`
		Size           int64  `json:"size"`
	} `json:"files"`
}

func TestM2VerifierFailsClosedWithMissingDependencies(t *testing.T) {
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	pwsh, err := exec.LookPath("pwsh.exe")
	if err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing.exe")
	command := exec.Command(
		pwsh,
		"-NoProfile",
		"-File", filepath.Join(repoRoot, "scripts", "verify_m2.ps1"),
		"-Go", missing,
		"-GCC", missing,
		"-Dlltool", missing,
		"-CMake", missing,
		"-VcpkgRoot", filepath.Join(t.TempDir(), "missing-vcpkg"),
		"-Dumpbin", missing,
		"-PGDSN", "postgres://invalid",
		"-PreflightOnly",
	)
	command.Dir = repoRoot
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("verifier with missing dependencies exited zero:\n%s", output)
	}
	text := string(output)
	for ac := 1; ac <= 8; ac++ {
		want := "AC-" + string(rune('0'+ac)) + " FAIL"
		if !strings.Contains(text, want) {
			t.Fatalf("verifier output missing %q:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "M2 VERIFY FAIL") {
		t.Fatalf("verifier did not report final fail:\n%s", text)
	}
}

func TestM2NativeTimeoutContractWorksInWindowsPowerShell5(t *testing.T) {
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(
		"powershell.exe",
		"-NoProfile",
		"-File", filepath.Join(repoRoot, "scripts", "verify_m2_native.ps1"),
		"-TimeoutContract",
	)
	command.Dir = repoRoot
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Windows PowerShell 5 timeout contract: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "NATIVE TIMEOUT CONTRACT PASS") {
		t.Fatalf("timeout contract proof missing:\n%s", output)
	}
}

func TestM2VerifierRunsTimeoutContractInPowerShell5AndPwsh(t *testing.T) {
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(
		"powershell.exe",
		"-NoProfile",
		"-File", filepath.Join(repoRoot, "scripts", "verify_m2.ps1"),
		"-TimeoutContractsOnly",
	)
	command.Dir = repoRoot
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("dual timeout contracts: %v\n%s", err, output)
	}
	text := string(output)
	for _, proof := range []string{
		"TIMEOUT CONTRACT powershell5 PASS",
		"TIMEOUT CONTRACT pwsh PASS",
	} {
		if !strings.Contains(text, proof) {
			t.Fatalf("dual timeout output missing %q:\n%s", proof, output)
		}
	}
	if strings.Count(text, "NATIVE TIMEOUT CONTRACT PASS") != 2 {
		t.Fatalf("native timeout contract did not run twice:\n%s", output)
	}
}

func TestM2VerifierScriptContainsRequiredFullGates(t *testing.T) {
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := os.ReadFile(filepath.Join(repoRoot, "scripts", "verify_m2.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	cgoHelper, err := os.ReadFile(filepath.Join(repoRoot, "scripts", "test-cgo.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"-Packages @('./...')",
		"-VetOnly",
		"TestPGRemoteUpsertFilesMatchesCentralSchemaWhenIntegrationEnabled",
		"TestPostgresSyncIsIdempotentWhenIntegrationEnabled",
		"TestPGRemoteFeatureUpsertsAndCentralMigrationWhenIntegrationEnabled",
		"0614e71ed97f0b9792ff3677f86356a71caeb4205ac76590972161d2b84f7f8f",
		"5f3c767af1cdbb9c44ad14478ce5fc036aec20e6a724755caa2f70abb9655c3f",
		"ffmpeg version N-125444-g6d72600a30-20260703 Copyright (c) 2000-2026 the FFmpeg developers",
		"expectedDependencies",
		"Get-TrackedProcessSnapshot",
		"process_audit",
		"expectedPDQTreeSHA256",
		"Get-TreeSHA256",
	} {
		if !bytes.Contains(verifier, []byte(required)) {
			t.Errorf("verify_m2.ps1 missing required full gate %q", required)
		}
	}
	if !bytes.Contains(cgoHelper, []byte("[switch]$VetOnly")) {
		t.Error("test-cgo.ps1 does not expose the safe CGO vet gate")
	}
}

func TestM2PDQTreePinRejectsModificationAdditionDeletion(t *testing.T) {
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	verifier := filepath.Join(repoRoot, "scripts", "verify_m2.ps1")
	source := filepath.Join(repoRoot, "mediacore", "src", "pdq_upstream")
	run := func(root string) ([]byte, error) {
		command := exec.Command(
			"powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", verifier,
			"-PinContract", "-PDQTreeRoot", root,
		)
		command.Dir = repoRoot
		return command.CombinedOutput()
	}
	if output, err := run(source); err != nil {
		t.Fatalf("actual PDQ tree pin contract: %v\n%s", err, output)
	}
	for _, mutation := range []struct {
		name string
		edit func(*testing.T, string)
	}{
		{
			name: "modify source",
			edit: func(t *testing.T, root string) {
				t.Helper()
				path := filepath.Join(root, "COMMIT")
				if err := os.WriteFile(path, []byte("modified\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "add source",
			edit: func(t *testing.T, root string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(root, "added.cpp"), []byte("added\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "delete source",
			edit: func(t *testing.T, root string) {
				t.Helper()
				if err := os.Remove(filepath.Join(root, "COMMIT")); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			copyRoot := filepath.Join(t.TempDir(), "pdq_upstream")
			copyM2TestTree(t, source, copyRoot)
			mutation.edit(t, copyRoot)
			if output, err := run(copyRoot); err == nil {
				t.Fatalf("mutated PDQ tree passed pin contract:\n%s", output)
			}
		})
	}
}

func copyM2TestTree(t *testing.T, source, destination string) {
	t.Helper()
	err := filepath.Walk(source, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestM2CorpusGeneratorIsDeterministic(t *testing.T) {
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	goExe := filepath.Join(runtime.GOROOT(), "bin", "go.exe")
	ffmpeg := filepath.Join(repoRoot, "third_party", "ffmpeg", "bin", "ffmpeg.exe")
	for _, path := range []string{goExe, ffmpeg} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("required corpus dependency %s: %v", path, err)
		}
	}
	outputs := []string{
		filepath.Join(t.TempDir(), "first"),
		filepath.Join(t.TempDir(), "second"),
	}
	manifests := make([][]byte, 0, len(outputs))
	for _, output := range outputs {
		command := exec.Command(
			goExe,
			"run",
			filepath.Join(repoRoot, "testdata", "m2", "gen_corrupt.go"),
			"-out", output,
			"-ffmpeg", ffmpeg,
		)
		command.Dir = repoRoot
		combined, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("generate corpus: %v\n%s", err, combined)
		}
		manifestBytes, err := os.ReadFile(filepath.Join(output, "manifest.json"))
		if err != nil {
			t.Fatal(err)
		}
		manifests = append(manifests, manifestBytes)
		var manifest m2CorpusManifest
		if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
			t.Fatal(err)
		}
		if manifest.Version != "1" || manifest.Seed != "m2-corpus-seed-v1" {
			t.Fatalf("manifest identity = version:%q seed:%q", manifest.Version, manifest.Seed)
		}
		for key, want := range map[string]int{
			"corrupt_classes": 8,
			"smoke_images":    1000,
			"warmup_images":   2000,
			"single_images":   100,
			"single_videos":   20,
			"cache_videos":    10,
			"crash_images":    10,
			"hang_images":     1,
		} {
			if got := manifest.Counts[key]; got != want {
				t.Fatalf("manifest count %s = %d, want %d", key, got, want)
			}
		}
		if len(manifest.Sources) < 3 {
			t.Fatalf("manifest source hashes = %#v, want image/video seeds", manifest.Sources)
		}
		assertM2ManifestFiles(t, output, manifest)
	}
	if !bytes.Equal(manifests[0], manifests[1]) {
		t.Fatal("manifest differs across identical generator runs")
	}
}

func assertM2ManifestFiles(
	t *testing.T,
	output string,
	manifest m2CorpusManifest,
) {
	t.Helper()
	sawWrongExtension := false
	sawUnicode := false
	sawLongPath := false
	smokeHashes := make(map[string]string, 1000)
	warmupHashes := make(map[string]string, 2000)
	for _, file := range manifest.Files {
		fullPath := filepath.Join(output, filepath.FromSlash(file.Path))
		info, err := os.Stat(fullPath)
		if err != nil {
			t.Fatalf("manifest file %s: %v", file.Path, err)
		}
		if info.Size() != file.Size || len(file.SHA512) != 128 ||
			file.Class == "" || file.Classification == "" {
			t.Fatalf("invalid manifest entry: %#v", file)
		}
		switch {
		case strings.HasPrefix(file.Path, "smoke/"):
			if prior, exists := smokeHashes[file.SHA512]; exists {
				t.Fatalf("smoke files share content SHA-512: %s and %s", prior, file.Path)
			}
			smokeHashes[file.SHA512] = file.Path
		case strings.HasPrefix(file.Path, "warmup/"):
			if prior, exists := warmupHashes[file.SHA512]; exists {
				t.Fatalf("warmup files share content SHA-512: %s and %s", prior, file.Path)
			}
			if measured, exists := smokeHashes[file.SHA512]; exists {
				t.Fatalf("warmup file %s shares measured content with %s", file.Path, measured)
			}
			warmupHashes[file.SHA512] = file.Path
		case filepath.Base(fullPath) == "wrongext.png":
			sawWrongExtension = true
		case filepath.Base(fullPath) == "图片_😀 副本.jpg":
			sawUnicode = true
		case len(fullPath) > 260:
			sawLongPath = true
		}
	}
	if !sawWrongExtension || !sawUnicode || !sawLongPath {
		t.Fatalf("special corpus paths wrongext=%v unicode=%v long=%v",
			sawWrongExtension, sawUnicode, sawLongPath)
	}
	if len(smokeHashes) != 1000 {
		t.Fatalf("unique smoke SHA-512 count = %d, want 1000", len(smokeHashes))
	}
	if len(warmupHashes) != 2000 {
		t.Fatalf("unique warmup SHA-512 count = %d, want 2000", len(warmupHashes))
	}
}
