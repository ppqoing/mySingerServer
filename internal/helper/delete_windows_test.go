package helper

import (
	"context"
	"crypto/sha512"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"dedup/internal/proto"
	"golang.org/x/sys/windows"
)

const processorTaskID = "abcdef12-3456-4789-8abc-def012345678"

func TestProcessorFrameValidationPrecedenceAndAllOrNothing(t *testing.T) {
	fixture := newLocalProcessorFixture(t, nil)
	first := fixture.writeFile(t, "first.bin", "first")
	second := fixture.writeFile(t, "second.bin", "second")
	noMutation := func(string) error {
		t.Fatal("mutation reached for an invalid frame")
		return nil
	}
	fixture.processor.ops.remove = noMutation
	fixture.processor.ops.rename = func(string, string) error {
		return noMutation("")
	}

	tests := []struct {
		name string
		task proto.DeleteTask
		code string
	}{
		{
			name: "confirmation wins over mode and structure",
			task: proto.DeleteTask{
				TaskID:    "not-a-uuid",
				Seq:       2,
				LastSeq:   1,
				Mode:      "erase",
				Confirmed: false,
				Entries:   []string{first, second},
			},
			code: proto.DeleteErrNotConfirmed,
		},
		{
			name: "mode wins over structure",
			task: proto.DeleteTask{
				TaskID:    "not-a-uuid",
				Seq:       2,
				LastSeq:   1,
				Mode:      "erase",
				Confirmed: true,
				Entries:   []string{first, second},
			},
			code: proto.DeleteErrBadMode,
		},
		{
			name: "hard disabled",
			task: proto.DeleteTask{
				TaskID:    processorTaskID,
				Mode:      proto.ModeHard,
				Confirmed: true,
				Entries:   []string{first, second},
			},
			code: proto.DeleteErrBadMode,
		},
	}
	fixture.processor.cfg.AllowHardDelete = false
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := fixture.processor.Process(context.Background(), tt.task)
			requireProcessorReport(t, tt.task, report, repeatCode(tt.code, len(tt.task.Entries)))
			requireFilesExist(t, tt.task.Entries...)
		})
	}
}

