package firstscreen

type bandKey struct {
	band uint8
	val  uint64
}

type bandIndex struct {
	m     map[bandKey][]uint32
	stamp []uint32
	cur   uint32
}

func newBandIndex(capHint int) *bandIndex {
	return &bandIndex{
		m:     make(map[bandKey][]uint32, capHint*bandCount),
		stamp: make([]uint32, 0, capHint),
	}
}

func (b *bandIndex) query(hash [4]uint64, scratch []uint32) []uint32 {
	out := scratch[:0]
	b.cur++
	if b.cur == 0 {
		for i := range b.stamp {
			b.stamp[i] = 0
		}
		b.cur = 1
	}
	for band := uint8(0); band < bandCount; band++ {
		key := bandKey{band: band, val: hash[band]}
		for _, index := range b.m[key] {
			if b.stamp[index] == b.cur {
				continue
			}
			b.stamp[index] = b.cur
			out = append(out, index)
		}
	}
	return out
}

func (b *bandIndex) add(index uint32, hash [4]uint64) {
	for band := uint8(0); band < bandCount; band++ {
		key := bandKey{band: band, val: hash[band]}
		b.m[key] = append(b.m[key], index)
	}
	for len(b.stamp) <= int(index) {
		b.stamp = append(b.stamp, 0)
	}
}
