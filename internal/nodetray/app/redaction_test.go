package app

import (
	"strings"
	"testing"

	"dedup/internal/nodetray/traymodel"
)

func TestSanitizeTextRemovesSecretsPathsURIUserinfoAndControls(t *testing.T) {
	input := "password=hunter2 token=abc postgres://user:secret@db/media https://alice:pw@example.test/x D:\\media\\private\\clip.mp4\r\nnext\x01"
	got := sanitizeText(input)
	for _, forbidden := range []string{"hunter2", "token=abc", "user:secret", "alice:pw", `D:\media`, "\r", "\n", "\x01"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("sanitizeText leaked %q in %q", forbidden, got)
		}
	}
	if got != sanitizeText(got) {
		t.Fatalf("sanitizeText is not idempotent: %q", got)
	}
}

func TestOperationAndComponentOutputsUseTheSameRedactionBoundary(t *testing.T) {
	operation := sanitizeOperation(traymodel.OperationResult{ErrorCode: "bad\r\n", ErrorSummary: "PGPassword=secret D:\\media\\a.jpg"})
	state := sanitizeComponentState(traymodel.ComponentState{ErrorCode: "bad\x00", ErrorSummary: "postgres://u:p@db/x"})
	combined := operation.ErrorCode + operation.ErrorSummary + state.ErrorCode + state.ErrorSummary
	for _, forbidden := range []string{"secret", `D:\media`, "u:p", "\r", "\n", "\x00"} {
		if strings.Contains(combined, forbidden) {
			t.Fatalf("UI output leaked %q in %q", forbidden, combined)
		}
	}
}
