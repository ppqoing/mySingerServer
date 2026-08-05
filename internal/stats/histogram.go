package stats

import "time"

var latencyBounds = [...]time.Duration{
	100 * time.Microsecond,
	250 * time.Microsecond,
	500 * time.Microsecond,
	time.Millisecond,
	2 * time.Millisecond,
	5 * time.Millisecond,
	10 * time.Millisecond,
	20 * time.Millisecond,
	50 * time.Millisecond,
	100 * time.Millisecond,
	250 * time.Millisecond,
	500 * time.Millisecond,
	time.Second,
	2 * time.Second,
	5 * time.Second,
	10 * time.Second,
	30 * time.Second,
	time.Minute,
	2 * time.Minute,
	5 * time.Minute,
	10 * time.Minute,
}

const latencyBucketCount = len(latencyBounds) + 1

type histogram struct {
	buckets [latencyBucketCount]uint64
	count   uint64
}

func (h *histogram) observe(value time.Duration) {
	if value < 0 {
		return
	}
	index := len(latencyBounds)
	for candidate, upper := range latencyBounds {
		if value <= upper {
			index = candidate
			break
		}
	}
	h.buckets[index]++
	h.count++
}

func (h *histogram) percentile(fraction float64) time.Duration {
	if h.count == 0 {
		return 0
	}
	if fraction <= 0 {
		fraction = 0.01
	}
	if fraction > 1 {
		fraction = 1
	}
	target := uint64(float64(h.count)*fraction + 0.999999999)
	var seen uint64
	for index, count := range h.buckets {
		seen += count
		if seen >= target {
			if index < len(latencyBounds) {
				return latencyBounds[index]
			}
			return latencyBounds[len(latencyBounds)-1]
		}
	}
	return latencyBounds[len(latencyBounds)-1]
}

func (h *histogram) merge(other histogram) {
	for index := range h.buckets {
		h.buckets[index] += other.buckets[index]
	}
	h.count += other.count
}

func (h *histogram) reset() {
	*h = histogram{}
}
