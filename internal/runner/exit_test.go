package runner

import (
	"os/exec"
	"strings"
	"testing"
)

// The distinction that decides what to do next: a tool that failed and said why,
// versus a process that was stopped and never got to. Reading the second as the
// first sends you looking through output for a cause that was never written.
func TestDescribeExitSeparatesFailureFromKill(t *testing.T) {
	t.Run("ordinary failure", func(t *testing.T) {
		err := exec.Command("sh", "-c", "exit 3").Run()
		if err == nil {
			t.Fatal("expected a failure")
		}
		got := describeExit(err)
		if !strings.Contains(got, "status 3") {
			t.Errorf("describeExit = %q, want the exit status", got)
		}
		if strings.Contains(got, "signal") {
			t.Errorf("an ordinary failure was described as a signal: %q", got)
		}
	})

	t.Run("killed", func(t *testing.T) {
		// SIGKILL is what a container's memory limit looks like from in here.
		err := exec.Command("sh", "-c", "kill -9 $$").Run()
		if err == nil {
			t.Fatal("expected a signal")
		}
		got := describeExit(err)
		if !strings.Contains(got, "signal 9") {
			t.Errorf("describeExit = %q, want it to name the signal", got)
		}
		if !strings.Contains(got, "nothing above this line explains it") {
			t.Errorf("describeExit = %q, want it to say the output is not the cause", got)
		}
		if !strings.Contains(got, "memory limit") {
			t.Errorf("describeExit = %q, want SIGKILL to point at the memory limit", got)
		}
	})

	t.Run("not an exit error", func(t *testing.T) {
		got := describeExit(exec.ErrNotFound)
		if !strings.Contains(got, "run failed") {
			t.Errorf("describeExit = %q", got)
		}
	})
}
