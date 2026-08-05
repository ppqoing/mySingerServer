package machineid

import (
	"errors"
	"strings"
	"testing"
)

type fakeSource struct {
	cpus      []string
	cpuErr    error
	boards    []string
	boardErr  error
	system    string
	systemErr error
}

func (f fakeSource) ProcessorIDs() ([]string, error) {
	return f.cpus, f.cpuErr
}

func (f fakeSource) BaseBoardSerialNumbers() ([]string, error) {
	return f.boards, f.boardErr
}

func (f fakeSource) MachineGUID() (string, error) {
	return f.system, f.systemErr
}

func TestResolveBuildsVersionedStableHardwareIdentity(t *testing.T) {
	got, err := Resolve(fakeSource{
		cpus:   []string{" bfebfbff000a0671 ", "BFEBFBFF000A0671"},
		boards: []string{"BOARD-001"},
		system: "00112233-4455-6677-8899-aabbccddeeff",
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = "node-5af06a5f3367adf7667600b1d18ff5d042d15c51fe531dbbfd348a5e4d7a0ced"
	if got.ID != want || !got.CPUAvailable || !got.BoardAvailable || !got.SystemAvailable {
		t.Fatalf("Result = %#v, want %q and all sources", got, want)
	}
	if len(got.Warnings) != 0 {
		t.Fatalf("Warnings = %q, want none", got.Warnings)
	}
}

func TestResolveSortsDeduplicatesAndFiltersPlaceholders(t *testing.T) {
	left, err := Resolve(fakeSource{
		cpus:   []string{"CPU-B", "UNKNOWN", "CPU-A", "0000", "--"},
		boards: []string{"TO BE FILLED BY O.E.M.", "BOARD-Z", "Default string"},
		system: "SYSTEM-X",
	})
	if err != nil {
		t.Fatal(err)
	}
	right, err := Resolve(fakeSource{
		cpus:   []string{"cpu-a", "cpu-b", "CPU-A"},
		boards: []string{" board-z "},
		system: "system-x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if left.ID != right.ID {
		t.Fatalf("enumeration order or placeholders changed ID: %q != %q", left.ID, right.ID)
	}
}

func TestResolveUsesRemainingSourcesAndRejectsNoSources(t *testing.T) {
	got, err := Resolve(fakeSource{
		cpuErr: errors.New("cpu raw identifier must not leak"),
		boards: []string{"DEFAULT STRING"},
		system: "SYSTEM-ONLY",
	})
	if err != nil || got.ID == "" || got.CPUAvailable || got.BoardAvailable ||
		!got.SystemAvailable || len(got.Warnings) != 2 {
		t.Fatalf("partial Result = %#v err=%v", got, err)
	}
	for _, warning := range got.Warnings {
		if strings.Contains(warning, "raw identifier") {
			t.Fatalf("warning leaked source error details: %q", warning)
		}
	}

	if _, err := Resolve(fakeSource{
		cpuErr:    errors.New("cpu"),
		boardErr:  errors.New("board"),
		systemErr: errors.New("system"),
	}); err == nil {
		t.Fatal("Resolve accepted three unavailable sources")
	}
	if _, err := Resolve(nil); err == nil {
		t.Fatal("Resolve accepted a nil source")
	}
}

func TestValidRequiresExactGeneratedFormat(t *testing.T) {
	valid := "node-5af06a5f3367adf7667600b1d18ff5d042d15c51fe531dbbfd348a5e4d7a0ced"
	if !Valid(valid) {
		t.Fatalf("Valid(%q) = false", valid)
	}
	for _, value := range []string{
		"machine-a",
		strings.ToUpper(valid),
		valid + "0",
		" node-5af06a5f3367adf7667600b1d18ff5d042d15c51fe531dbbfd348a5e4d7a0ced",
	} {
		if Valid(value) {
			t.Errorf("Valid(%q) = true", value)
		}
	}
}
