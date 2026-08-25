package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/crgimenes/hostd/api"
	"github.com/crgimenes/hostd/filoconf"
	"github.com/crgimenes/hostd/state"
	"github.com/crgimenes/hostd/supervisor"
)

// emit writes the answer in whichever shape the caller asked for. With -filo or
// -json, stdout carries that and nothing else: a caller parsing it must not
// have to skip a heading.
//
// filoBody is what the daemon sent, passed through untouched — re-rendering it
// here would be a second implementation of the wire format. JSON is rendered
// from `structured`, the same answer already decoded for the human path, so
// what an agent reads is the same values the person reads rather than a
// translation of the Filo text. Filo stays the language between the two
// programs; JSON is only a way out.
func emit(opt options, filoBody string, structured any, human func()) {
	out := opt.out
	switch {
	case opt.filoOut:
		_, _ = fmt.Fprintln(out, strings.TrimRight(filoBody, "\n"))
	case opt.jsonOut:
		writeJSON(out, structured)
	default:
		human()
	}
}

// writeJSONLine renders one value on one line, for output that streams. The
// indented form below would break a reader that takes a line at a time, which
// is what every log tool does.
func writeJSONLine(out io.Writer, value any) {
	err := json.NewEncoder(out).Encode(value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hostctl: cannot render a log line as JSON: %v\n", err)
	}
}

// Indented because a person reads this too — an agent does not care, and the
// one who is debugging why the agent did something does.
func writeJSON(out io.Writer, value any) {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	err := encoder.Encode(value)
	if err != nil {
		// stdout carries the answer and nothing else, so a failure to render it
		// goes to stderr rather than leaving half an object on the pipe.
		fmt.Fprintf(os.Stderr, "hostctl: cannot render the answer as JSON: %v\n", err)
	}
}

func runDescribe(ctx context.Context, client *api.Client, opt options) (int, error) {
	out := opt.out
	resp, err := client.Do(ctx, api.Request{Op: api.OpDescribe})
	if err != nil {
		return exitComms, err
	}
	if resp.Failed() {
		return codeFor(resp.Err()), resp.Err()
	}
	var d api.Description
	err = decode(ctx, resp.Body, &d)
	if err != nil {
		return exitFailed, err
	}
	emit(opt, resp.Body, d, func() {
		_, _ = fmt.Fprintf(out, "host      %s\n", client.Target())
		_, _ = fmt.Fprintf(out, "version   %s\n", d.Version)
		_, _ = fmt.Fprintf(out, "protocol  %d\n", d.Protocol)
		_, _ = fmt.Fprintf(out, "schema    %d\n", d.Schema)
		_, _ = fmt.Fprintf(out, "runtime   %s\n", runtimeText(d))
		_, _ = fmt.Fprintf(out, "hardware  %d cpu(s), %s\n", d.CPUs, formatBytes(d.MemoryBytes))
		_, _ = fmt.Fprintf(out, "supports  %s\n", strings.Join(d.Operations, " "))
	})
	return exitOK, nil
}

// A machine with no container runtime can hold declarations and run nothing.
// Printing an empty field would read as a machine that failed to answer.
func runtimeText(d api.Description) string {
	if d.Arch == "" {
		return "none on this machine"
	}
	return fmt.Sprintf("%s on %s", d.Runtime, d.Arch)
}

func runStatus(ctx context.Context, client *api.Client, opt options) (int, error) {
	return showStatus(ctx, client, opt, api.Request{Op: api.OpStatus})
}

func runService(ctx context.Context, client *api.Client, opt options, args []string) (int, error) {
	if len(args) == 0 {
		return exitUsage, fmt.Errorf("service needs a subcommand: list, versions, start, stop or restart")
	}
	switch args[0] {
	case "list":
		return showStatus(ctx, client, opt, api.Request{Op: api.OpServiceList})
	case "versions":
		return runServiceVersions(ctx, client, opt, args[1:])
	case "start", "stop", "restart":
		if len(args) < 2 {
			return exitUsage, fmt.Errorf("service %s needs a service name", args[0])
		}
		op := map[string]string{
			"start":   api.OpServiceStart,
			"stop":    api.OpServiceStop,
			"restart": api.OpServiceRestrt,
		}[args[0]]
		return showStatus(ctx, client, opt, api.Request{
			Op:               op,
			Name:             args[1],
			ExpectGeneration: opt.expectGen,
			OnBehalfOf:       opt.onBehalfOf,
		})
	default:
		return exitUsage, fmt.Errorf("unknown service subcommand %q; expected list, versions, start, stop or restart", args[0])
	}
}

