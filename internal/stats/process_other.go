//go:build !windows

package stats

import "time"

func newProcessSampler() func(time.Time) processSample {
	return func(time.Time) processSample { return processSample{} }
}
