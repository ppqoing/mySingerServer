//go:build cgo && windows && legacy_mediacore

package mediacore

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type nativeOpsCallCounts struct {
	allocate   atomic.Int32
	decode     atomic.Int32
	pdq256     atomic.Int32
	phase2     atomic.Int32
	phashParts atomic.Int32
	sobelHist  atomic.Int32
	freeImage  atomic.Int32
	freeOuter  atomic.Int32
}

func TestGrayImageActualCImportBoundaryCounts(t *testing.T) {
	resetNativeBoundaryCallCountsForTest()
	t.Cleanup(resetNativeBoundaryCallCountsForTest)

	decoded, err := DecodeFromMemory(testPNG(t, 96, 80))
	if err != nil {
		t.Fatal(err)
	}
	defer decoded.Free()

	for range 3 {
		if _, _, err := decoded.PDQ256(); err != nil {
			t.Fatal(err)
		}
	}
	for range 4 {
		if _, err := decoded.Phase2(); err != nil {
			t.Fatal(err)
		}
	}

	got := snapshotNativeBoundaryCallCountsForTest()
	want := nativeBoundaryCallCounts{
		decode:     1,
		pdq256:     3,
		phase2:     4,
		phashParts: 0,
		sobelHist:  0,
	}
	if got != want {
		t.Fatalf("actual C import boundary calls = %+v, want %+v", got, want)
	}
}

func TestGrayImageUsesOneDecodeAndCombinedPhase2Calls(t *testing.T) {
	ops, calls := newCountingNativeOps(realNativeGrayImageOps)
	restore := swapNativeGrayImageOpsForTest(ops)
	t.Cleanup(restore)

	decoded, err := DecodeFromMemory(testPNG(t, 96, 80))
	if err != nil {
		t.Fatal(err)
	}
	defer decoded.Free()

	for range 3 {
		if _, _, err := decoded.PDQ256(); err != nil {
			t.Fatal(err)
		}
	}
	for range 4 {
		if _, err := decoded.Phase2(); err != nil {
			t.Fatal(err)
		}
	}

	assertNativeCallCount(t, "allocate", &calls.allocate, 1)
	assertNativeCallCount(t, "decode", &calls.decode, 1)
	assertNativeCallCount(t, "PDQ256", &calls.pdq256, 3)
	assertNativeCallCount(t, "combined phase2", &calls.phase2, 4)
	assertNativeCallCount(t, "separate pHash", &calls.phashParts, 0)
	assertNativeCallCount(t, "separate Sobel", &calls.sobelHist, 0)
}

func TestGrayImageFinalizerReleasesNativeAllocationsExactlyOnce(t *testing.T) {
	ops, calls := newCountingNativeOps(realNativeGrayImageOps)
	restore := swapNativeGrayImageOpsForTest(ops)
	t.Cleanup(restore)

	func() {
		decoded, err := DecodeFromMemory(testPNG(t, 64, 64))
		if err != nil {
			t.Fatal(err)
		}
		runtime.KeepAlive(decoded)
	}()

	waitForNativeRelease(t, calls)
	assertNativeCallCount(t, "native image free", &calls.freeImage, 1)
	assertNativeCallCount(t, "outer allocation free", &calls.freeOuter, 1)
}

func TestGrayImageExplicitFreePreventsFinalizerDoubleRelease(t *testing.T) {
	ops, calls := newCountingNativeOps(realNativeGrayImageOps)
	restore := swapNativeGrayImageOpsForTest(ops)
	t.Cleanup(restore)

	func() {
		decoded, err := DecodeFromMemory(testPNG(t, 64, 64))
		if err != nil {
			t.Fatal(err)
		}
		decoded.Free()
	}()
	assertNativeCallCount(t, "native image free", &calls.freeImage, 1)
	assertNativeCallCount(t, "outer allocation free", &calls.freeOuter, 1)

	for range 20 {
		runtime.GC()
		runtime.Gosched()
		time.Sleep(5 * time.Millisecond)
	}
	assertNativeCallCount(t, "native image free after GC", &calls.freeImage, 1)
	assertNativeCallCount(t, "outer allocation free after GC", &calls.freeOuter, 1)
}

