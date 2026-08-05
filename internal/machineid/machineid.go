package machineid

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

const canonicalPrefix = "mysingerserver-machine-id:v1\n"

var (
	machineIDPattern = regexp.MustCompile(`^node-[0-9a-f]{64}$`)
	placeholders     = map[string]struct{}{
		"UNKNOWN":                {},
		"NONE":                   {},
		"DEFAULT STRING":         {},
		"TO BE FILLED BY O.E.M.": {},
		"NOT SPECIFIED":          {},
	}
)

// Source provides the three classes of identifiers used by Resolve.
type Source interface {
	ProcessorIDs() ([]string, error)
	BaseBoardSerialNumbers() ([]string, error)
	MachineGUID() (string, error)
}

// Result is the generated machine identity and non-sensitive source health.
type Result struct {
	ID              string
	CPUAvailable    bool
	BoardAvailable  bool
	SystemAvailable bool
	Warnings        []string
}

// Valid reports whether value has the exact generated machine ID format.
func Valid(value string) bool {
	return machineIDPattern.MatchString(value)
}

// Resolve normalizes the available source identifiers and hashes the stable,
// versioned canonical representation. Raw identifiers are never returned.
func Resolve(source Source) (Result, error) {
	if source == nil {
		return Result{}, errors.New("machine identity unavailable: source is nil")
	}

	cpus, cpuErr := source.ProcessorIDs()
	boards, boardErr := source.BaseBoardSerialNumbers()
	system, systemErr := source.MachineGUID()

	cpuValues := normalizeMany(cpus)
	boardValues := normalizeMany(boards)
	systemValues := normalizeMany([]string{system})
	result := Result{
		CPUAvailable:    len(cpuValues) > 0,
		BoardAvailable:  len(boardValues) > 0,
		SystemAvailable: len(systemValues) > 0,
	}
	result.Warnings = sourceWarnings(cpuErr, boardErr, systemErr, result)

	if !result.CPUAvailable && !result.BoardAvailable && !result.SystemAvailable {
		return Result{}, errors.New("machine identity unavailable: no valid CPU, board, or system ID")
	}

	systemValue := ""
	if len(systemValues) != 0 {
		systemValue = systemValues[0]
	}
	canonical := canonicalPrefix +
		"cpu=" + strings.Join(cpuValues, "|") + "\n" +
		"board=" + strings.Join(boardValues, "|") + "\n" +
		"system=" + systemValue + "\n"
	digest := sha256.Sum256([]byte(canonical))
	result.ID = "node-" + hex.EncodeToString(digest[:])
	return result, nil
}

func normalizeMany(values []string) []string {
	unique := make(map[string]struct{}, len(values))
	for _, value := range values {
		normalized := strings.ToUpper(strings.TrimFunc(value, func(r rune) bool {
			return r == '\x00' || unicode.IsSpace(r)
		}))
		if normalized == "" || isPlaceholder(normalized) {
			continue
		}
		unique[normalized] = struct{}{}
	}

	result := make([]string, 0, len(unique))
	for value := range unique {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func isPlaceholder(value string) bool {
	if _, found := placeholders[value]; found {
		return true
	}
	for _, r := range value {
		if r != '0' && r != '-' && !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func sourceWarnings(cpuErr, boardErr, systemErr error, result Result) []string {
	warnings := make([]string, 0, 3)
	warnings = appendSourceWarning(warnings, "cpu", cpuErr, result.CPUAvailable)
	warnings = appendSourceWarning(warnings, "board", boardErr, result.BoardAvailable)
	warnings = appendSourceWarning(warnings, "system", systemErr, result.SystemAvailable)
	return warnings
}

func appendSourceWarning(warnings []string, source string, sourceErr error, available bool) []string {
	if sourceErr != nil {
		return append(warnings, source+" source read failed")
	}
	if !available {
		return append(warnings, source+" source has no valid value")
	}
	return warnings
}