func TestProcessorRejectsStructuralFramesBeforeMutation(t *testing.T) {
	fixture := newLocalProcessorFixture(t, nil)
	file := fixture.writeFile(t, "frame.bin", "frame")
	noMutation := func(string) error {
		t.Fatal("mutation reached for a structurally invalid frame")
		return nil
	}
	fixture.processor.ops.remove = noMutation
	fixture.processor.ops.rename = func(string, string) error {
		return noMutation("")
	}

	oversize := make([]string, 2001)
	for index := range oversize {
		oversize[index] = fmt.Sprintf(`Z:\frame-%04d.bin`, index)
	}
	duplicate := strings.ToUpper(strings.ReplaceAll(file, `\`, `/`)) + "/."
	tests := []struct {
		name string
		task proto.DeleteTask
	}{
		{
			name: "non canonical task id",
			task: validProcessorTask([]string{file}, proto.ModeHard),
		},
		{
			name: "nil task id",
			task: validProcessorTask([]string{file}, proto.ModeHard),
		},
		{
			name: "sequence exceeds envelope",
			task: validProcessorTask([]string{file}, proto.ModeHard),
		},
		{
			name: "empty entries",
			task: validProcessorTask(nil, proto.ModeHard),
		},
		{
			name: "oversize entries",
			task: validProcessorTask(oversize, proto.ModeHard),
		},
		{
			name: "empty entry",
			task: validProcessorTask([]string{file, ""}, proto.ModeHard),
		},
		{
			name: "normalized duplicate",
			task: validProcessorTask([]string{file, duplicate}, proto.ModeHard),
		},
	}
	tests[0].task.TaskID = strings.ToUpper(processorTaskID)
	tests[1].task.TaskID = "00000000-0000-0000-0000-000000000000"
	tests[2].task.Seq = 2
	tests[2].task.LastSeq = 1

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := fixture.processor.Process(context.Background(), tt.task)
			requireProcessorReport(
				t,
				tt.task,
				report,
				repeatCode(proto.DeleteErrBadPath, len(tt.task.Entries)),
			)
			requireFilesExist(t, file)
		})
	}
}

func TestProcessorMalformedPathFailsPerItemAndLaterHardDeleteContinues(t *testing.T) {
	fixture := newLocalProcessorFixture(t, nil)
	valid := fixture.writeFile(t, "later.bin", "later")
	task := validProcessorTask(
		[]string{`relative\malformed.bin`, valid},
		proto.ModeHard,
	)

	report := fixture.processor.Process(context.Background(), task)

	requireProcessorReport(
		t,
		task,
		report,
		[]string{proto.DeleteErrBadPath, ""},
	)
	requireFilesMissing(t, valid)
	if report.Entries[0].Path != task.Entries[0] ||
		report.Entries[1].Path != task.Entries[1] {
		t.Fatalf("report path order changed: %#v", report.Entries)
	}
}

func TestProcessorOmittedModeSoftDeletesOnSubstVolumePreservesHashAndCollisions(t *testing.T) {
	fixture := newSubstProcessorFixture(t)
	fixture.processor.cfg.DefaultMode = proto.ModeHard
	source := filepath.Join(fixture.sourceRoot, "album", "photo.bin")
	expectedBase := filepath.Join(
		fixture.driveRoot,
		fixture.cfg.RecycleDirName,
		processorTaskID,
		"source",
		"album",
		"photo.bin",
	)
	contents := []string{"first-content", "second-content", "third-content"}
	expectedDestinations := []string{
		expectedBase,
		strings.TrimSuffix(expectedBase, ".bin") + "_1.bin",
		strings.TrimSuffix(expectedBase, ".bin") + "_2.bin",
	}

	for index, content := range contents {
		writeProcessorFile(t, source, content)
		before := hashProcessorFile(t, source)
		task := validProcessorTask([]string{source}, "")
		report := fixture.processor.Process(context.Background(), task)

		requireProcessorReport(t, task, report, []string{""})
		result := report.Entries[0]
		if result.RecycledTo != expectedDestinations[index] {
			t.Fatalf(
				"collision %d RecycledTo = %q, want %q",
				index,
				result.RecycledTo,
				expectedDestinations[index],
			)
		}
		if filepath.VolumeName(result.RecycledTo) != filepath.VolumeName(source) {
			t.Fatalf("soft delete crossed volumes: %q -> %q", source, result.RecycledTo)
		}
		requireFilesMissing(t, source)
		requireFilesExist(t, result.RecycledTo)
		if after := hashProcessorFile(t, result.RecycledTo); after != before {
			t.Fatalf("SHA-512 changed across soft delete: %x != %x", after, before)
		}
	}
}

func TestProcessorSoftDeleteDoesNotOverwriteCollisionCreatedAfterPrecheck(t *testing.T) {
	fixture := newSubstProcessorFixture(t)
	source := fixture.writeFile(t, "race.bin", "source-content")
	baseDestination := filepath.Join(
		fixture.driveRoot,
		fixture.cfg.RecycleDirName,
		processorTaskID,
		"source",
		"race.bin",
	)
	nextDestination := testCollisionPath(baseDestination, 1)
	realRename := fixture.processor.ops.rename
	injected := false
	fixture.processor.ops.rename = func(from, to string) error {
		if !injected {
			injected = true
			writeProcessorFile(t, to, "collision-content")
		}
		return realRename(from, to)
	}
	task := validProcessorTask([]string{source}, proto.ModeSoft)

	report := fixture.processor.Process(context.Background(), task)

	requireProcessorReport(t, task, report, []string{""})
	if !injected {
		t.Fatal("test did not create a collision after the destination precheck")
	}
	if got := report.Entries[0].RecycledTo; got != nextDestination {
		t.Fatalf("RecycledTo = %q, want next collision %q", got, nextDestination)
	}
	if got := string(readProcessorFile(t, baseDestination)); got != "collision-content" {
		t.Fatalf("collision content = %q, want it preserved", got)
	}
	if got := string(readProcessorFile(t, nextDestination)); got != "source-content" {
		t.Fatalf("recycled source content = %q, want original source", got)
	}
	requireFilesMissing(t, source)
}

func TestProcessorSoftDeleteFailsClosedBeforeMutationOnVolumeIdentityFailure(t *testing.T) {
	tests := []struct {
		name     string
		volumeID func(string) (string, error)
	}{
		{
			name: "mismatch",
			volumeID: func(path string) (string, error) {
				if strings.Contains(path, `$Task3Recycle`) {
					return `\\?\Volume{22222222-2222-2222-2222-222222222222}\`, nil
				}
				return `\\?\Volume{11111111-1111-1111-1111-111111111111}\`, nil
			},
		},
		{
			name: "lookup failure",
			volumeID: func(string) (string, error) {
				return "", errors.New("volume identity lookup failed")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newSubstProcessorFixture(t)
			source := fixture.writeFile(t, "volume-first.bin", "source")
			fixture.processor.ops.volumeID = tt.volumeID
			fixture.processor.ops.mkdir = func(string, fs.FileMode) error {
				t.Fatal("mkdir reached after initial volume identity failure")
				return nil
			}
			fixture.processor.ops.rename = func(string, string) error {
				t.Fatal("move reached after initial volume identity failure")
				return nil
			}
			task := validProcessorTask([]string{source}, proto.ModeSoft)

			report := fixture.processor.Process(context.Background(), task)

			requireProcessorReport(
				t,
				task,
				report,
				[]string{proto.DeleteErrRecycleFailed},
			)
			requireFilesExist(t, source)
		})
	}
}

func TestProcessorSoftDeleteFailsClosedOnFinalVolumeIdentityMismatch(t *testing.T) {
	fixture := newSubstProcessorFixture(t)
	source := fixture.writeFile(t, "volume-final.bin", "source")
	volumeCalls := 0
	fixture.processor.ops.volumeID = func(string) (string, error) {
		volumeCalls++
		switch volumeCalls {
		case 1, 2, 3:
			return `\\?\Volume{11111111-1111-1111-1111-111111111111}\`, nil
		case 4:
			return `\\?\Volume{22222222-2222-2222-2222-222222222222}\`, nil
		default:
			t.Fatalf("unexpected volume identity lookup %d", volumeCalls)
			return "", nil
		}
	}
	realMkdir := fixture.processor.ops.mkdir
	mkdirCalls := 0
	fixture.processor.ops.mkdir = func(path string, mode fs.FileMode) error {
		mkdirCalls++
		return realMkdir(path, mode)
	}
	fixture.processor.ops.rename = func(string, string) error {
		t.Fatal("move reached after final volume identity mismatch")
		return nil
	}
	task := validProcessorTask([]string{source}, proto.ModeSoft)

	report := fixture.processor.Process(context.Background(), task)

	requireProcessorReport(
		t,
		task,
		report,
		[]string{proto.DeleteErrRecycleFailed},
	)
	if mkdirCalls == 0 {
		t.Fatal("test did not reach destination parent mutation before final check")
	}
	if volumeCalls != 4 {
		t.Fatalf("volume identity lookups = %d, want 4", volumeCalls)
	}
	requireFilesExist(t, source)
}

func TestProcessorDefaultVolumeIdentityMatchesPhysicalAndSubstAlias(t *testing.T) {
	fixture := newSubstProcessorFixture(t)
	aliasPath := fixture.writeFile(t, "volume-alias.bin", "source")
	physicalPath := filepath.Join(fixture.base, "source", "volume-alias.bin")

	aliasID, err := resolveVolumeID(aliasPath)
	if err != nil {
		t.Fatalf("resolve alias volume identity: %v", err)
	}
	physicalID, err := resolveVolumeID(physicalPath)
	if err != nil {
		t.Fatalf("resolve physical volume identity: %v", err)
	}

	if aliasID == "" || physicalID == "" {
		t.Fatalf("empty volume identity: alias=%q physical=%q", aliasID, physicalID)
	}
	if !ordinalEqualFold(aliasID, physicalID) {
		t.Fatalf(
			"same file resolved to different volumes: alias=%q physical=%q",
			aliasID,
			physicalID,
		)
	}
}

func TestProcessorDOSAliasTargetValidation(t *testing.T) {
	safeTarget := `\??\C:\run-temp`
	tests := []struct {
		name    string
		alias   string
		targets []string
		want    string
	}{
		{
			name:    "safe local drive absolute",
			alias:   `Y:\source\file.bin`,
			targets: []string{safeTarget},
			want:    `C:\run-temp\source\file.bin`,
		},
		{
			name:    "non letter target drive",
			alias:   `Y:\source\file.bin`,
			targets: []string{`\??\1:\run-temp`},
		},
		{
			name:    "non ASCII target drive",
			alias:   `Y:\source\file.bin`,
			targets: []string{`\??\É:\run-temp`},
		},
		{
			name:    "target drive relative",
			alias:   `Y:\source\file.bin`,
			targets: []string{`\??\C:run-temp`},
		},
		{
			name:    "target root relative",
			alias:   `Y:\source\file.bin`,
			targets: []string{`\??\\run-temp`},
		},
		{
			name:    "target UNC",
			alias:   `Y:\source\file.bin`,
			targets: []string{`\??\\server\share\run-temp`},
		},
		{
			name:    "target UNC namespace",
			alias:   `Y:\source\file.bin`,
			targets: []string{`\??\UNC\server\share\run-temp`},
		},
		{
			name:    "target volume GUID namespace",
			alias:   `Y:\source\file.bin`,
			targets: []string{`\??\\?\Volume{11111111-1111-1111-1111-111111111111}\run-temp`},
		},
		{
			name:    "target device namespace",
			alias:   `Y:\source\file.bin`,
			targets: []string{`\??\\.\C:\run-temp`},
		},
		{
			name:    "target NT device",
			alias:   `Y:\source\file.bin`,
			targets: []string{`\Device\HarddiskVolume1\run-temp`},
		},
		{
			name:    "target missing prefix",
			alias:   `Y:\source\file.bin`,
			targets: []string{`C:\run-temp`},
		},
		{
			name:    "target unsafe trailing dot",
			alias:   `Y:\source\file.bin`,
			targets: []string{`\??\C:\run-temp.`},
		},
		{
			name:    "target unsafe trailing space",
			alias:   `Y:\source\file.bin`,
			targets: []string{`\??\C:\run-temp `},
		},
		{
			name:    "target unsafe short alias",
			alias:   `Y:\source\file.bin`,
			targets: []string{`\??\C:\RUN-TE~1`},
		},
		{
			name:    "ambiguous multiple targets",
			alias:   `Y:\source\file.bin`,
			targets: []string{safeTarget, `\??\D:\prior-target`},
		},
		{
			name:    "non letter alias drive",
			alias:   `1:\source\file.bin`,
			targets: []string{safeTarget},
		},
		{
			name:    "non ASCII alias drive",
			alias:   `É:\source\file.bin`,
			targets: []string{safeTarget},
		},
		{
			name:    "alias drive relative",
			alias:   `Y:source\file.bin`,
			targets: []string{safeTarget},
		},
		{
			name:    "alias root relative",
			alias:   `\source\file.bin`,
			targets: []string{safeTarget},
		},
		{
			name:    "alias UNC",
			alias:   `\\server\share\file.bin`,
			targets: []string{safeTarget},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			targetBuffer, targetLength := processorMultiSZ(t, tt.targets...)

			got, err := expandDOSAliasTarget(
				tt.alias,
				targetBuffer,
				targetLength,
			)

			if tt.want == "" {
				if err == nil {
					t.Fatalf("expandDOSAliasTarget() = %q, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("expandDOSAliasTarget(): %v", err)
			}
			if got != tt.want {
				t.Fatalf("expandDOSAliasTarget() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProcessorSoftDeleteStopsAtCollisionLimit(t *testing.T) {
	fixture := newSubstProcessorFixture(t)
	source := fixture.writeFile(t, "conflict.bin", "source")
	baseDestination := filepath.Join(
		fixture.driveRoot,
		fixture.cfg.RecycleDirName,
		processorTaskID,
		"source",
		"conflict.bin",
	)
	for collision := 0; collision <= maxRecycleCollisions; collision++ {
		writeProcessorFile(
			t,
			testCollisionPath(baseDestination, collision),
			fmt.Sprintf("collision-%d", collision),
		)
	}
	task := validProcessorTask([]string{source}, proto.ModeSoft)

	report := fixture.processor.Process(context.Background(), task)

	requireProcessorReport(t, task, report, []string{proto.DeleteErrRecycleFailed})
	requireFilesExist(t, source)
	if report.Entries[0].RecycledTo != "" {
		t.Fatalf("failed collision reported destination %q", report.Entries[0].RecycledTo)
	}
}

func TestProcessorHardDeleteClearsReadonlyAndPreservesOtherAttributes(t *testing.T) {
	fixture := newLocalProcessorFixture(t, nil)
	normal := fixture.writeFile(t, "normal.bin", "normal")
	readonly := fixture.writeFile(t, "readonly.bin", "readonly")
	setProcessorAttributes(
		t,
		normal,
		windows.FILE_ATTRIBUTE_ARCHIVE|windows.FILE_ATTRIBUTE_HIDDEN,
	)
	readonlyBefore := uint32(
		windows.FILE_ATTRIBUTE_ARCHIVE |
			windows.FILE_ATTRIBUTE_HIDDEN |
			windows.FILE_ATTRIBUTE_READONLY,
	)
	setProcessorAttributes(t, readonly, readonlyBefore)
	normalBefore, err := getFileAttributes(normal)
	if err != nil {
		t.Fatal(err)
	}
	attributesAtRemove := make(map[string]uint32)
	realRemove := fixture.processor.ops.remove
	fixture.processor.ops.remove = func(path string) error {
		attributes, err := getFileAttributes(path)
		if err != nil {
			return err
		}
		attributesAtRemove[path] = attributes
		return realRemove(path)
	}
	task := validProcessorTask([]string{normal, readonly}, proto.ModeHard)

	report := fixture.processor.Process(context.Background(), task)

	requireProcessorReport(t, task, report, []string{"", ""})
	requireFilesMissing(t, normal, readonly)
	if report.Entries[0].ReadonlyCleared {
		t.Fatal("normal file reported readonly_cleared")
	}
	if !report.Entries[1].ReadonlyCleared {
		t.Fatal("readonly file did not report readonly_cleared")
	}
	if attributesAtRemove[normal] != normalBefore {
		t.Fatalf(
			"normal attributes before remove = %#x, want %#x",
			attributesAtRemove[normal],
			normalBefore,
		)
	}
	if want := readonlyBefore &^ windows.FILE_ATTRIBUTE_READONLY; attributesAtRemove[readonly] != want {
		t.Fatalf(
			"readonly attributes before remove = %#x, want %#x",
			attributesAtRemove[readonly],
			want,
		)
	}
}

func TestProcessorMapsValidationFailuresAndContinues(t *testing.T) {
	fixture := newLocalProcessorFixture(t, []string{"denied"})
	missing := filepath.Join(fixture.sourceRoot, "missing.bin")
	denied := fixture.writeFile(t, filepath.Join("denied", "secret.bin"), "secret")
	valid := fixture.writeFile(t, "valid.bin", "valid")
	task := validProcessorTask([]string{missing, denied, valid}, proto.ModeHard)

	report := fixture.processor.Process(context.Background(), task)

	requireProcessorReport(
		t,
		task,
		report,
		[]string{proto.DeleteErrNotFound, proto.DeleteErrPathDenied, ""},
	)
	requireFilesExist(t, denied)
	requireFilesMissing(t, valid)
}

func TestProcessorMapsRealReparseAndInUseFailures(t *testing.T) {
	t.Run("source reparse", func(t *testing.T) {
		fixture := newLocalProcessorFixture(t, nil)
		target := filepath.Join(fixture.base, "junction-target")
		if err := os.MkdirAll(target, 0o700); err != nil {
			t.Fatal(err)
		}
		junction := filepath.Join(fixture.sourceRoot, "junction")
		createJunction(t, junction, target)
		task := validProcessorTask([]string{junction}, proto.ModeHard)

		report := fixture.processor.Process(context.Background(), task)

		requireProcessorReport(t, task, report, []string{proto.DeleteErrReparse})
		requireFilesExist(t, junction)
	})

	t.Run("exclusive handle", func(t *testing.T) {
		fixture := newLocalProcessorFixture(t, nil)
		path := fixture.writeFile(t, "locked.bin", "locked")
		handle := openExclusiveProcessorHandle(t, path)
		t.Cleanup(func() {
			if err := windows.CloseHandle(handle); err != nil {
				t.Errorf("CloseHandle: %v", err)
			}
		})
		task := validProcessorTask([]string{path}, proto.ModeHard)

		report := fixture.processor.Process(context.Background(), task)

		requireProcessorReport(t, task, report, []string{proto.DeleteErrInUse})
		requireFilesExist(t, path)
	})
}

func TestProcessorMapsHardMutationErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code string
	}{
		{"missing", windows.ERROR_FILE_NOT_FOUND, proto.DeleteErrNotFound},
		{"path missing", windows.ERROR_PATH_NOT_FOUND, proto.DeleteErrNotFound},
		{"sharing", windows.ERROR_SHARING_VIOLATION, proto.DeleteErrInUse},
		{"lock", windows.ERROR_LOCK_VIOLATION, proto.DeleteErrInUse},
		{"access", windows.ERROR_ACCESS_DENIED, proto.DeleteErrAccessDenied},
		{"fallback", errors.New("remove failure"), proto.DeleteErrDeleteFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newLocalProcessorFixture(t, nil)
			path := fixture.writeFile(t, "remove.bin", "remove")
			fixture.processor.ops.remove = func(string) error { return tt.err }
			task := validProcessorTask([]string{path}, proto.ModeHard)

			report := fixture.processor.Process(context.Background(), task)

			requireProcessorReport(t, task, report, []string{tt.code})
			requireFilesExist(t, path)
		})
	}

	t.Run("readonly clear fallback", func(t *testing.T) {
		fixture := newLocalProcessorFixture(t, nil)
		path := fixture.writeFile(t, "readonly.bin", "readonly")
		setProcessorAttributes(
			t,
			path,
			windows.FILE_ATTRIBUTE_ARCHIVE|windows.FILE_ATTRIBUTE_READONLY,
		)
		fixture.processor.ops.setAttributes = func(string, uint32) error {
			return errors.New("readonly clear failure")
		}
		task := validProcessorTask([]string{path}, proto.ModeHard)

		report := fixture.processor.Process(context.Background(), task)

		requireProcessorReport(t, task, report, []string{proto.DeleteErrReadonly})
		requireFilesExist(t, path)
	})

	t.Run("readonly clear access denied remains readonly", func(t *testing.T) {
		fixture := newLocalProcessorFixture(t, nil)
		path := fixture.writeFile(t, "readonly-access.bin", "readonly")
		setProcessorAttributes(
			t,
			path,
			windows.FILE_ATTRIBUTE_ARCHIVE|windows.FILE_ATTRIBUTE_READONLY,
		)
		fixture.processor.ops.setAttributes = func(string, uint32) error {
			return windows.ERROR_ACCESS_DENIED
		}
		task := validProcessorTask([]string{path}, proto.ModeHard)

		report := fixture.processor.Process(context.Background(), task)

		requireProcessorReport(t, task, report, []string{proto.DeleteErrReadonly})
		requireFilesExist(t, path)
	})

	t.Run("remove failure reports completed readonly clear", func(t *testing.T) {
		fixture := newLocalProcessorFixture(t, nil)
		path := fixture.writeFile(t, "readonly-remove.bin", "readonly")
		setProcessorAttributes(
			t,
			path,
			windows.FILE_ATTRIBUTE_ARCHIVE|windows.FILE_ATTRIBUTE_READONLY,
		)
		fixture.processor.ops.remove = func(string) error {
			return errors.New("remove after readonly clear")
		}
		task := validProcessorTask([]string{path}, proto.ModeHard)

		report := fixture.processor.Process(context.Background(), task)

		requireProcessorReport(t, task, report, []string{proto.DeleteErrDeleteFailed})
		if !report.Entries[0].ReadonlyCleared {
			t.Fatal("failure after successful attribute change lost readonly_cleared metadata")
		}
		attributes, err := getFileAttributes(path)
		if err != nil {
			t.Fatal(err)
		}
		if attributes&windows.FILE_ATTRIBUTE_READONLY != 0 {
			t.Fatalf("readonly attribute remains after reported clear: %#x", attributes)
		}
	})
}

func TestProcessorMapsSoftMutationErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code string
	}{
		{"missing", windows.ERROR_FILE_NOT_FOUND, proto.DeleteErrNotFound},
		{"path missing", windows.ERROR_PATH_NOT_FOUND, proto.DeleteErrNotFound},
		{"sharing", windows.ERROR_SHARING_VIOLATION, proto.DeleteErrInUse},
		{"lock", windows.ERROR_LOCK_VIOLATION, proto.DeleteErrInUse},
		{"access", windows.ERROR_ACCESS_DENIED, proto.DeleteErrAccessDenied},
		{"fallback", errors.New("rename failure"), proto.DeleteErrRecycleFailed},
	}
	fixture := newSubstProcessorFixture(t)
	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := fixture.writeFile(
				t,
				fmt.Sprintf("rename-%d.bin", index),
				"rename",
			)
			fixture.processor.ops.rename = func(string, string) error { return tt.err }
			task := validProcessorTask([]string{path}, proto.ModeSoft)

			report := fixture.processor.Process(context.Background(), task)

			requireProcessorReport(t, task, report, []string{tt.code})
			requireFilesExist(t, path)
		})
	}
}

func TestProcessorRejectsExistingRecycleReparseAndSourceSwap(t *testing.T) {
	t.Run("existing recycle ancestor", func(t *testing.T) {
		fixture := newSubstProcessorFixture(t)
		source := fixture.writeFile(t, "destination-reparse.bin", "source")
		recycleRoot := filepath.Join(fixture.driveRoot, fixture.cfg.RecycleDirName)
		target := filepath.Join(fixture.driveRoot, "junction-target")
		if err := os.MkdirAll(target, 0o700); err != nil {
			t.Fatal(err)
		}
		createJunction(t, recycleRoot, target)
		task := validProcessorTask([]string{source}, proto.ModeSoft)

		report := fixture.processor.Process(context.Background(), task)

		requireProcessorReport(t, task, report, []string{proto.DeleteErrReparse})
		requireFilesExist(t, source)
	})

	t.Run("source revalidated before first destination mutation", func(t *testing.T) {
		fixture := newSubstProcessorFixture(t)
		source := fixture.writeFile(t, "ordered.bin", "source")
		realRevalidate := fixture.processor.ops.revalidateSource
		revalidations := 0
		fixture.processor.ops.revalidateSource = func(
			previous ValidatedPath,
		) (ValidatedPath, error) {
			revalidations++
			return realRevalidate(previous)
		}
		realMkdir := fixture.processor.ops.mkdir
		mkdirCalls := 0
		fixture.processor.ops.mkdir = func(path string, mode fs.FileMode) error {
			mkdirCalls++
			if revalidations == 0 {
				t.Fatal("destination mutation preceded source revalidation")
			}
			return realMkdir(path, mode)
		}
		realRename := fixture.processor.ops.rename
		fixture.processor.ops.rename = func(from, to string) error {
			if revalidations < 2 {
				t.Fatalf(
					"move preceded final source revalidation: got %d calls",
					revalidations,
				)
			}
			return realRename(from, to)
		}
		task := validProcessorTask([]string{source}, proto.ModeSoft)

		report := fixture.processor.Process(context.Background(), task)

		requireProcessorReport(t, task, report, []string{""})
		if mkdirCalls == 0 {
			t.Fatal("test did not exercise destination mutation")
		}
	})

	t.Run("source swap after preflight", func(t *testing.T) {
		fixture := newSubstProcessorFixture(t)
		source := fixture.writeFile(t, "swap.bin", "source")
		target := filepath.Join(fixture.driveRoot, "swap-target")
		if err := os.MkdirAll(target, 0o700); err != nil {
			t.Fatal(err)
		}
		realMkdir := fixture.processor.ops.mkdir
		swapped := false
		fixture.processor.ops.mkdir = func(path string, mode fs.FileMode) error {
			err := realMkdir(path, mode)
			if err == nil && !swapped {
				swapped = true
				if removeErr := os.Remove(source); removeErr != nil {
					t.Fatalf("remove source for swap: %v", removeErr)
				}
				createJunction(t, source, target)
			}
			return err
		}
		task := validProcessorTask([]string{source}, proto.ModeSoft)

		report := fixture.processor.Process(context.Background(), task)

		requireProcessorReport(t, task, report, []string{proto.DeleteErrReparse})
		if !swapped {
			t.Fatal("test did not swap source between preflight and mutation")
		}
		requireFilesExist(t, source)
	})
}

func TestProcessorCancellationPreservesCompletedAndSkipsRemaining(t *testing.T) {
	fixture := newLocalProcessorFixture(t, nil)
	paths := []string{
		fixture.writeFile(t, "first.bin", "first"),
		fixture.writeFile(t, "second.bin", "second"),
		fixture.writeFile(t, "third.bin", "third"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	realRemove := fixture.processor.ops.remove
	removeCalls := 0
	fixture.processor.ops.remove = func(path string) error {
		removeCalls++
		err := realRemove(path)
		if removeCalls == 1 {
			cancel()
		}
		return err
	}
	task := validProcessorTask(paths, proto.ModeHard)

	report := fixture.processor.Process(ctx, task)

	requireProcessorReport(
		t,
		task,
		report,
		[]string{"", proto.DeleteErrDeleteFailed, proto.DeleteErrDeleteFailed},
	)
	if removeCalls != 1 {
		t.Fatalf("remove calls after cancellation = %d, want 1", removeCalls)
	}
	requireFilesMissing(t, paths[0])
	requireFilesExist(t, paths[1], paths[2])
}

type processorFixture struct {
	base       string
	driveRoot  string
	sourceRoot string
	cfg        Config
	processor  *Processor
}

func newLocalProcessorFixture(t *testing.T, deniedRelative []string) *processorFixture {
	t.Helper()
	base := runTempDir(t)
	sourceRoot := filepath.Join(base, "source")
	if err := os.MkdirAll(sourceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	denied := make([]string, 0, len(deniedRelative))
	for _, relative := range deniedRelative {
		denied = append(denied, filepath.Join(sourceRoot, relative))
	}
	cfg := processorConfig(sourceRoot, denied)
	validator, err := NewValidator(cfg)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	return &processorFixture{
		base:       base,
		driveRoot:  filepath.VolumeName(base) + `\`,
		sourceRoot: sourceRoot,
		cfg:        cfg,
		processor:  NewProcessor(cfg, validator),
	}
}

func newSubstProcessorFixture(t *testing.T) *processorFixture {
	t.Helper()
	physicalRoot := runTempDir(t)
	driveRoot := mountProcessorSubst(t, physicalRoot)
	sourceRoot := filepath.Join(driveRoot, "source")
	if err := os.MkdirAll(sourceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := processorConfig(sourceRoot, nil)
	cfg.DefaultMode = proto.ModeHard
	validator, err := NewValidator(cfg)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	return &processorFixture{
		base:       physicalRoot,
		driveRoot:  driveRoot,
		sourceRoot: sourceRoot,
		cfg:        cfg,
		processor:  NewProcessor(cfg, validator),
	}
}

func processorConfig(sourceRoot string, denied []string) Config {
	return Config{
		AllowedRoots:       []string{sourceRoot},
		DeniedRoots:        denied,
		DefaultMode:        proto.ModeSoft,
		AllowHardDelete:    true,
		RecycleDirName:     "$Task3Recycle",
		MaxEntriesPerFrame: 2000,
	}
}

func (f *processorFixture) writeFile(t *testing.T, relative, content string) string {
	t.Helper()
	path := filepath.Join(f.sourceRoot, relative)
	writeProcessorFile(t, path, content)
	return path
}

func mountProcessorSubst(t *testing.T, physicalRoot string) string {
	t.Helper()
	candidates := []byte("ZYXWVUTSRQPONMLKJGFED")
	var failures []string
	for _, letter := range candidates {
		drive := string(letter) + ":"
		output, err := exec.Command("subst.exe", drive, physicalRoot).CombinedOutput()
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v: %s", drive, err, output))
			continue
		}
		driveRoot := drive + `\`
		t.Cleanup(func() {
			entries, readErr := os.ReadDir(physicalRoot)
			if readErr != nil {
				t.Errorf("read subst physical root before cleanup: %v", readErr)
			} else {
				for _, entry := range entries {
					target := filepath.Join(physicalRoot, entry.Name())
					if removeErr := os.RemoveAll(target); removeErr != nil {
						t.Errorf("remove test-owned subst residue %q: %v", target, removeErr)
					}
				}
			}
			remaining, readErr := os.ReadDir(physicalRoot)
			if readErr != nil {
				t.Errorf("read subst physical root after cleanup: %v", readErr)
			} else if len(remaining) != 0 {
				t.Errorf("subst physical root residue: %#v", remaining)
			}
			output, unmountErr := exec.Command("subst.exe", drive, "/D").CombinedOutput()
			if unmountErr != nil {
				t.Errorf("unmount subst %s: %v\n%s", drive, unmountErr, output)
			}
			if _, statErr := os.Stat(driveRoot); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("subst drive still reachable after unmount: %v", statErr)
			}
		})
		return driveRoot
	}
	t.Fatalf(
		"NEEDS_CONTEXT: no dynamic subst drive available without touching H: or I:\n%s",
		strings.Join(failures, "\n"),
	)
	return ""
}

func validProcessorTask(entries []string, mode string) proto.DeleteTask {
	return proto.DeleteTask{
		TaskID:    processorTaskID,
		Seq:       0,
		LastSeq:   0,
		Mode:      mode,
		Confirmed: true,
		Entries:   entries,
	}
}

func requireProcessorReport(
	t *testing.T,
	task proto.DeleteTask,
	report proto.DeleteReport,
	codes []string,
) {
	t.Helper()
	if report.TaskID != task.TaskID ||
		report.Seq != task.Seq ||
		report.LastSeq != task.LastSeq {
		t.Fatalf("report envelope = %#v, task = %#v", report, task)
	}
	if len(report.Entries) != len(task.Entries) || len(report.Entries) != len(codes) {
		t.Fatalf(
			"report entries = %d, task entries = %d, codes = %d",
			len(report.Entries),
			len(task.Entries),
			len(codes),
		)
	}
	okCount := 0
	for index, code := range codes {
		result := report.Entries[index]
		if result.Path != task.Entries[index] {
			t.Fatalf("entry %d path = %q, want %q", index, result.Path, task.Entries[index])
		}
		if code == "" {
			okCount++
			if !result.OK || result.ErrCode != "" || result.Err != "" {
				t.Fatalf("entry %d success metadata = %#v", index, result)
			}
		} else {
			if result.OK || result.ErrCode != code || result.Err == "" {
				t.Fatalf("entry %d failure metadata = %#v, want code %q", index, result, code)
			}
			if result.RecycledTo != "" {
				t.Fatalf("entry %d failure leaked recycle metadata: %#v", index, result)
			}
		}
	}
	wantStats := proto.DeleteStats{
		Total:  len(codes),
		OK:     okCount,
		Failed: len(codes) - okCount,
	}
	if report.Stats != wantStats {
		t.Fatalf("stats = %#v, want %#v", report.Stats, wantStats)
	}
}

func repeatCode(code string, count int) []string {
	result := make([]string, count)
	for index := range result {
		result[index] = code
	}
	return result
}

func writeProcessorFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func hashProcessorFile(t *testing.T, path string) [sha512.Size]byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha512.Sum512(content)
}

func readProcessorFile(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func processorMultiSZ(t *testing.T, values ...string) ([]uint16, uint32) {
	t.Helper()
	result := make([]uint16, 0)
	for _, value := range values {
		encoded, err := windows.UTF16FromString(value)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, encoded...)
	}
	result = append(result, 0)
	return result, uint32(len(result))
}

func requireFilesExist(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("expected %q to exist: %v", path, err)
		}
	}
}

func requireFilesMissing(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expected %q to be missing, got %v", path, err)
		}
	}
}

func testCollisionPath(path string, collision int) string {
	if collision == 0 {
		return path
	}
	extension := filepath.Ext(path)
	return strings.TrimSuffix(path, extension) +
		fmt.Sprintf("_%d", collision) +
		extension
}

func setProcessorAttributes(t *testing.T, path string, attributes uint32) {
	t.Helper()
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetFileAttributes(pathUTF16, attributes); err != nil {
		t.Fatalf("SetFileAttributes(%q, %#x): %v", path, attributes, err)
	}
}

func openExclusiveProcessorHandle(t *testing.T, path string) windows.Handle {
	t.Helper()
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(
		pathUTF16,
		windows.GENERIC_READ,
		0,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatalf("CreateFile exclusive %q: %v", path, err)
	}
	return handle
}
