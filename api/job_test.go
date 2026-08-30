package api

import (
	"context"
	"testing"

	"github.com/crgimenes/hostd/state"
	"github.com/crgimenes/hostd/supervisor"
)

// The run id has to reach the operator, because it is the only handle on the
// output: a command that answered "started" and nothing else would leave them
// guessing which of several runs is theirs.
func TestAskingForARunAnswersWithItsID(t *testing.T) {
	f := newFixture(t)
	f.sup.statuses = []supervisor.Status{{Name: "backup", Every: "1h"}}

	before := f.store.Generation()
	resp, err := f.client().Do(context.Background(), Request{Op: OpJobRun, Name: "backup"})
	if err != nil {
		t.Fatalf("job.run: %v", err)
	}
	if resp.Failed() {
		t.Fatalf("asking for a run failed: %v", resp.Err())
	}
	var started JobRun
	err = decodeBody(t, resp.Body, &started)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if started.Service != "backup" || started.Run == "" {
		t.Fatalf("the answer does not name the run: %+v", started)
	}
	// A generation is desired state, and one turn of a job never was part of
	// it: the declaration says the same thing before and after.
	if f.store.Generation() != before {
		t.Fatalf("generation moved from %d to %d for a run", before, f.store.Generation())
	}
}

// Who asked for a run belongs in the audit even though the desired state did
// not move. Otherwise a job that ran at 3am has no answer to "who did that".
func TestARunAskedForIsAudited(t *testing.T) {
	f := newFixture(t)
	f.sup.statuses = []supervisor.Status{{Name: "backup"}}

	_, err := f.client().Do(context.Background(), Request{Op: OpJobRun, Name: "backup", OnBehalfOf: "the night agent"})
	if err != nil {
		t.Fatalf("job.run: %v", err)
	}
	recent := f.store.Recent(10)
	at := -1
	for index, entry := range recent {
		if entry.Operation == OpJobRun {
			at = index
			break
		}
	}
	if at < 0 {
		t.Fatalf("no audit entry for the run: %+v", recent)
	}
	if recent[at].Target != "backup" || recent[at].OnBehalfOf != "the night agent" {
		t.Fatalf("the audit entry does not say who asked for what: %+v", recent[at])
	}
	if recent[at].Result != state.ResultOK {
		t.Fatalf("result = %q", recent[at].Result)
	}
}

// Refused is not failed. An agent reading a full ceiling as a failure asks
// again, and again, for as long as the ceiling holds; one reading it as
// success acts as though a run were going.
func TestAJobAtItsCeilingIsRefusedAndNotFailed(t *testing.T) {
	f := newFixture(t)
	f.sup.statuses = []supervisor.Status{{Name: "backup"}}
	f.sup.failWith = supervisor.ErrRunRefused{Reason: "a run was asked for and 10 are already going, which is the ceiling"}

	resp, err := f.client().Do(context.Background(), Request{Op: OpJobRun, Name: "backup"})
	if err != nil {
		t.Fatalf("job.run: %v", err)
	}
	if resp.Code != CodeRefused {
		t.Fatalf("code = %q, want %q", resp.Code, CodeRefused)
	}
	// Refused means nothing happened, and the audit has to agree with that.
	recent := f.store.Recent(10)
	for _, entry := range recent {
		if entry.Operation != OpJobRun {
			continue
		}
		if entry.Result != state.ResultRefused {
			t.Fatalf("a refused run was audited as %q", entry.Result)
		}
		return
	}
	t.Fatalf("no audit entry for the refused run: %+v", recent)
}

func TestAskingForARunWithoutSayingWhichIsInvalid(t *testing.T) {
	f := newFixture(t)
	resp, err := f.client().Do(context.Background(), Request{Op: OpJobRun})
	if err != nil {
		t.Fatalf("job.run: %v", err)
	}
	if resp.Code != CodeInvalid {
		t.Fatalf("code = %q, want %q", resp.Code, CodeInvalid)
	}
}

// Asking a container for a run is the wrong thing asked of the right service:
// the caller's mistake, not the machine's failure. Exiting as a failure would
// have an agent retry something that can never succeed.
func TestAskingAContainerForARunIsAUsageError(t *testing.T) {
	f := newFixture(t)
	f.sup.statuses = []supervisor.Status{{Name: "caddy"}}
	f.sup.failWith = supervisor.ErrNotAJob{Name: "caddy"}

	resp, err := f.client().Do(context.Background(), Request{Op: OpJobRun, Name: "caddy"})
	if err != nil {
		t.Fatalf("job.run: %v", err)
	}
	if resp.Code != CodeInvalid {
		t.Fatalf("code = %q, want %q", resp.Code, CodeInvalid)
	}
}
