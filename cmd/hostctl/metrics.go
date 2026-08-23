package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/crgimenes/hostd/api"
	"github.com/crgimenes/hostd/metrics"
)

func runMetrics(ctx context.Context, client *api.Client, opt options) (int, error) {
	req := api.Request{
		Op:      api.OpMetrics,
		Scope:   opt.scope,
		Service: opt.service,
		Metric:  opt.metric,
		Limit:   opt.limit,
	}
	if opt.service != "" && opt.scope == "" {
		req.Scope = metrics.ScopeService
	}
	now := time.Now()
	if opt.window > 0 {
		req.FromMS = float64(now.Add(-opt.window).UnixMilli())
		req.ToMS = float64(now.UnixMilli())
		req.StepMS = float64(opt.step.Milliseconds())
	}

	resp, err := client.Do(ctx, req)
	if err != nil {
		return exitComms, err
	}
	if resp.Failed() {
		return codeFor(resp.Err()), resp.Err()
	}
	var series []metrics.Series
	err = decode(ctx, resp.Body, &series)
	if err != nil {
		return exitFailed, err
	}
	emit(opt, resp.Body, func() {
		fmt.Printf("host %s, generation %d\n", client.Target(), resp.Generation)
		if len(series) == 0 {
			fmt.Println("nothing has been sampled yet; hostd samples every " + metrics.SampleInterval.String())
			return
		}
		if opt.window > 0 {
			printWindow(series)
			return
		}
		printLatest(series, now)
	})
	return exitOK, nil
}

func printLatest(series []metrics.Series, now time.Time) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "SCOPE\tNAME\tMETRIC\tVALUE\tAGE")
	for _, s := range series {
		if len(s.Points) == 0 {
			continue
		}
		point := s.Points[len(s.Points)-1]
		age := now.Sub(time.UnixMilli(int64(point.TimeMS))).Truncate(time.Second)
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			s.Scope, nameOrDash(s.Name), s.Metric, formatValue(s.Metric, point.Value), age)
	}
	_ = w.Flush()
}

// A window is a shape, not a number: the summary is what a person can read,
// and -filo carries the points for anything that draws them.
func printWindow(series []metrics.Series) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "SCOPE\tNAME\tMETRIC\tPOINTS\tMIN\tAVG\tMAX\tLAST")
	for _, s := range series {
		if len(s.Points) == 0 {
			continue
		}
		low, average, high := summarise(s.Points)
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			s.Scope, nameOrDash(s.Name), s.Metric, len(s.Points),
			formatValue(s.Metric, low), formatValue(s.Metric, average), formatValue(s.Metric, high),
			formatValue(s.Metric, s.Points[len(s.Points)-1].Value))
	}
	_ = w.Flush()
}

func summarise(points []metrics.Point) (low float64, average float64, high float64) {
	low, high = points[0].Value, points[0].Value
	total := 0.0
	for _, p := range points {
		total += p.Value
		low = min(low, p.Value)
		high = max(high, p.Value)
	}
	return low, total / float64(len(points)), high
}

func nameOrDash(name string) string {
	if name == "" {
		return "-"
	}
	return name
}

// Raw bytes are the contract on the wire; a person reading a column wants the
// unit the number is in.
func formatValue(metric string, value float64) string {
	switch {
	case strings.HasSuffix(metric, "-percent"):
		return fmt.Sprintf("%.1f%%", value)
	case strings.HasSuffix(metric, "-bytes-per-second"):
		return formatBytes(value) + "/s"
	case strings.HasSuffix(metric, "-bytes"):
		return formatBytes(value)
	default:
		return fmt.Sprintf("%.2f", value)
	}
}

func formatBytes(value float64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for value >= 1024 && i < len(units)-1 {
		value /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f %s", value, units[i])
	}
	return fmt.Sprintf("%.1f %s", value, units[i])
}
