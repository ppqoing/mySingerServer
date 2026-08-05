package stats

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestJSONLSinkWritesDecodableSnapshots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.log")
	sink, err := NewJSONLSink(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	for index := 0; index < 2; index++ {
		if err := sink.Write(Snapshot{
			Time: time.Unix(int64(index+1), 0), FilesDone: int64(index + 1),
		}); err != nil {
			t.Fatal(err)
		}
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	var count int
	for scanner.Scan() {
		var snapshot Snapshot
		if err := json.Unmarshal(scanner.Bytes(), &snapshot); err != nil {
			t.Fatalf("line %d: %v", count+1, err)
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("decoded lines = %d, want 2", count)
	}
}
