package wproc

import (
	"time"

	"dedup/internal/wproc/mediacore"
)

func mediacoreVersion() string {
	return mediacore.Version()
}

func mediacoreDebugCrash() {
	mediacore.DebugCrash()
}

func mediacoreDebugSleep(duration time.Duration) {
	mediacore.DebugSleep(uint32(duration / time.Millisecond))
}
