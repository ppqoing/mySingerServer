//go:build !windows

package task

func newPlatformSchedulerBackend() (schedulerBackend, error) {
	return nil, ErrWindowsRequired
}

func platformResolveFinalHelper(string) (string, error) {
	return "", ErrWindowsRequired
}
