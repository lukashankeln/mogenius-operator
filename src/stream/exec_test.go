package stream

import (
	"slices"
	"testing"
)

// TRUNCATED goes out before the exit, whether a writer cut the output or
// the command's buffer did (a session command).
func TestExecClosingFrames(t *testing.T) {
	cases := []struct {
		name    string
		outcome ExecStreamOutcome
		writers bool
		want    []string
	}{
		{"plain", ExecStreamOutcome{ExitCode: 3}, false, []string{"EXIT:3"}},
		{"writer cut", ExecStreamOutcome{}, true, []string{"TRUNCATED", "EXIT:0"}},
		{"buffer cut", ExecStreamOutcome{Truncated: true}, false, []string{"TRUNCATED", "EXIT:0"}},
		{"cut and timed out", ExecStreamOutcome{ExitCode: 137, TimedOut: true, Truncated: true}, true, []string{"TRUNCATED", "TIMEOUT", "EXIT:137"}},
	}
	for _, c := range cases {
		if got := execClosingFrames(c.outcome, c.writers); !slices.Equal(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
