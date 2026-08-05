package main

import (
	"flag"
	"fmt"
	"os"

	"dedup/internal/wproc"
)

func main() {
	pipe := flag.String("pipe", "", `named pipe path (\\.\pipe\...)`)
	index := flag.Int("worker-index", -1, "worker slot index")
	flag.Parse()
	if *pipe == "" {
		fmt.Fprintln(os.Stderr, "worker: --pipe is required")
		os.Exit(2)
	}
	os.Exit(wproc.Run(*pipe, *index))
}