func showStatus(ctx context.Context, client *api.Client, opt options, req api.Request) (int, error) {
	resp, err := client.Do(ctx, req)
	if err != nil {
		return exitComms, err
	}
	if resp.Failed() {
		return codeFor(resp.Err()), resp.Err()
	}
	var statuses []supervisor.Status
	err = decode(ctx, resp.Body, &statuses)
	if err != nil {
		return exitFailed, err
	}
	emit(opt, resp.Body, statuses, func() { printStatuses(opt.out, client.Target(), resp.Generation, statuses) })
	return exitOK, nil
}

func printStatuses(out io.Writer, target string, generation uint64, statuses []supervisor.Status) {
	_, _ = fmt.Fprintf(out, "host %s, generation %d\n", target, generation)
	if len(statuses) == 0 {
		_, _ = fmt.Fprintln(out, "no services are declared")
		return
	}
	slices.SortFunc(statuses, func(a, b supervisor.Status) int { return strings.Compare(a.Name, b.Name) })
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "SERVICE\tSTATE\tPID\tUPTIME\tRUNS\tRESTARTS\tDETAIL")
	for _, s := range statuses {
		detail := s.LastError
		if detail == "" && s.Every != "" {
			detail = "every " + s.Every
		}
		if detail == "" && s.Orphan {
			// On this machine with no file declaring it: worth seeing at a
			// glance, because it is the one thing an apply will not fix.
			detail = "orphan, not declared"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			s.Name, s.State, pidText(s.PID), uptime(s), runsText(s), s.Restarts, detail)
	}
	_ = w.Flush()
}

// A service that is not a job has no runs, and a dash says that better than a
// zero, which reads as "none right now".
func runsText(s supervisor.Status) string {
	if s.Runs == 0 && s.State != supervisor.StateRunning {
		return "-"
	}
	if s.Runs == 0 {
		return "-"
	}
	return fmt.Sprint(s.Runs)
}

func pidText(pid int) string {
	if pid == 0 {
		return "-"
	}
	return fmt.Sprint(pid)
}

func uptime(s supervisor.Status) string {
	if s.State != supervisor.StateRunning || s.Since == 0 {
		return "-"
	}
	d := time.Since(time.UnixMilli(int64(s.Since))).Truncate(time.Second)
	if d < 0 {
		return "-"
	}
	return d.String()
}

func runPlan(ctx context.Context, client *api.Client, opt options) (int, error) {
	return showPlan(ctx, client, opt, api.Request{Op: api.OpPlan}, "nothing to change")
}

func runApply(ctx context.Context, client *api.Client, opt options) (int, error) {
	if opt.dryRun {
		return runPlan(ctx, client, opt)
	}
	req := api.Request{
		Op:               api.OpApply,
		ExpectGeneration: opt.expectGen,
		AllowDestructive: opt.allowDestr,
		OnBehalfOf:       opt.onBehalfOf,
	}
	return showPlan(ctx, client, opt, req, "nothing to change")
}

func showPlan(ctx context.Context, client *api.Client, opt options, req api.Request, empty string) (int, error) {
	out := opt.out
	resp, err := client.Do(ctx, req)
	if err != nil {
		return exitComms, err
	}
	var changes []supervisor.Change
	if resp.Body != "" {
		err = decode(ctx, resp.Body, &changes)
		if err != nil {
			return exitFailed, err
		}
	}
	emit(opt, resp.Body, changes, func() {
		_, _ = fmt.Fprintf(out, "host %s, generation %d\n", client.Target(), resp.Generation)
		if len(changes) == 0 {
			_, _ = fmt.Fprintln(out, empty)
			return
		}
		printChanges(out, changes)
	})
	if !resp.Failed() {
		return exitOK, nil
	}
	if resp.Code == api.CodeFailed {
		return exitPartial, resp.Err()
	}
	return codeFor(resp.Err()), resp.Err()
}