func TestGrayImageFreeWaitsForBlockingNativeFeatureCall(t *testing.T) {
	ops, calls := newCountingNativeOps(realNativeGrayImageOps)
	entered := make(chan struct{})
	release := make(chan struct{})
	realPhase2 := ops.phase2
	var signalOnce sync.Once
	ops.phase2 = func(image *nativeGrayImage) (Phase2Result, error) {
		signalOnce.Do(func() { close(entered) })
		<-release
		return realPhase2(image)
	}
	restore := swapNativeGrayImageOpsForTest(ops)
	t.Cleanup(restore)

	decoded, err := DecodeFromMemory(testPNG(t, 96, 80))
	if err != nil {
		t.Fatal(err)
	}

	featureDone := make(chan error, 1)
	go func() {
		_, callErr := decoded.Phase2()
		featureDone <- callErr
	}()
	waitForSignal(t, entered, "native feature call entry")

	freeStarted := make(chan struct{})
	freeDone := make(chan struct{})
	go func() {
		close(freeStarted)
		decoded.Free()
		close(freeDone)
	}()
	waitForSignal(t, freeStarted, "Free start")
	select {
	case <-freeDone:
		t.Fatal("Free returned while the native feature call was blocked")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	if callErr := waitForError(t, featureDone, "native feature completion"); callErr != nil {
		t.Fatal(callErr)
	}
	waitForSignal(t, freeDone, "Free completion")

	if _, _, err := decoded.PDQ256(); err == nil {
		t.Fatal("PDQ256 succeeded after blocking call and Free completed")
	}
	if _, err := decoded.Phase2(); err == nil {
		t.Fatal("Phase2 succeeded after blocking call and Free completed")
	}
	assertNativeCallCount(t, "native image free", &calls.freeImage, 1)
	assertNativeCallCount(t, "outer allocation free", &calls.freeOuter, 1)
}

func newCountingNativeOps(
	delegate *nativeGrayImageOps,
) (*nativeGrayImageOps, *nativeOpsCallCounts) {
	calls := &nativeOpsCallCounts{}
	return &nativeGrayImageOps{
		allocate: func() *nativeGrayImage {
			calls.allocate.Add(1)
			return delegate.allocate()
		},
		decode: func(data []byte, image *nativeGrayImage) error {
			calls.decode.Add(1)
			return delegate.decode(data, image)
		},
		pdq256: func(image *nativeGrayImage) ([PDQ256Bytes]byte, int32, error) {
			calls.pdq256.Add(1)
			return delegate.pdq256(image)
		},
		phase2: func(image *nativeGrayImage) (Phase2Result, error) {
			calls.phase2.Add(1)
			return delegate.phase2(image)
		},
		phashParts: func(image *nativeGrayImage) ([PHashPartsCount]uint64, error) {
			calls.phashParts.Add(1)
			return delegate.phashParts(image)
		},
		sobelHist: func(image *nativeGrayImage) ([SobelHistDim]float32, error) {
			calls.sobelHist.Add(1)
			return delegate.sobelHist(image)
		},
		freeImage: func(image *nativeGrayImage) {
			calls.freeImage.Add(1)
			delegate.freeImage(image)
		},
		freeOuter: func(image *nativeGrayImage) {
			calls.freeOuter.Add(1)
			delegate.freeOuter(image)
		},
	}, calls
}

func waitForNativeRelease(t *testing.T, calls *nativeOpsCallCounts) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		runtime.Gosched()
		if calls.freeImage.Load() == 1 && calls.freeOuter.Load() == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf(
		"finalizer release counts = image:%d outer:%d, want 1 each",
		calls.freeImage.Load(),
		calls.freeOuter.Load(),
	)
}

func assertNativeCallCount(
	t *testing.T,
	name string,
	counter *atomic.Int32,
	want int32,
) {
	t.Helper()
	if got := counter.Load(); got != want {
		t.Fatalf("%s calls = %d, want %d", name, got, want)
	}
}

func waitForSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func waitForError(t *testing.T, result <-chan error, name string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
		return nil
	}
}
