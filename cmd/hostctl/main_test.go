package main

import (
	"flag"
	"strings"
	"testing"
)

// The flag package stops at the first positional argument, which would turn
// `hostctl log -follow` into a search for the text "-follow". A command that
// quietly does something else is worse than one that fails.
func TestFlagsAreAcceptedAnywhere(t *testing.T) {
	cases := []struct {
		args      []string
		wantWords []string
		wantLimit int
		wantFollw bool
	}{
		{[]string{"log"}, []string{"log"}, 200, false},
		{[]string{"-limit", "5", "log"}, []string{"log"}, 5, false},
		{[]string{"log", "-limit", "5"}, []string{"log"}, 5, false},
		{[]string{"log", "-follow"}, []string{"log"}, 200, true},
		{[]string{"log", "-limit", "5", "timeout"}, []string{"log", "timeout"}, 5, false},
		{[]string{"service", "restart", "api"}, []string{"service", "restart", "api"}, 200, false},
		{[]string{"service", "-filo", "list"}, []string{"service", "list"}, 200, false},
	}
	for _, c := range cases {
		var limit int
		var follow, filoOut bool
		flags := flag.NewFlagSet("hostctl", flag.ContinueOnError)
		flags.SetOutput(&strings.Builder{})
		flags.IntVar(&limit, "limit", 200, "")
		flags.BoolVar(&follow, "follow", false, "")
		flags.BoolVar(&filoOut, "filo", false, "")

		words, err := parseAnywhere(flags, c.args)
		if err != nil {
			t.Errorf("%v: %v", c.args, err)
			continue
		}
		if strings.Join(words, " ") != strings.Join(c.wantWords, " ") {
			t.Errorf("%v: words = %v, want %v", c.args, words, c.wantWords)
		}
		if limit != c.wantLimit {
			t.Errorf("%v: limit = %d, want %d", c.args, limit, c.wantLimit)
		}
		if follow != c.wantFollw {
			t.Errorf("%v: follow = %v, want %v", c.args, follow, c.wantFollw)
		}
	}
}

func TestUnknownFlagIsRejected(t *testing.T) {
	flags := flag.NewFlagSet("hostctl", flag.ContinueOnError)
	flags.SetOutput(&strings.Builder{})
	_, err := parseAnywhere(flags, []string{"log", "-nonsense"})
	if err == nil {
		t.Fatal("an unknown flag was accepted")
	}
}
