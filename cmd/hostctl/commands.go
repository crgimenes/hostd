package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/crgimenes/hostd/internal/api"
	"github.com/crgimenes/hostd/internal/supervisor"
)

// emit writes a result. With --filo, stdout carries a Filo expression and
// nothing else, because a caller parsing it must not have to skip a heading.
func emit(opt options, filoBody string, human func()) {
	if opt.filoOut {
		fmt.Println(strings.TrimRight(filoBody, "\n"))
		return
	}
	human()
}

func runDescribe(ctx context.Context, client *api.Client, opt options) (int, error) {
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
	emit(opt, resp.Body, func() {
		fmt.Printf("host      %s\n", client.Target())
		fmt.Printf("version   %s\n", d.Version)
		fmt.Printf("protocol  %d\n", d.Protocol)
		fmt.Printf("schema    %d\n", d.Schema)
		fmt.Printf("supports  %s\n", strings.Join(d.Operations, " "))
	})
	return exitOK, nil
}

func runStatus(ctx context.Context, client *api.Client, opt options) (int, error) {
	return showStatus(ctx, client, opt, api.Request{Op: api.OpStatus})
}

func runService(ctx context.Context, client *api.Client, opt options, args []string) (int, error) {
	if len(args) == 0 {
		return exitUsage, fmt.Errorf("service needs a subcommand: list, start, stop or restart")
	}
	switch args[0] {
	case "list":
		return showStatus(ctx, client, opt, api.Request{Op: api.OpServiceList})
	case "start", "stop", "restart":
		if len(args) < 2 {
			return exitUsage, fmt.Errorf("service %s needs a service name", args[0])
		}
		op := map[string]string{
			"start":   api.OpServiceStart,
			"stop":    api.OpServiceStop,
			"restart": api.OpServiceRestrt,
		}[args[0]]
		return showStatus(ctx, client, opt, api.Request{Op: op, Name: args[1]})
	default:
		return exitUsage, fmt.Errorf("unknown service subcommand %q; expected list, start, stop or restart", args[0])
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
	emit(opt, resp.Body, func() { printStatuses(client.Target(), statuses) })
	return exitOK, nil
}

func printStatuses(target string, statuses []supervisor.Status) {
	// The host a command landed on is printed with the result. "Which machine
	// did I just do that to?" is a question that costs a machine in a fleet.
	fmt.Printf("host %s\n", target)
	if len(statuses) == 0 {
		fmt.Println("no services are declared")
		return
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "SERVICE\tSTATE\tPID\tUPTIME\tRESTARTS\tDETAIL")
	for _, s := range statuses {
		detail := s.LastError
		if detail == "" && s.Adopted {
			detail = "adopted"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n",
			s.Name, s.State, pidText(s.PID), uptime(s), s.Restarts, detail)
	}
	_ = w.Flush()
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

func runApply(ctx context.Context, client *api.Client, opt options) (int, error) {
	resp, err := client.Do(ctx, api.Request{Op: api.OpApply})
	if err != nil {
		return exitComms, err
	}
	var changes []string
	if resp.Body != "" {
		err = decode(ctx, resp.Body, &changes)
		if err != nil {
			return exitFailed, err
		}
	}
	emit(opt, resp.Body, func() {
		fmt.Printf("host %s\n", client.Target())
		if len(changes) == 0 {
			fmt.Println("nothing to change")
			return
		}
		for _, c := range changes {
			fmt.Println(c)
		}
	})
	if resp.Failed() {
		// Some files applied and some did not: the valid part took effect and
		// the refused part is reported, rather than the whole call failing.
		return exitPartial, resp.Err()
	}
	return exitOK, nil
}

func runLog(ctx context.Context, client *api.Client, opt options, args []string) (int, error) {
	req := api.Request{
		Service: opt.service,
		Stream:  opt.stream,
		Limit:   opt.limit,
		Since:   opt.sinceSeq,
	}
	if len(args) > 0 {
		req.Match = strings.Join(args, " ")
	}
	if opt.follow {
		err := client.Follow(ctx, req, func(l api.LogLine) error {
			printLine(opt, l)
			return nil
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
		fmt.Println(strings.TrimRight(resp.Body, "\n"))
		return exitOK, nil
	}
	for _, l := range lines {
		printLine(opt, l)
	}
	return exitOK, nil
}

func printLine(opt options, l api.LogLine) {
	if opt.filoOut {
		// Following in Filo emits one expression per line, so a program can
		// read a stream without waiting for it to end.
		out, err := marshal(l)
		if err == nil {
			fmt.Println(out)
		}
		return
	}
	fmt.Printf("%s %s %s %s\n",
		l.At().Format("2006-01-02 15:04:05"), l.Service, streamMark(l.Stream), l.Text)
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
