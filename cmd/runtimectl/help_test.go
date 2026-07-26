package main

import (
	"os/exec"
	"strings"
	"testing"
)

// TestHelpFlagsExitZero pins the contract the container smoke test in ci.yml
// depends on: `runtimectl --help` is a successful invocation, not a usage
// error. It regressed silently because the CI step was added in the same
// commit that assumed the behaviour, and nothing else asserted it — every
// help form fell through to the default branch and exited 2.
//
// This builds the real binary and runs it rather than calling usage()
// directly, because os.Exit is exactly what is under test: an in-process call
// would terminate the test binary itself.
func TestHelpFlagsExitZero(t *testing.T) {
	bin := t.TempDir() + "/runtimectl"
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	for _, arg := range []string{"--help", "-h", "help"} {
		t.Run(arg, func(t *testing.T) {
			out, err := exec.Command(bin, arg).CombinedOutput()
			if err != nil {
				t.Fatalf("runtimectl %s exited non-zero (%v); output:\n%s", arg, err, out)
			}
			if !strings.Contains(string(out), "usage: runtimectl") {
				t.Fatalf("runtimectl %s printed no usage line; output:\n%s", arg, out)
			}
		})
	}
}

// TestUnknownCommandStillFails is the security counterpart: making help
// succeed must not make a typo or an unsupported subcommand succeed too.
func TestUnknownCommandStillFails(t *testing.T) {
	bin := t.TempDir() + "/runtimectl"
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	if out, err := exec.Command(bin, "definitely-not-a-command").CombinedOutput(); err == nil {
		t.Fatalf("unknown command exited zero; output:\n%s", out)
	}
}
