//go:build identitytest

package capacity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSwiftAttestationRejectsStaleInputsAndExecutable(t *testing.T) {
	root := t.TempDir()
	pkg := filepath.Join(root, "package")
	os.Mkdir(pkg, 0700)
	source := filepath.Join(pkg, "Package.swift")
	os.WriteFile(source, []byte("source A"), 0600)
	exe := filepath.Join(root, "tests")
	os.WriteFile(exe, []byte("executable A"), 0700)
	inputs, err := swiftInputHash(pkg)
	if err != nil {
		t.Fatal(err)
	}
	binary, err := swiftFileHash(exe)
	if err != nil {
		t.Fatal(err)
	}
	artifact := SwiftArtifact{PackagePath: pkg, InputSHA256: inputs, Executable: exe, ExecutableSHA256: binary}
	runner := filepath.Join(root, "helper")
	os.WriteFile(runner, []byte("helper A"), 0700)
	artifact.Runner = runner
	artifact.RunnerSHA256, _ = swiftFileHash(runner)
	if err := artifact.Validate(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(runner, []byte("helper B"), 0700)
	if artifact.Validate() == nil {
		t.Fatal("changed helper accepted")
	}
	os.WriteFile(runner, []byte("helper A"), 0700)
	os.WriteFile(source, []byte("source B"), 0600)
	if artifact.Validate() == nil {
		t.Fatal("stale source accepted")
	}
	os.WriteFile(source, []byte("source A"), 0600)
	os.WriteFile(exe, []byte("executable B"), 0700)
	if artifact.Validate() == nil {
		t.Fatal("mismatched executable accepted")
	}
	os.WriteFile(exe, []byte("executable A"), 0700)
	resource := filepath.Join(pkg, "resource.json")
	os.WriteFile(resource, []byte("new resource"), 0600)
	if artifact.Validate() == nil {
		t.Fatal("unattested package resource accepted")
	}
	os.Remove(resource)
	os.Symlink(source, resource)
	if _, err := swiftInputHash(pkg); err == nil {
		t.Fatal("unattested symlink input accepted")
	}
}

func TestSwiftGraphRejectsExternalAndEscapingInputs(t *testing.T) {
	for _, raw := range []string{
		`{"dependencies":[{}],"targets":[{"type":"regular"}]}`,
		`{"dependencies":[],"targets":[{"type":"regular","path":"../outside"}]}`,
		`{"dependencies":[],"targets":[{"type":"regular","sources":["../outside"]}]}`,
		`{"dependencies":[],"targets":[{"type":"regular","resources":[{"path":"../outside"}]}]}`,
		`{"dependencies":[],"targets":[{"type":"regular","settings":[{"tool":"swift","kind":{"unsafeFlags":{"_0":["-I/outside"]}}}]}]}`,
	} {
		if validateSwiftGraph([]byte(raw)) == nil {
			t.Fatal("unsupported graph accepted", raw)
		}
	}
	if err := validateSwiftGraph([]byte(`{"dependencies":[],"targets":[{"type":"regular","resources":[{"path":"inside.json"}]}]}`)); err != nil {
		t.Fatal(err)
	}
}

func TestSwiftCommandLoadsExactAttestedBundle(t *testing.T) {
	a := SwiftArtifact{Executable: "/owned/Tests.xctest/Contents/MacOS/Tests", Runner: "/toolchain/swiftpm-testing-helper", RuntimeEnvironment: []string{"DYLD_FRAMEWORK_PATH=/sdk/Frameworks"}}
	cmd := a.testCommand(t.Context(), "OnlyThisTest")
	found := false
	for _, v := range cmd.Env {
		found = found || v == "DYLD_FRAMEWORK_PATH=/sdk/Frameworks"
	}
	if !found {
		t.Fatal("missing scoped loader environment")
	}
	if cmd.Path != a.Runner || len(cmd.Args) != 7 || cmd.Args[1] != "--test-bundle-path" || cmd.Args[2] != a.Executable || cmd.Args[6] != "OnlyThisTest" {
		t.Fatalf("wrong Darwin bundle handoff: %v", cmd.Args)
	}
}

func TestSwiftInputHashExcludesCacheFileWithoutSkippingSources(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: elsewhere"), 0600)
	source := filepath.Join(root, "Package.swift")
	os.WriteFile(source, []byte("A"), 0600)
	a, err := swiftInputHash(root)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(source, []byte("B"), 0600)
	b, err := swiftInputHash(root)
	if err != nil || a == b {
		t.Fatal("excluded non-directory skipped package sources", err)
	}
}
