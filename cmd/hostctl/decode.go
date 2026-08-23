package main

import (
	"context"

	"github.com/crgimenes/hostd/filoconf"
)

func decode(ctx context.Context, body string, out any) error {
	if body == "" {
		return nil
	}
	return filoconf.Decode(ctx, "response", body, out)
}
