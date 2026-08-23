package logs

import (
	"io"
	"os"
)

// A variable so a test can prove a failure is reported rather than swallowed.
var stderr io.Writer = os.Stderr
