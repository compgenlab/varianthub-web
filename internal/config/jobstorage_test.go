package config

import (
	"strings"
	"testing"
)

// A relative job storage path is always wrong, and wrong in a way that looks
// fine from one process: it resolves against each process's working directory,
// so the API and the worker would each get their own. The first upload then
// fails in the worker with "no such file", pointing at the worker rather than
// at the line of configuration that caused it.
func TestARelativeJobStoragePathIsRefused(t *testing.T) {
	for _, p := range []string{"jobs", "./jobs", "../shared/jobs"} {
		c := Defaults()
		c.JobStorage = p
		err := c.checkJobStorage("config.toml")
		if err == nil {
			t.Errorf("%q was accepted; each process would resolve it separately", p)
			continue
		}
		// The message has to explain the failure, not just name it — the reason
		// a relative path cannot work is the whole content of the error.
		if !strings.Contains(err.Error(), "working directory") {
			t.Errorf("%q: error does not say why: %v", p, err)
		}
	}
}

func TestAnAbsolutePathAndABucketAreBothAccepted(t *testing.T) {
	cases := map[string]string{
		"/mnt/jobs":              "/mnt/jobs",
		"/mnt/jobs/":             "/mnt/jobs",
		"/mnt//jobs/./":          "/mnt/jobs",
		"s3://bucket/jobs":       "s3://bucket/jobs",
		"s3://bucket/jobs/":      "s3://bucket/jobs",
		"s3://bucket/some/where": "s3://bucket/some/where",
	}
	for in, want := range cases {
		c := Defaults()
		c.JobStorage = in
		if err := c.checkJobStorage("config.toml"); err != nil {
			t.Errorf("%q was refused: %v", in, err)
			continue
		}
		// Normalized, so two spellings of one location cannot become two
		// prefixes — a job written under one and looked for under the other is
		// a job whose input has vanished.
		if c.JobStorage != want {
			t.Errorf("%q normalized to %q, want %q", in, c.JobStorage, want)
		}
	}
}

// Empty is refused rather than defaulted, because there is no safe guess: the
// two processes must agree, and picking one for them is how they come to
// disagree silently.
func TestEmptyJobStorageIsRefusedWithSomethingActionable(t *testing.T) {
	c := Defaults()
	c.JobStorage = "   "
	err := c.checkJobStorage("/etc/varianthub-web/config.toml")
	if err == nil {
		t.Fatal("empty job storage was accepted")
	}
	for _, want := range []string{"worker.job_storage", "VHW_JOB_STORAGE", "/etc/varianthub-web/config.toml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should mention %q so it can be acted on: %v", want, err)
		}
	}
}

func TestABucketWithNoNameIsRefused(t *testing.T) {
	c := Defaults()
	c.JobStorage = "s3://"
	if err := c.checkJobStorage("config.toml"); err == nil {
		t.Error("s3:// with no bucket was accepted")
	}
}

// The shipped default has to pass its own validation, or a fresh single-process
// install fails at startup on a value nobody set.
func TestTheDefaultJobStorageIsValid(t *testing.T) {
	c := Defaults()
	if err := c.checkJobStorage("config.toml"); err != nil {
		t.Errorf("the built-in default %q does not validate: %v", Defaults().JobStorage, err)
	}
}
