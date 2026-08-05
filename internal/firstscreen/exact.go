package firstscreen

type FileRef struct {
	ID        int64
	MachineID string
	DiskNo    int
	Path      string
	Size      int64
}

type ExactGroup struct {
	SHA512  [64]byte
	Members []FileRef
}

type exactCollector struct {
	current [64]byte
	has     bool
	buffer  []FileRef
	groups  []ExactGroup
}

func (c *exactCollector) add(sha [64]byte, file FileRef) {
	if c.has && sha != c.current {
		c.flush()
	}
	c.has = true
	c.current = sha
	c.buffer = append(c.buffer, file)
}

func (c *exactCollector) flush() {
	if len(c.buffer) >= 2 {
		members := append([]FileRef(nil), c.buffer...)
		c.groups = append(c.groups, ExactGroup{
			SHA512:  c.current,
			Members: members,
		})
	}
	c.buffer = c.buffer[:0]
}

func (c *exactCollector) finish() []ExactGroup {
	if c.has {
		c.flush()
	}
	return c.groups
}
