package diskio

import "time"

type DiskKey string

type SourceClass uint8

const (
	SourceSequential SourceClass = iota + 1
	SourceRandom
)

type Identity struct {
	Key      DiskKey
	Local    bool
	SSD      bool
	KnownSSD bool
	Volume   string
	DiskNos  []uint32
}

type PolicyConfig struct {
	LeaseBytes         int64
	MinLeaseBytes      int64
	MaxLeaseBytes      int64
	HDDInitial         int
	SSDInitial         int
	MaxPerDisk         int
	HDDRandomMax       int
	Window             time.Duration
	IncreaseThreshold  float64
	DecreaseThreshold  float64
	MaxQueuedPerWorker int
}
