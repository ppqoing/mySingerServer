package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"dedup/internal/m6bench"
)

type inputList []string

func (i *inputList) String() string { return strings.Join(*i, ";") }
func (i *inputList) Set(value string) error {
	for _, path := range strings.Split(value, ";") {
		if path = strings.TrimSpace(path); path != "" {
			*i = append(*i, path)
		}
	}
	return nil
}

func main() {
	os.Exit(runCLI(os.Args[1:], os.Stdout, os.Stderr))
}

func runCLI(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("perfreport", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var inputs inputList
	flags.Var(&inputs, "input", "input artifact path; repeat or separate with semicolon")
	jsonPath := flags.String("json", "", "result JSON path")
	markdownPath := flags.String("markdown", "", "result Markdown path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if len(inputs) == 0 {
		_, _ = fmt.Fprintln(stderr, "at least one -input is required")
		return 2
	}
	artifacts := make([]m6bench.Artifact, 0, len(inputs))
	for _, path := range inputs {
		artifact, err := m6bench.LoadArtifact(path)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		artifacts = append(artifacts, artifact)
	}
	report := m6bench.BuildReport(artifacts)
	if err := m6bench.WriteReport(report, *jsonPath, *markdownPath); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if err := m6bench.EncodeJSON(stdout, report); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
