//go:build !windows

package process

import "errors"

// TrustedTerminator fail-closes outside Windows and never simulates process
// termination.
type TrustedTerminator struct{}

type DirectTerminator struct{}

func NewTrustedTerminator(Inspector) *TrustedTerminator { return &TrustedTerminator{} }

func (*TrustedTerminator) Terminate(Identity, uint32) error {
	return errors.New("trusted process termination is only supported on Windows")
}

func NewDirectTerminator() *DirectTerminator { return &DirectTerminator{} }

func (*DirectTerminator) Terminate(Identity, uint32) error {
	return errors.New("direct process termination is only supported on Windows")
}
