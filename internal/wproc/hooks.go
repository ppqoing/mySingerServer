package wproc

import (
	"time"

	"dedup/internal/wproc/mediacore"
)

func mediacoreDebugCrash() {
	mediacore.DebugCrash()
}

func mediacoreDebugSleep(duration time.Duration) {
	mediacore.DebugSleep(uint32(duration / time.Millisecond))
}
