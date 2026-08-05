package process

import (
	"context"
	"errors"
)

// Identity is the minimum immutable process identity used for adoption and
// termination decisions. ExecutablePath must be the platform's final path.
type Identity struct {
	PID             int
	StartedAtUnixMS int64
	ExecutablePath  string
}

type Inspector interface {
	Inspect(pid int) (Identity, error)
	Wait(ctx context.Context, identity Identity) (exitCode int, err error)
}

func SamePIDAndExecutable(expected, actual Identity) bool {
	return expected.PID > 0 &&
		expected.PID == actual.PID &&
		expected.ExecutablePath != "" &&
		actual.ExecutablePath != "" &&
		sameExecutablePath(expected.ExecutablePath, actual.ExecutablePath)
}

func SameProcess(expected, actual Identity) bool {
	return SamePIDAndExecutable(expected, actual) &&
		expected.StartedAtUnixMS > 0 &&
		expected.StartedAtUnixMS == actual.StartedAtUnixMS
}

// ErrUACCancelled is returned when the user rejects the Windows elevation
// prompt. Callers can preserve their previous state instead of treating this
// user choice as a component failure.
type ErrUACCancelled struct{}

func (*ErrUACCancelled) Error() string { return "UAC elevation was cancelled" }

func IsUACCancelled(err error) bool {
	var target *ErrUACCancelled
	return errors.As(err, &target)
}
