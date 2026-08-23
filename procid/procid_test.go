package procid

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
)

func skipUnsupported(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("process identity is not implemented on %s", runtime.GOOS)
	}
}

func TestTokenStable(t *testing.T) {
	skipUnsupported(t)
	first, err := Token(os.Getpid())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if first == "" {
		t.Fatal("Token returned an empty identity")
	}
	second, err := Token(os.Getpid())
	if err != nil {
		t.Fatalf("Token second call: %v", err)
	}
	if first != second {
		t.Fatalf("identity changed between calls: %q then %q", first, second)
	}
}

func TestTokenDeadProcess(t *testing.T) {
	skipUnsupported(t)
	cmd := exec.Command("sleep", "0.05")
	err := cmd.Start()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	alive, err := Token(pid)
	if err != nil {
		t.Fatalf("Token while alive: %v", err)
	}
	err = cmd.Wait()
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	// After the child is reaped the PID no longer resolves, which is the
	// condition adoption relies on to notice a service died while hostd was
	// away.
	_, err = Token(pid)
	if err == nil {
		t.Fatal("Token resolved a reaped process")
	}
	if Matches(pid, alive) {
		t.Fatal("Matches accepted a reaped process")
	}
}

func TestMatches(t *testing.T) {
	skipUnsupported(t)
	self := os.Getpid()
	token, err := Token(self)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if !Matches(self, token) {
		t.Fatal("Matches rejected the current process")
	}
	if Matches(self, "not-the-recorded-identity") {
		t.Fatal("Matches accepted a wrong identity")
	}
	// An empty identity must never match: that is the bare-PID adoption this
	// package exists to prevent.
	if Matches(self, "") {
		t.Fatal("Matches accepted an empty identity")
	}
}

func TestTokenRejectsInvalidPID(t *testing.T) {
	_, err := Token(0)
	if err == nil {
		t.Fatal("Token(0) returned no error")
	}
	_, err = Token(-1)
	if err == nil {
		t.Fatal("Token(-1) returned no error")
	}
}
