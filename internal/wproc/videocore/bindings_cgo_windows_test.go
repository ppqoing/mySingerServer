//go:build cgo && windows

package videocore

import "testing"

// Break caught: analyze stores a Go UTF-16 slice pointer inside a request
// passed to C, so any non-empty contact-sheet path panics before native code.
func TestCGOAnalyzeTempPathReturnsNativeErrorWithoutPointerPanic(t *testing.T) {
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("analyze panicked at cgo boundary: %v", recovered)
		}
	}()

	_, err := (cgoBridge{}).analyze(nativeSession{}, AnalysisRequest{
		TempJPEGPath: `D:\临时\contact-sheet.jpg`,
	})
	if err == nil {
		t.Fatal("analyze with a nil native session returned no error")
	}
}
