package version

import (
	"cmp"
	"strconv"
	"strings"
)

// Compare orders two release versions the way their tags do: v0.0.4 is newer
// than v0.0.3.
//
// The bool is false when either side is not a release version — a development
// build, or whatever an unknown daemon chose to answer. Guessing an order there
// is how a fleet gets silently taken backwards by a client that is itself the
// old one, so "cannot tell" is an answer this returns rather than a case it
// papers over.
func Compare(a, b string) (int, bool) {
	first, readable := parse(a)
	if !readable {
		return 0, false
	}
	second, readable := parse(b)
	if !readable {
		return 0, false
	}
	for at := range first {
		if first[at] != second[at] {
			return cmp.Compare(first[at], second[at]), true
		}
	}
	return 0, true
}

// Exactly three numbers, with or without the leading v the tags carry. A suffix
// of any kind is not read as an order: a release candidate and a release are
// not something this has been asked to rank.
func parse(release string) ([3]int, bool) {
	var out [3]int
	fields := strings.Split(strings.TrimPrefix(strings.TrimSpace(release), "v"), ".")
	if len(fields) != 3 {
		return out, false
	}
	for at, field := range fields {
		if field == "" || strings.IndexFunc(field, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return out, false
		}
		number, err := strconv.Atoi(field)
		if err != nil {
			return out, false
		}
		out[at] = number
	}
	return out, true
}
