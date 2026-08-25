package supervisor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crgimenes/hostd/service"
)

// A job declared to run once an hour, asked for now. The point of the command
// is exactly this gap: without it, trying a backup means waiting for the clock
// or editing the declaration to something it should not say.
func TestARunAskedForByHandHappensWithoutWaitingForTheClock(t *testing.T) {
	client, image := requireRuntime(t)
	h := newHarness(t)
	const job = "hostd-suite-ondemand"
	cleanupRuns(t, client, job)
	h.sup = New(h.buffer, h.services)
	h.sup.Runtime(client)

	svc := container(job, image, `echo asked for`)
	svc.Every = "1h"

	err := h.sup.Adopt(context.Background(), []service.Service{svc})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go h.sup.Run(ctx)
	defer h.stop()

	run, err := h.sup.RunNow(job)
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if run == "" {
		t.Fatal("a run that started has an id; without it nothing can follow its output")
	}
	// The id has to be the one the timeline uses, or `log -run <id>` follows
	// nothing and the command's own advice is wrong.
	h.waitFor("the run to be recorded under its id", func() bool {
		return strings.Contains(h.logText(), "run "+run+" started as container")
	})
	h.waitFor("what the run printed to arrive", func() bool {
		return strings.Contains(h.logText(), "asked for")
	})
}

// The ceiling and the overlap rule live in the declaration, and a hand asking
// is not a reason to overrule the file. Refused is not failed: asking again
// unchanged gets the same answer, and an agent has to be able to tell those
// apart or it retries for as long as the ceiling holds.
func TestAskingForARunDoesNotOverruleTheOverlapTheFileDeclares(t *testing.T) {
	client, image := requireRuntime(t)
	h := newHarness(t)
	const job = "hostd-suite-nooverlap"
	cleanupRuns(t, client, job)
	h.sup = New(h.buffer, h.services)
	h.sup.Runtime(client)

	// Long enough that the first run is still going when the second is asked
	// for, and declared not to overlap.
	svc := container(job, image, `sleep 60`)
	svc.Every = "1h"
	svc.Overlap = service.OverlapSkip

	err := h.sup.Adopt(context.Background(), []service.Service{svc})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go h.sup.Run(ctx)
	defer h.stop()

	_, err = h.sup.RunNow(job)
	if err != nil {
		t.Fatalf("the first run: %v", err)
	}
	_, err = h.sup.RunNow(job)
	refusal, refused := errors.AsType[ErrRunRefused](err)
	if !refused {
		t.Fatalf("the second ask returned %v, and a job declared %q must refuse it", err, service.OverlapSkip)
	}
	if !strings.Contains(refusal.Reason, "asked for") {
		t.Fatalf("the refusal does not say what asked: %q", refusal.Reason)
	}
	// The turn nobody took belongs in the timeline, not only in the answer to
	// whoever asked: a week later the log is all there is.
	h.waitFor("the refusal to reach the timeline", func() bool {
		return strings.Contains(h.logText(), "the previous one is still going")
	})
}

// A container is not a job and has no runs. Answering with a start would be a
// different operation than the one asked for.
func TestAContainerHasNoRunToAskFor(t *testing.T) {
	client, image := requireRuntime(t)
	h := newHarness(t)
	h.sup = New(h.buffer, h.services)
	h.sup.Runtime(client)

	svc := container("hostd-suite-notajob", image, `sleep 60`)
	err := h.sup.Adopt(context.Background(), []service.Service{svc})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	_, err = h.sup.RunNow(svc.Name)
	_, notAJob := errors.AsType[ErrNotAJob](err)
	if !notAJob {
		t.Fatalf("asking a container for a run returned %v", err)
	}
	if !strings.Contains(err.Error(), "service start") {
		t.Fatalf("the refusal does not name the operation that IS the right one: %v", err)
	}
}

func TestAskingARunOfSomethingNobodyDeclaredIsNotFound(t *testing.T) {
	h := newHarness(t)
	h.sup = New(h.buffer, h.services)

	_, err := h.sup.RunNow("nothing-declares-this")
	_, unknown := errors.AsType[ErrUnknownService](err)
	if !unknown {
		t.Fatalf("RunNow of an undeclared service returned %v", err)
	}
}
