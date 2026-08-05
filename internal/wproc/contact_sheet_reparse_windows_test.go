//go:build windows

package wproc

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestContactSheetReparseAttributeIsRejected(t *testing.T) {
	if !contactSheetDirectoryHasReparsePoint(windows.FILE_ATTRIBUTE_REPARSE_POINT) {
		t.Fatal("reparse-point directory attribute was accepted")
	}
	if contactSheetDirectoryHasReparsePoint(windows.FILE_ATTRIBUTE_DIRECTORY) {
		t.Fatal("ordinary directory attribute was rejected")
	}
}
