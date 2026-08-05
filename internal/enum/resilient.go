package enum

import (
	"errors"
	"fmt"
	"strings"
)

var ErrNoResults = errors.New("enumerator: primary returned no results for root")

type ResilientEnumerator struct {
	primary    Enumerator
	fallback   Enumerator
	onFallback func(root string, cause error)
}

func NewResilientEnumerator(
	primary Enumerator,
	fallback Enumerator,
	onFallback func(root string, cause error),
) *ResilientEnumerator {
	return &ResilientEnumerator{
		primary:    primary,
		fallback:   fallback,
		onFallback: onFallback,
	}
}

func (e *ResilientEnumerator) Name() string {
	return e.primary.Name() + "+" + e.fallback.Name()
}

func (e *ResilientEnumerator) Available() error {
	return e.primary.Available()
}

func (e *ResilientEnumerator) Enum(
	root string,
	visit func(FileRecord) error,
) error {
	seen := make(map[string]struct{})
	primaryErr := e.primary.Enum(root, func(record FileRecord) error {
		key := recordPathKey(record.Path)
		if _, exists := seen[key]; exists {
			return nil
		}
		if err := visit(record); err != nil {
			return &visitorError{err: err}
		}
		seen[key] = struct{}{}
		return nil
	})
	var callbackErr *visitorError
	if errors.As(primaryErr, &callbackErr) {
		return callbackErr.err
	}
	if primaryErr == nil && len(seen) > 0 {
		return nil
	}

	cause := primaryErr
	if cause == nil {
		cause = ErrNoResults
	}
	if e.onFallback != nil {
		e.onFallback(root, cause)
	}
	fallbackErr := e.fallback.Enum(root, func(record FileRecord) error {
		key := recordPathKey(record.Path)
		if _, exists := seen[key]; exists {
			return nil
		}
		if err := visit(record); err != nil {
			return err
		}
		seen[key] = struct{}{}
		return nil
	})
	if fallbackErr != nil {
		return fmt.Errorf(
			"enumerator: primary failed (%v), fallback failed: %w",
			cause,
			fallbackErr,
		)
	}
	return nil
}

func recordPathKey(path string) string {
	return strings.ToLower(strings.ReplaceAll(cleanPath(path), "/", `\`))
}

type visitorError struct {
	err error
}

func (e *visitorError) Error() string { return e.err.Error() }
func (e *visitorError) Unwrap() error { return e.err }
