package main

import (
	"fmt"
	"html/template"
	"slices"
	"strings"

	"github.com/crgimenes/hostd/metrics"
)

// Charts are drawn where the numbers are, in SVG, rather than shipped to the
// window as data for a script to plot: the picture then changes only when the
// picture changed, which is the whole point of sending fragments.
//
// The box is a thousand units wide by a hundred tall and is stretched to
// whatever width the card has; strokes are told not to stretch with it, and
// nothing inside is text, because text would stretch.
const (
	plotWidth  = 1000.0
	plotHeight = 100.0
)

type layer struct {
	Label  string
	Points []metrics.Point
	Colour string
	Area   bool
	Stack  bool
	// A ceiling the series cannot exceed, so a percentage keeps its meaning
	// instead of being rescaled to whatever the highest sample happened to be.
	Top float64
}

func chartOf(layers []layer, view viewState, format func(*float64) string, short bool) chartView {
	from, to := window(view, layers)
	top := ceiling(layers)
	out := chartView{Short: short, Top: format(&top)}
	for _, one := range layers {
		out.Legend = append(out.Legend, legendView{
			Label:  one.Label,
			Colour: one.Colour,
			Value:  format(lastOf(one.Points)),
		})
	}

	var body strings.Builder
	for _, fraction := range []float64{0.25, 0.5, 0.75, 1} {
		y := plotHeight - plotHeight*fraction
		fmt.Fprintf(&body, `<line class="grid" x1="0" y1="%.1f" x2="%.0f" y2="%.1f"/>`, y, plotWidth, y)
	}
	drawStack(&body, layers, from, to, top)
	for _, one := range layers {
		if one.Stack || len(one.Points) < 2 {
			continue
		}
		line := path(one.Points, from, to, top)
		if one.Area {
			fmt.Fprintf(&body, `<path fill="%s" fill-opacity="0.18" d="%s L %.2f %.0f L %.2f %.0f Z"/>`,
				one.Colour, line,
				at(one.Points[len(one.Points)-1].TimeMS, from, to), plotHeight,
				at(one.Points[0].TimeMS, from, to), plotHeight)
		}
		fmt.Fprintf(&body, `<path fill="none" stroke="%s" stroke-width="1.5" vector-effect="non-scaling-stroke" d="%s"/>`,
			one.Colour, line)
	}
	// #nosec G203 -- the picture is numbers and colours from a fixed palette,
	// with no text in it at all: every label the fleet sent goes to the legend,
	// which the template escapes
	out.Body = template.HTML(body.String())
	return out
}

// The live window ends at the NEWEST SAMPLE, not at the clock. Anchoring it to
// now would redraw the chart every two seconds for a picture that only changes
// when a sample lands, and a fragment that changes is a fragment that gets
// swapped in front of somebody who was reading it.
func window(view viewState, layers []layer) (from, to float64) {
	if view.from > 0 {
		return view.from, view.to
	}
	for _, one := range layers {
		for _, point := range one.Points {
			to = max(to, point.TimeMS)
		}
	}
	if to == 0 {
		return 0, 0
	}
	return to - float64(view.window)*1000, to
}

func ceiling(layers []layer) float64 {
	top := 0.0
	stacked := false
	for _, one := range layers {
		if one.Stack {
			stacked = true
			continue
		}
		if one.Top > 0 {
			top = max(top, one.Top)
			continue
		}
		for _, point := range one.Points {
			top = max(top, point.Value)
		}
	}
	if stacked {
		for _, when := range instants(layers) {
			sum := 0.0
			for _, one := range layers {
				sum += valueAt(one.Points, when)
			}
			top = max(top, sum)
		}
	}
	if top == 0 {
		return 1
	}
	return top
}

func at(when, from, to float64) float64 {
	if to <= from {
		return 0
	}
	return (when - from) / (to - from) * plotWidth
}

func path(points []metrics.Point, from, to, top float64) string {
	var out strings.Builder
	for index, point := range points {
		verb := "L"
		if index == 0 {
			verb = "M"
		}
		fmt.Fprintf(&out, "%s %.2f %.2f ", verb,
			at(point.TimeMS, from, to), plotHeight-min(point.Value, top)/top*plotHeight)
	}
	return strings.TrimSpace(out.String())
}

// One time axis for every series: a stack drawn on each series' own instants
// would put the layers on different positions and add up to nothing.
func instants(layers []layer) []float64 {
	seen := map[float64]bool{}
	var out []float64
	for _, one := range layers {
		if !one.Stack {
			continue
		}
		for _, point := range one.Points {
			if seen[point.TimeMS] {
				continue
			}
			seen[point.TimeMS] = true
			out = append(out, point.TimeMS)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func valueAt(points []metrics.Point, when float64) float64 {
	value := 0.0
	for _, point := range points {
		if point.TimeMS > when {
			break
		}
		value = point.Value
	}
	return value
}

func drawStack(body *strings.Builder, layers []layer, from, to, top float64) {
	moments := instants(layers)
	if len(moments) < 2 {
		return
	}
	floor := make([]float64, len(moments))
	for _, one := range layers {
		if !one.Stack {
			continue
		}
		var region strings.Builder
		for index, when := range moments {
			verb := "L"
			if index == 0 {
				verb = "M"
			}
			height := min(floor[index]+valueAt(one.Points, when), top)
			fmt.Fprintf(&region, "%s %.2f %.2f ", verb, at(when, from, to), plotHeight-height/top*plotHeight)
		}
		for index, moment := range slices.Backward(moments) {
			fmt.Fprintf(&region, "L %.2f %.2f ", at(moment, from, to),
				plotHeight-min(floor[index], top)/top*plotHeight)
		}
		fmt.Fprintf(body, `<path fill="%s" fill-opacity="0.8" stroke="%s" stroke-width="0.75" vector-effect="non-scaling-stroke" d="%sZ"/>`,
			one.Colour, one.Colour, region.String())
		for index, when := range moments {
			floor[index] += valueAt(one.Points, when)
		}
	}
}
