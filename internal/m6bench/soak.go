package m6bench

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

type SoakConfig struct {
	CorpusRoot  string
	Duration    time.Duration
	Commands    [][]string
	Environment map[string]string
	OutputDir   string
}

type SoakChildResult struct {
	PID       int   `json:"pid"`
	ExitCode  int   `json:"exit_code"`
	ElapsedMS int64 `json:"elapsed_ms"`
}

type SoakResult struct {
	SchemaVersion int               `json:"schema_version"`
	Kind          string            `json:"kind"`
	StartedAt     time.Time         `json:"started_at"`
	ElapsedMS     int64             `json:"elapsed_ms"`
	StopReason    string            `json:"stop_reason"`
	Children      []SoakChildResult `json:"children"`
}

func RunSoak(parent context.Context, cfg SoakConfig) (SoakResult, error) {
	root, err := validateCorpusRoot(cfg.CorpusRoot)
	if err != nil {
		return SoakResult{}, err
	}
	if _, err := loadCorpusOwner(root); err != nil {
		return SoakResult{}, fmt.Errorf("soakrun: corpus is not owned: %w", err)
	}
	if cfg.Duration <= 0 || cfg.Duration > 7*24*time.Hour ||
		len(cfg.Commands) == 0 || len(cfg.Commands) > 64 ||
		cfg.OutputDir == "" {
		return SoakResult{}, fmt.Errorf("soakrun: invalid bounded configuration")
	}
	for _, command := range cfg.Commands {
		if len(command) == 0 || command[0] == "" {
			return SoakResult{}, fmt.Errorf("soakrun: empty command")
		}
	}
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		return SoakResult{}, err
	}
	ctx, cancel := context.WithTimeout(parent, cfg.Duration)
	defer cancel()
	started := time.Now()
	result := SoakResult{
		SchemaVersion: SchemaVersion,
		Kind:          "soak",
		StartedAt:     started.UTC(),
		Children:      make([]SoakChildResult, len(cfg.Commands)),
	}
	var commands []*exec.Cmd
	var files []*os.File
	for index, arguments := range cfg.Commands {
		stdout, err := os.Create(filepath.Join(cfg.OutputDir, fmt.Sprintf("child-%02d.stdout.log", index)))
		if err != nil {
			cancel()
			return SoakResult{}, err
		}
		stderr, err := os.Create(filepath.Join(cfg.OutputDir, fmt.Sprintf("child-%02d.stderr.log", index)))
		if err != nil {
			stdout.Close()
			cancel()
			return SoakResult{}, err
		}
		files = append(files, stdout, stderr)
		command := exec.CommandContext(ctx, arguments[0], arguments[1:]...)
		command.Stdout, command.Stderr = stdout, stderr
		command.Dir = root
		command.Env = append([]string(nil), os.Environ()...)
		for key, value := range cfg.Environment {
			command.Env = append(command.Env, key+"="+value)
		}
		if err := command.Start(); err != nil {
			cancel()
			for _, running := range commands {
				if running.Process != nil {
					_ = running.Process.Kill()
				}
			}
			for _, file := range files {
				_ = file.Close()
			}
			return SoakResult{}, fmt.Errorf("soakrun: start child %d: %w", index, err)
		}
		result.Children[index].PID = command.Process.Pid
		commands = append(commands, command)
	}
	var wait sync.WaitGroup
	wait.Add(len(commands))
	allDone := make(chan struct{})
	for index, command := range commands {
		go func(index int, command *exec.Cmd) {
			defer wait.Done()
			childStarted := time.Now()
			waitErr := command.Wait()
			result.Children[index].ElapsedMS = time.Since(childStarted).Milliseconds()
			if waitErr == nil {
				result.Children[index].ExitCode = 0
				return
			}
			if exitErr, ok := waitErr.(*exec.ExitError); ok {
				result.Children[index].ExitCode = exitErr.ExitCode()
			} else {
				result.Children[index].ExitCode = -1
			}
		}(index, command)
	}
	go func() {
		wait.Wait()
		close(allDone)
	}()
	select {
	case <-ctx.Done():
		if parent.Err() != nil {
			result.StopReason = "cancelled"
		} else {
			result.StopReason = "duration"
		}
	case <-allDone:
		result.StopReason = "children_exit"
		cancel()
	}
	<-allDone
	for _, file := range files {
		_ = file.Close()
	}
	result.ElapsedMS = time.Since(started).Milliseconds()
	return result, nil
}
