//go:build !windows

package nodectl

import (
	"context"
	"strings"
	"testing"
)

func TestHelperPipeStubReportsWindowsRequirement(t *testing.T) {
	if _, err := Listen(HelperPipeName()); err == nil || !strings.Contains(err.Error(), "nodectl named pipes require windows") {
		t.Fatalf("Listen error = %v, want Windows requirement", err)
	}
	if _, err := Dial(context.Background(), HelperPipeName()); err == nil || !strings.Contains(err.Error(), "nodectl named pipes require windows") {
		t.Fatalf("Dial error = %v, want Windows requirement", err)
	}
}
