package api

import (
	"errors"

	"github.com/crgimenes/hostd/state"
	"github.com/crgimenes/hostd/supervisor"
)

// What a run asked for by hand answers with. The id is what the timeline calls
// this run, so following it is `hostctl log -run <id>` and nothing has to be
// guessed from timestamps.
type JobRun struct {
	Service string `filo:"service" json:"service"`
	Run     string `filo:"run" json:"run"`
}

// runJob starts one turn of a job because somebody asked. It answers as soon as
// the run exists rather than when it ends: a job may take an hour, and a
// command that waited would time out on exactly the jobs worth watching.
//
// Audited, and the generation does not move. A generation is desired state, and
// one turn of a job never was part of it — the declaration says the same thing
// before and after.
func (s *Server) runJob(req Request, actor string) Response {
	if req.Name == "" {
		return Response{Code: CodeInvalid, Message: "which job should run?"}
	}
	entry := state.Entry{Operation: req.Op, Target: req.Name, Actor: actor, OnBehalfOf: req.OnBehalfOf}
	err := s.store.Check(req.ExpectGeneration)
	if err != nil {
		return s.refuse(entry, CodeConflict, err)
	}
	run, err := s.sup.RunNow(req.Name)
	if err != nil {
		return s.runFailed(entry, err)
	}
	entry.Result = state.ResultOK
	entry.Detail = "run " + run
	resp := body(JobRun{Service: req.Name, Run: run})
	resp.Generation = s.store.Record(entry, false)
	return resp
}

// A job at its ceiling is a job doing what its declaration says, so it is
// refused rather than failed: an agent that read it as a failure would ask
// again, and again, for as long as the ceiling held.
func (s *Server) runFailed(entry state.Entry, err error) Response {
	_, unknown := errors.AsType[supervisor.ErrUnknownService](err)
	if unknown {
		return s.refuse(entry, CodeNotFound, err)
	}
	// The wrong thing asked of the right service is the caller's mistake, so
	// it exits as a usage error rather than as an operation that tried and
	// failed: an agent that retried this would retry for ever.
	_, notAJob := errors.AsType[supervisor.ErrNotAJob](err)
	if notAJob {
		return s.refuse(entry, CodeInvalid, err)
	}
	_, refused := errors.AsType[supervisor.ErrRunRefused](err)
	if refused {
		return s.refuse(entry, CodeRefused, err)
	}
	entry.Result = state.ResultFailed
	entry.Detail = err.Error()
	return Response{Code: CodeFailed, Message: err.Error(), Generation: s.store.Record(entry, false)}
}
