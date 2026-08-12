package main

import (
	"sync/atomic"
	"testing"

	"dedup/internal/gui"
)

func TestOperationalRuntimeExposesAPIAndClosesIdempotently(t *testing.T) {
	api := gui.NewAPI(nil, nil, nil)
	var closeCalls atomic.Int32
	runtime := &operationalRuntime{
		api: api,
		closeRuntime: func() {
			closeCalls.Add(1)
		},
	}

	if got := runtime.API(); got != api {
		t.Fatalf("API() = %p, want %p", got, api)
	}
	runtime.Close()
	runtime.Close()
	if got := closeCalls.Load(); got != 1 {
		t.Fatalf("close calls = %d, want 1", got)
	}
}
