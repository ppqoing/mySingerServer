package helper

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"dedup/internal/proto"
	"golang.org/x/sys/windows"
)

func TestValidatorAcceptsCaseInsensitiveCleanSlashNormalizedFile(t *testing.T) {
	base := runTempDir(t)
	root := filepath.Join(base, "Media")
	path := filepath.Join(root, "Album", "track.bin")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	v := newTestValidator(t, strings.ToUpper(root), nil, uniqueRecycleName(t))
	input := strings.ReplaceAll(filepath.Join(root, "Album", ".", "track.bin"), `\`, `/`)

	got, err := v.ValidateFile(input)
	if err != nil {
		t.Fatalf("ValidateFile: %v", err)
	}
	wantPath := getLongExistingPath(t, filepath.Clean(path))
	if got.Path != wantPath {
		t.Fatalf("Path = %q, want %q", got.Path, wantPath)
	}
	if got.VolumeRoot != filepath.VolumeName(path)+`\` {
		t.Fatalf("VolumeRoot = %q", got.VolumeRoot)
	}
	if got.Relative != filepath.Join("Album", "track.bin") {
		t.Fatalf("Relative = %q", got.Relative)
	}
	if got.Attributes == 0 {
		t.Fatal("Attributes must contain the real Windows file attributes")
	}
}

func TestValidatorOrdinalContainmentDistinguishesKelvinSignSibling(t *testing.T) {
	base := runTempDir(t)
	allowed := filepath.Join(base, "K-media")
	kelvinSiblingFile := filepath.Join(base, "K-media", "file.bin")
	if err := os.MkdirAll(allowed, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, kelvinSiblingFile)
	v := newTestValidator(t, allowed, nil, uniqueRecycleName(t))

	requirePathCode(
		t,
		validateFileError(v, kelvinSiblingFile),
		proto.DeleteErrPathDenied,
	)
}

func TestValidatorOrdinalRelativePathDistinguishesKelvinAndReturnsDescendant(t *testing.T) {
	base := filepath.Join(runTempDir(t), "root")
	asciiRoot := filepath.Join(base, "K-media")
	kelvinSibling := filepath.Join(base, "K-media", "file.bin")
	if _, err := ordinalRelativePath(asciiRoot, kelvinSibling); err == nil {
		t.Fatalf("ordinalRelativePath accepted Kelvin-sign sibling %q", kelvinSibling)
	}

	descendant := filepath.Join(asciiRoot, "Album", "track.bin")
	relative, err := ordinalRelativePath(asciiRoot, descendant)
	if err != nil {
		t.Fatalf("ordinalRelativePath(descendant): %v", err)
	}
	if relative != `Album\track.bin` {
		t.Fatalf("relative path = %q, want %q", relative, `Album\track.bin`)
	}
}

func TestValidatorRejectsSiblingPrefixDotDotAndDeniedRootBeforeFilesystemAccess(t *testing.T) {
	base := runTempDir(t)
	root := filepath.Join(base, "media")
	denied := filepath.Join(root, "private")
	sibling := filepath.Join(base, "media-backup")
	for _, dir := range []string{root, denied, sibling} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	siblingFile := filepath.Join(sibling, "escape.bin")
	deniedFile := filepath.Join(denied, "secret.bin")
	for _, path := range []string{siblingFile, deniedFile} {
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	v := newTestValidator(t, root, []string{denied}, uniqueRecycleName(t))

	for _, path := range []string{
		siblingFile,
		filepath.Join(root, "..", filepath.Base(sibling), filepath.Base(siblingFile)),
		deniedFile,
		strings.ToUpper(deniedFile),
	} {
		err := validateFileError(v, path)
		requirePathCode(t, err, proto.DeleteErrPathDenied)
	}
}

func TestValidatorRejectsTrailingDotDeniedAliasBeforeFilesystemAccess(t *testing.T) {
	root := filepath.Join(runTempDir(t), "media")
	denied := filepath.Join(root, "private")
	file := filepath.Join(denied, "victim.bin")
	writeFixtureFile(t, file)
	v := newTestValidator(t, root, []string{denied}, uniqueRecycleName(t))

	requirePathCode(
		t,
		validateFileError(v, filepath.Join(root, "private.", "victim.bin")),
		proto.DeleteErrBadPath,
	)
}

func TestValidatorRejectsUnsafeWin32AliasComponentsForAllPathRoles(t *testing.T) {
	root := filepath.Join(runTempDir(t), "media")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	recycle := uniqueRecycleName(t)
	v := newTestValidator(t, root, nil, recycle)
	volumeRoot := filepath.VolumeName(root) + `\`

	for _, tt := range []struct {
		name     string
		validate func(string) error
		path     string
	}{
		{
			name:     "file internal trailing space",
			validate: func(path string) error { return validateFileError(v, path) },
			path:     filepath.Join(root, "unsafe ", "file.bin"),
		},
		{
			name:     "file tilde alias",
			validate: func(path string) error { return validateFileError(v, path) },
			path:     filepath.Join(root, "UNSAFE~1", "file.bin"),
		},
		{
			name:     "recycle trailing dot",
			validate: v.ValidateRecycleTarget,
			path:     filepath.Join(volumeRoot, recycle+".", "file.bin"),
		},
		{
			name:     "recycle tilde alias",
			validate: v.ValidateRecycleTarget,
			path:     filepath.Join(volumeRoot, recycle, "TASK~1", "file.bin"),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			requirePathCode(t, tt.validate(tt.path), proto.DeleteErrBadPath)
		})
	}
}

func TestValidatorRejectsRealShortPathAliasWhenVolumeProvidesOne(t *testing.T) {
	base := runTempDir(t)
	denied := filepath.Join(base, "long-directory-name-for-task-two")
	file := filepath.Join(denied, "file.bin")
	writeFixtureFile(t, file)
	shortFile, ok := getShortPath(t, file)
	if !ok {
		t.Skip("8.3 short names are unavailable for this run-unique directory")
	}
	v := newTestValidator(t, base, []string{denied}, uniqueRecycleName(t))

	requirePathCode(t, validateFileError(v, shortFile), proto.DeleteErrBadPath)
}

func TestValidatorRejectsMalformedDeviceAndRelativePaths(t *testing.T) {
	root := filepath.Join(runTempDir(t), "media")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	v := newTestValidator(t, root, nil, uniqueRecycleName(t))
	for _, path := range []string{
		"",
		`relative\file.bin`,
		`C:file.bin`,
		`\volume-relative\file.bin`,
		`\\server\share\file.bin`,
		`\\?\C:\file.bin`,
		`\\.\C:\file.bin`,
		`\??\C:\file.bin`,
	} {
		t.Run(strings.ReplaceAll(path, `\`, "_"), func(t *testing.T) {
			requirePathCode(t, validateFileError(v, path), proto.DeleteErrBadPath)
		})
	}
}

func TestValidatorRejectsMissingFilesAndDirectories(t *testing.T) {
	root := filepath.Join(runTempDir(t), "media")
	dir := filepath.Join(root, "directory")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	v := newTestValidator(t, root, nil, uniqueRecycleName(t))

	requirePathCode(
		t,
		validateFileError(v, filepath.Join(root, "missing.bin")),
		proto.DeleteErrNotFound,
	)
	requirePathCode(t, validateFileError(v, dir), proto.DeleteErrBadPath)
}

func TestValidatorRejectsReparseAtEveryExistingPathComponent(t *testing.T) {
	for _, position := range []string{"allowed root", "first ancestor", "deep ancestor", "target"} {
		t.Run(position, func(t *testing.T) {
			base := runTempDir(t)
			realRoot := filepath.Join(base, "allowed")
			if err := os.MkdirAll(realRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			targetDir := filepath.Join(base, "junction-target")
			if err := os.MkdirAll(targetDir, 0o700); err != nil {
				t.Fatal(err)
			}

			allowedRoot := realRoot
			var input string
			switch position {
			case "allowed root":
				file := filepath.Join(targetDir, "file.bin")
				writeFixtureFile(t, file)
				link := filepath.Join(base, "allowed-link")
				createJunction(t, link, targetDir)
				allowedRoot = link
				input = filepath.Join(link, "file.bin")
			case "first ancestor":
				file := filepath.Join(targetDir, "deep", "file.bin")
				writeFixtureFile(t, file)
				link := filepath.Join(realRoot, "first")
				createJunction(t, link, targetDir)
				input = filepath.Join(link, "deep", "file.bin")
			case "deep ancestor":
				parent := filepath.Join(realRoot, "first")
				if err := os.MkdirAll(parent, 0o700); err != nil {
					t.Fatal(err)
				}
				file := filepath.Join(targetDir, "file.bin")
				writeFixtureFile(t, file)
				link := filepath.Join(parent, "deep")
				createJunction(t, link, targetDir)
				input = filepath.Join(link, "file.bin")
			case "target":
				link := filepath.Join(realRoot, "target")
				createJunction(t, link, targetDir)
				input = link
			}

			v := newTestValidator(t, allowedRoot, nil, uniqueRecycleName(t))
			requirePathCode(t, validateFileError(v, input), proto.DeleteErrReparse)
		})
	}
}

func TestValidatorRejectsDirectRecycleTreeInputs(t *testing.T) {
	root := filepath.Join(runTempDir(t), "media")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	recycle := uniqueRecycleName(t)
	v := newTestValidator(t, root, nil, recycle)
	volumeRoot := filepath.VolumeName(root) + `\`

	for _, path := range []string{
		filepath.Join(volumeRoot, recycle),
		filepath.Join(volumeRoot, recycle, "task", "file.bin"),
	} {
		requirePathCode(t, validateFileError(v, path), proto.DeleteErrPathDenied)
	}
}

func TestValidatorValidatesRecycleDestinationLexicallyAndChecksExistingAncestors(t *testing.T) {
	root := filepath.Join(runTempDir(t), "media")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	recycle := uniqueRecycleName(t)
	v := newTestValidator(t, root, nil, recycle)
	volumeRoot := filepath.VolumeName(root) + `\`
	valid := filepath.Join(volumeRoot, recycle, "task-1", "file.bin")

	if err := v.ValidateRecycleTarget(valid); err != nil {
		t.Fatalf("ValidateRecycleTarget(valid): %v", err)
	}
	for _, path := range []string{
		filepath.Join(volumeRoot, recycle),
		filepath.Join(root, "not-recycle", "file.bin"),
		`\\server\share\file.bin`,
		`\\?\C:\file.bin`,
		`C:relative.bin`,
	} {
		err := v.ValidateRecycleTarget(path)
		if strings.HasPrefix(path, `\\`) || path == `C:relative.bin` {
			requirePathCode(t, err, proto.DeleteErrBadPath)
		} else {
			requirePathCode(t, err, proto.DeleteErrPathDenied)
		}
	}
}

func TestValidatorRejectsReparseInExistingRecycleTargetPath(t *testing.T) {
	base := runTempDir(t)
	recycleRoot := filepath.Join(base, "recycle")
	junctionTarget := filepath.Join(base, "junction-target")
	for _, dir := range []string{recycleRoot, junctionTarget} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeFixtureFile(t, filepath.Join(junctionTarget, "file.bin"))
	junction := filepath.Join(recycleRoot, "task")
	createJunction(t, junction, junctionTarget)

	volumeRoot := filepath.VolumeName(base) + `\`
	v := &Validator{allowed: []validatedRoot{{
		path:        base,
		volumeRoot:  volumeRoot,
		recycleRoot: recycleRoot,
	}}}
	for _, target := range []string{
		junction,
		filepath.Join(junction, "file.bin"),
	} {
		requirePathCode(
			t,
			v.ValidateRecycleTarget(target),
			proto.DeleteErrReparse,
		)
	}
}

func validateFileError(v *Validator, path string) error {
	_, err := v.ValidateFile(path)
	return err
}

func newTestValidator(
	t *testing.T,
	allowed string,
	denied []string,
	recycle string,
) *Validator {
	t.Helper()
	v, err := NewValidator(Config{
		AllowedRoots:   []string{allowed},
		DeniedRoots:    denied,
		RecycleDirName: recycle,
	})
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	return v
}

func requirePathCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected PathError code %s, got nil", want)
	}
	var pathErr *PathError
	if !errors.As(err, &pathErr) {
		t.Fatalf("error %T %v is not *PathError", err, err)
	}
	if pathErr.Code != want {
		t.Fatalf("PathError.Code = %q, want %q (err=%v)", pathErr.Code, want, err)
	}
}

func uniqueRecycleName(t *testing.T) string {
	t.Helper()
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'A' && r <= 'Z',
			r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '.', r == '_', r == '$', r == '-':
			return r
		default:
			return '-'
		}
	}, filepath.Base(runTempDir(t)))
	name = "$R-" + name
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

func writeFixtureFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func createJunction(t *testing.T, link, target string) {
	t.Helper()
	output, err := exec.Command("cmd.exe", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Fatalf(
			"NEEDS_CONTEXT: create real Windows junction %q -> %q: %v\n%s",
			link,
			target,
			err,
			output,
		)
	}
	t.Cleanup(func() {
		if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove junction %q: %v", link, err)
		}
	})
}

func getShortPath(t *testing.T, path string) (string, bool) {
	t.Helper()
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	size, err := windows.GetShortPathName(pathUTF16, nil, 0)
	if err != nil {
		t.Logf("GetShortPathNameW unavailable for %q: %v", path, err)
		return "", false
	}
	buffer := make([]uint16, size)
	length, err := windows.GetShortPathName(pathUTF16, &buffer[0], uint32(len(buffer)))
	if err != nil {
		t.Fatal(err)
	}
	shortPath := windows.UTF16ToString(buffer[:length])
	return shortPath, strings.Contains(shortPath, "~")
}

func getLongExistingPath(t *testing.T, path string) string {
	t.Helper()
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	size, err := windows.GetLongPathName(pathUTF16, nil, 0)
	if err != nil {
		t.Fatalf("GetLongPathNameW size for %q: %v", path, err)
	}
	buffer := make([]uint16, size)
	length, err := windows.GetLongPathName(
		pathUTF16,
		&buffer[0],
		uint32(len(buffer)),
	)
	if err != nil {
		t.Fatalf("GetLongPathNameW for %q: %v", path, err)
	}
	return windows.UTF16ToString(buffer[:length])
}

func runTempDir(t *testing.T) string {
	t.Helper()
	return getLongExistingPath(t, t.TempDir())
}

func TestConfigErrorFormattingDoesNotExposeMutationCapability(t *testing.T) {
	err := &PathError{Code: proto.DeleteErrBadPath, Err: errors.New("invalid path")}
	if got := fmt.Sprint(err); got != "E_BAD_PATH: invalid path" {
		t.Fatalf("PathError.Error() = %q", got)
	}
	if !errors.Is(err, err.Err) {
		t.Fatal("PathError must unwrap its cause")
	}
}
