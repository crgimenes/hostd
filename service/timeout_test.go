package service

import (
	"context"
	"strings"
	"testing"
)

func parseJob(t *testing.T, body string) (Service, error) {
	t.Helper()
	return Parse(context.Background(), "probe.filo", body)
}

// The bound belongs to a job. A service that stays up is not something a
// deadline applies to, and accepting one would be accepting a promise nothing
// keeps.
func TestARunTimeoutOnAServiceThatStaysUpIsRefused(t *testing.T) {
	_, err := parseJob(t, `(service
  (tuple "name" "web")
  (tuple "image" "site:1")
  (tuple "run-timeout" "30s"))`)
	if err == nil {
		t.Fatal("a service with no schedule accepted a run-timeout")
	}
	if !strings.Contains(err.Error(), "run-timeout") {
		t.Fatalf("the refusal does not name what is wrong: %v", err)
	}
}

// Refused where the declaration is read, not hours later when a run was
// supposed to be stopped and nothing happened.
func TestARunTimeoutThatIsNotAUsableDurationIsRefused(t *testing.T) {
	for _, bad := range []string{"soon", "10", "-5m", "0s"} {
		_, err := parseJob(t, `(service
  (tuple "name" "backup")
  (tuple "image" "restic:1")
  (tuple "every" "1h")
  (tuple "run-timeout" "`+bad+`"))`)
		if err == nil {
			t.Errorf("run-timeout %q was accepted", bad)
		}
	}
}

// A job with no bound is the shape every job had until now, and it has to stay
// legal: this is a ceiling somebody opts into, not one imposed on them.
func TestAJobWithoutARunTimeoutIsStillAJob(t *testing.T) {
	svc, err := parseJob(t, `(service
  (tuple "name" "backup")
  (tuple "image" "restic:1")
  (tuple "every" "1h"))`)
	if err != nil {
		t.Fatalf("a job with no run-timeout was refused: %v", err)
	}
	if svc.RunLimit() != 0 {
		t.Fatalf("a job with no run-timeout has a limit of %s", svc.RunLimit())
	}
}

func TestARunTimeoutIsReadAsTheDurationItSays(t *testing.T) {
	svc, err := parseJob(t, `(service
  (tuple "name" "backup")
  (tuple "image" "restic:1")
  (tuple "every" "1h")
  (tuple "run-timeout" "90s"))`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if svc.RunLimit().String() != "1m30s" {
		t.Fatalf("run-timeout 90s came out as %s", svc.RunLimit())
	}
}
