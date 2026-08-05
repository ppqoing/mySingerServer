package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"dedup/internal/m6bench"
)

type commandList [][]string

func (c *commandList) String() string {
	data, _ := json.Marshal(c)
	return string(data)
}

func (c *commandList) Set(value string) error {
	var command []string
	if err := json.Unmarshal([]byte(value), &command); err != nil {
		return fmt.Errorf("command must be a JSON string array: %w", err)
	}
	if len(command) == 0 {
		return fmt.Errorf("command must not be empty")
	}
	*c = append(*c, command)
	return nil
}

func main() {
	os.Exit(runCLI(os.Args[1:], os.Stdout, os.Stderr))
}

func runCLI(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("soakrun", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("corpus-root", "", "corpusgen-owned root")
	duration := flags.Duration("duration", 24*time.Hour, "soak duration")
	output := flags.String("output", "", "evidence directory")
	out := flags.String("out", "", "optional result JSON path")
	var commands commandList
	flags.Var(&commands, "command", `child command as JSON array, for example ["agent.exe","-config","agent.json"]`)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if len(commands) == 0 {
		_, _ = fmt.Fprintln(stderr, "at least one -command is required")
		return 2
	}
	result, err := m6bench.RunSoak(context.Background(), m6bench.SoakConfig{
		CorpusRoot: *root, Duration: *duration,
		Commands: commands, OutputDir: *output,
	})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if *out != "" {
		if err := m6bench.WriteJSON(*out, result); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
	}
	if err := m6bench.EncodeJSON(stdout, result); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
