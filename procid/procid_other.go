//go:build !linux && !darwin

package procid

import "errors"

// errUnsupported keeps the package compiling on platforms hostd does not
// target. Adoption is a real feature with a real mechanism per OS; a stub that
// invented an identity would let a supervisor adopt a stranger.
var errUnsupported = errors.New("procid: process identity not supported on this platform")

func token(int) (string, error) {
	return "", errUnsupported
}
