package scripts

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestTask4ImmutabilityGateCleanAndMutated(t *testing.T) {
	repository := t.TempDir()
	protected := []struct {
		path     string
		accepted []byte
	}{
		{"Server/migrations/000005_identity_sessions.sql", []byte("-- accepted identity schema\n")},
		{"Server/internal/migrate/compatibility.json", []byte("{\"min\":5,\"max\":5}\n")},
	}

	runTask4Git(t, repository, "init", "--quiet")
	runTask4Git(t, repository, "config", "user.name", "Task 4 Immutability Test")
	runTask4Git(t, repository, "config", "user.email", "task4-immutability@example.invalid")
	// A throwaway repository inherits the developer's global git config,
	// including commit signing. This project signs over SSH, so on a machine
	// whose agent is not reachable from the test's environment the gate failed
	// with "Couldn't find key in agent" — a red server suite caused entirely by
	// the tester's keyring. Nothing here is signed for a reason.
	runTask4Git(t, repository, "config", "commit.gpgsign", "false")
	for _, file := range protected {
		writeTask4ProtectedFile(t, repository, file.path, file.accepted)
	}
	runTask4Git(t, repository, "add", "--", protected[0].path, protected[1].path)
	runTask4Git(t, repository, "commit", "--quiet", "-m", "accepted task 3 identity schema")
	acceptedRef := runTask4Git(t, repository, "rev-parse", "HEAD")

	diffArgs := []string{"diff", "--quiet", acceptedRef, "--", protected[0].path, protected[1].path}
	if output, err := task4Git(repository, diffArgs...); err != nil {
		t.Fatalf("clean immutability gate failed: %v: %s", err, output)
	}

	for _, mutation := range protected {
		mutation := mutation
		t.Run(filepath.Base(mutation.path), func(t *testing.T) {
			for _, file := range protected {
				writeTask4ProtectedFile(t, repository, file.path, file.accepted)
			}
			writeTask4ProtectedFile(t, repository, mutation.path, append(append([]byte(nil), mutation.accepted...), []byte("mutated\n")...))

			output, err := task4Git(repository, diffArgs...)
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) || exitError.ExitCode() != 1 {
				t.Fatalf("immutability gate accepted mutation of %s: err=%v output=%s", mutation.path, err, output)
			}
		})
	}
}

func writeTask4ProtectedFile(t *testing.T, repository, relativePath string, contents []byte) {
	t.Helper()
	path := filepath.Join(repository, filepath.FromSlash(relativePath))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
}

func runTask4Git(t *testing.T, repository string, args ...string) string {
	t.Helper()
	output, err := task4Git(repository, args...)
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return string(trimTask4Space(output))
}

func task4Git(repository string, args ...string) ([]byte, error) {
	command := exec.Command("git", append([]string{"-C", repository}, args...)...)
	return command.CombinedOutput()
}

func trimTask4Space(value []byte) []byte {
	for len(value) > 0 {
		switch value[len(value)-1] {
		case ' ', '\t', '\r', '\n':
			value = value[:len(value)-1]
		default:
			return value
		}
	}
	return value
}
