package diskio

import "testing"

func TestDiskIdentityModelPreservesSchedulingFacts(t *testing.T) {
	identity := Identity{
		Key:      DiskKey("physical-set:1,7"),
		Local:    true,
		SSD:      true,
		KnownSSD: true,
		Volume:   `\\?\Volume{fixture}\\`,
		DiskNos:  []uint32{1, 7},
	}
	if identity.Key == "" || !identity.Local || !identity.SSD || !identity.KnownSSD {
		t.Fatalf("Identity lost scheduling facts: %#v", identity)
	}
	if SourceSequential == SourceRandom {
		t.Fatal("source classes must remain distinct")
	}
}
