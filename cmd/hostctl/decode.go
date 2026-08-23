package main

import (
	"context"

	"github.com/crgimenes/hostd/internal/filoconf"
)

// decode reads a Filo body from the daemon into a Go value.
func decode(ctx context.Context, body string, out any) error {
	if body == "" {
		return nil
	}
	return filoconf.Decode(ctx, "response", body, out)
}

// marshal renders a value as one line of Filo.
func marshal(v any) (string, error) {
	out, err := filoconf.Marshal(v)
	if err != nil {
		return "", err
	}
	return out, nil
}