func printChanges(out io.Writer, changes []supervisor.Change) {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "SERVICE\tACTION\tIMPACT\tDETAIL")
	for _, c := range changes {
		impact := "safe"
		switch {
		case c.Destructive:
			impact = "STOPS SERVICE"
		case c.Disruptive:
			impact = "interrupts"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", c.Service, c.Action, impact, c.Detail)
	}
	_ = w.Flush()
}

func runAudit(ctx context.Context, client *api.Client, opt options) (int, error) {
	out := opt.out
	resp, err := client.Do(ctx, api.Request{Op: api.OpAudit, Limit: opt.limit})
	if err != nil {
		return exitComms, err
	}
	if resp.Failed() {
		return codeFor(resp.Err()), resp.Err()
	}
	var entries []state.Entry
	err = decode(ctx, resp.Body, &entries)
	if err != nil {
		return exitFailed, err
	}
	emit(opt, resp.Body, entries, func() {
		_, _ = fmt.Fprintf(out, "host %s, generation %d\n", client.Target(), resp.Generation)
		if len(entries) == 0 {
			_, _ = fmt.Fprintln(out, "nothing has changed this host yet")
			return
		}
		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "WHEN\tACTOR\tOPERATION\tTARGET\tGEN\tRESULT\tDETAIL")
		for _, e := range entries {
			actor := e.Actor
			if e.OnBehalfOf != "" {
				actor += " for " + e.OnBehalfOf
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d->%d\t%s\t%s\n",
				time.UnixMilli(int64(e.TimeMS)).Format("2006-01-02 15:04:05"),
				actor, e.Operation, e.Target, e.Before, e.After, e.Result, e.Detail)
		}
		_ = w.Flush()
	})
	return exitOK, nil
}

func runLog(ctx context.Context, client *api.Client, opt options, args []string) (int, error) {
	out := opt.out
	req := api.Request{
		Service: opt.service,
		Stream:  opt.stream,
		Kind:    opt.kind,
		Run:     opt.run,
		Limit:   opt.limit,
		Since:   opt.sinceSeq,
	}
	if len(args) > 0 {
		req.Match = strings.Join(args, " ")
	}
	if opt.follow {
		err := client.Follow(ctx, req, func(l api.LogLine) error {
			return printLine(opt, l)
		})
		if err != nil {
			return exitComms, err
		}
		return exitOK, nil
	}

	req.Op = api.OpLogSearch
	resp, err := client.Do(ctx, req)
	if err != nil {
		return exitComms, err
	}
	if resp.Failed() {
		return codeFor(resp.Err()), resp.Err()
	}
	var lines []api.LogLine
	err = decode(ctx, resp.Body, &lines)
	if err != nil {
		return exitFailed, err
	}
	if opt.filoOut {
		_, _ = fmt.Fprintln(out, strings.TrimRight(resp.Body, "\n"))
		return exitOK, nil
	}
	for _, l := range lines {
		err = printLine(opt, l)
		if err != nil {
			return exitFailed, err
		}
	}
	return exitOK, nil
}

func printLine(opt options, l api.LogLine) error {
	out := opt.out
	// One object per line, never one array: a follower has no last line, and a
	// reader that waited for the closing bracket would wait forever. This is
	// the shape every log-reading tool already expects of a stream.
	if opt.jsonOut {
		writeJSONLine(out, l)
		return nil
	}
	if !opt.filoOut {
		// A line from a job says which run wrote it: several runs of one job
		// write at the same time on purpose.
		where := l.Service
		if l.Run != "" {
			where += "/" + l.Run
		}
		_, _ = fmt.Fprintf(out, "%s %s %s %s\n",
			l.At().Format("2006-01-02 15:04:05"), where, streamMark(l.Stream), l.Text)
		return nil
	}
	rendered, err := filoconf.Marshal(l)
	if err != nil {
		return fmt.Errorf("cannot render log line %d: %w", l.Seq, err)
	}
	_, _ = fmt.Fprintln(out, rendered)
	return nil
}

func streamMark(stream string) string {
	switch stream {
	case "stderr":
		return "E"
	case "event":
		return "*"
	default:
		return "|"
	}
}
