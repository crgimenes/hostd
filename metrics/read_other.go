//go:build !linux

package metrics

// Development happens on macOS and the targets are Linux. Reporting the
// absence is the honest answer: a sampler that invented numbers here would
// draw a graph of nothing.
type systemSource struct{}

func (systemSource) host() (hostCounters, error) {
	return hostCounters{}, ErrUnsupported
}

func (systemSource) process(int) (procCounters, error) {
	return procCounters{}, ErrUnsupported
}
