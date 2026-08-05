package helper

import "fmt"

type PathError struct {
	Code string
	Err  error
}

func (e *PathError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err == nil {
		return e.Code
	}
	return fmt.Sprintf("%s: %v", e.Code, e.Err)
}

func (e *PathError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}
