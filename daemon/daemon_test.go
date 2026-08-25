package daemon

import (
	"strings"
	"testing"
)

// A development build carries no daemon, and the error has to say that is why
// — otherwise it reads as a broken install at some machine's far end, when the
// truth is a build made without make dist.
func TestABuildWithoutTheDaemonSaysSo(t *testing.T) {
	_, err := Zip("amd64")
	if err == nil {
		// make dist was run in this tree and left its zips behind; the embed
		// picked them up, which is exactly what a release build does.
		t.Skip("this tree carries release zips; the missing-daemon path needs a clean tree")
	}
	if !strings.Contains(err.Error(), "make dist") {
		t.Fatalf("the error does not say how a build gets the daemon: %v", err)
	}
}
