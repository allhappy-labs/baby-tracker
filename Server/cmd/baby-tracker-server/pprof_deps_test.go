package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The release binary links no net/http/pprof; only the identitytest build
// does (the second listing proves the check can see it).
func TestReleaseDependencyGraphExcludesPprof(t *testing.T) {
	list := func(args ...string) []string {
		t.Helper()
		command := exec.Command("go", append([]string{"list", "-deps"}, append(args, "./cmd/baby-tracker-server")...)...)
		command.Dir = "../.."
		command.Env = append(os.Environ(), "GOFLAGS=")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("go list %v: %v: %s", args, err, output)
		}
		return strings.Fields(string(output))
	}
	has := func(deps []string, pkg string) bool {
		for _, dep := range deps {
			if dep == pkg {
				return true
			}
		}
		return false
	}
	if has(list(), "net/http/pprof") {
		t.Fatal("the release server links net/http/pprof")
	}
	if !has(list("-tags", "identitytest"), "net/http/pprof") {
		t.Fatal("the identitytest server does not link net/http/pprof: the release check proves nothing")
	}
}
