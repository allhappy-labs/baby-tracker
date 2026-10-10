//go:build identitytest

package capacity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var errSwiftArtifact = errors.New("capacity_swift_artifact_mismatch")
var errSwiftGraph = errors.New("capacity_swift_dependency_graph_unsupported")

// SwiftArtifact is independent of the server image identity. No cached test
// executable is eligible: every invocation builds in a new private scratch root.
type SwiftArtifact struct {
	RuntimeEnvironment []string          `json:"runtime_environment"`
	RuntimeImages      map[string]string `json:"runtime_image_sha256"`
	Runner             string            `json:"runner_executable"`
	RunnerSHA256       string            `json:"runner_executable_sha256"`
	Status             string            `json:"status"`
	SourceSHA          string            `json:"source_sha"`
	PackagePath        string            `json:"package_path"`
	InputSHA256        string            `json:"package_input_sha256"`
	Dependencies       string            `json:"dependencies"`
	Toolchain          string            `json:"toolchain"`
	SwiftVersion       string            `json:"swift_version"`
	SDKPath            string            `json:"sdk_path"`
	SDKVersion         string            `json:"sdk_version"`
	Recipe             []string          `json:"build_recipe"`
	BuildSeconds       float64           `json:"build_seconds"`
	Executable         string            `json:"test_bundle_binary"`
	ExecutableSHA256   string            `json:"test_bundle_binary_sha256"`
	Resources          map[string]string `json:"resource_sha256"`
}

func swiftFileHash(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", errSwiftArtifact
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err = io.Copy(hash, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
func swiftInputHash(root string) (string, error) {
	hash := sha256.New()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == ".build" || rel == ".swiftpm" || rel == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errSwiftArtifact
		}
		digest, err := swiftFileHash(path)
		if err != nil {
			return err
		}
		io.WriteString(hash, filepath.ToSlash(rel)+"\x00"+digest+"\x00")
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
func (a SwiftArtifact) Validate() error {
	inputs, err := swiftInputHash(a.PackagePath)
	if err != nil || inputs != a.InputSHA256 {
		return errSwiftArtifact
	}
	binary, err := swiftFileHash(a.Executable)
	if err != nil || binary != a.ExecutableSHA256 {
		return errSwiftArtifact
	}
	if a.Runner != "" {
		got, e := swiftFileHash(a.Runner)
		if e != nil || got != a.RunnerSHA256 {
			return errSwiftArtifact
		}
	}
	for path, want := range a.RuntimeImages {
		got, e := swiftFileHash(path)
		if e != nil || got != want {
			return errSwiftArtifact
		}
	}
	for path, want := range a.Resources {
		got, err := swiftInputHash(path)
		if err != nil || got != want {
			return errSwiftArtifact
		}
	}
	return nil
}

// A manifest can select inputs outside the package or fetch dependencies. The
// capacity fixture currently supports neither; fail closed if that graph changes.
func validateSwiftGraph(raw []byte) error {
	var graph struct {
		Dependencies []json.RawMessage `json:"dependencies"`
		Targets      []struct {
			Settings  []json.RawMessage `json:"settings"`
			Plugins   []json.RawMessage `json:"pluginUsages"`
			Type      string            `json:"type"`
			Path      *string           `json:"path"`
			Sources   []string          `json:"sources"`
			Resources []struct {
				Path string `json:"path"`
			} `json:"resources"`
		} `json:"targets"`
	}
	if json.Unmarshal(raw, &graph) != nil || len(graph.Dependencies) != 0 || len(graph.Targets) == 0 {
		return errSwiftGraph
	}
	safe := func(p string) bool {
		return p != "" && !filepath.IsAbs(p) && filepath.Clean(p) == p && p != ".." && !strings.HasPrefix(p, "../")
	}
	for _, target := range graph.Targets {
		if len(target.Settings) > 0 || len(target.Plugins) > 0 {
			return errSwiftGraph
		}
		if target.Type == "binary" || target.Type == "plugin" || target.Type == "system" {
			return errSwiftGraph
		}
		if target.Path != nil && !safe(*target.Path) {
			return errSwiftGraph
		}
		for _, p := range target.Sources {
			if !safe(p) {
				return errSwiftGraph
			}
		}
		for _, p := range target.Resources {
			if !safe(p.Path) {
				return errSwiftGraph
			}
		}
	}
	return nil
}

func BuildSwiftArtifact(ctx context.Context, packagePath, sourceSHA, output string) (SwiftArtifact, error) {
	a := SwiftArtifact{Status: "unattested", SourceSHA: sourceSHA, PackagePath: packagePath, Dependencies: "package-local targets only; external dependencies refused", Resources: map[string]string{}}
	var err error
	a.InputSHA256, err = swiftInputHash(packagePath)
	if err != nil {
		return a, err
	}
	root := filepath.Join(output, "swift-build")
	if err = os.Mkdir(root, 0700); err != nil {
		return a, err
	}
	for _, name := range []string{"cache", "config", "security", "tmp"} {
		if err = os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			return a, err
		}
	}
	env := []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + os.Getenv("HOME"), "TMPDIR=" + filepath.Join(root, "tmp") + "/", "CLANG_MODULE_CACHE_PATH=" + filepath.Join(root, "cache", "clang")}
	query := func(name string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Env = env
		raw, e := cmd.Output()
		return strings.TrimSpace(string(raw)), e
	}
	a.Toolchain, err = query("/usr/bin/xcrun", "--find", "swift")
	if err != nil {
		return a, err
	}
	a.Runner = filepath.Join(filepath.Dir(filepath.Dir(a.Toolchain)), "libexec", "swift", "pm", "swiftpm-testing-helper")
	a.RunnerSHA256, err = swiftFileHash(a.Runner)
	if err != nil {
		return a, err
	}
	a.SwiftVersion, err = query(a.Toolchain, "--version")
	if err != nil {
		return a, err
	}
	a.SDKPath, err = query("/usr/bin/xcrun", "--sdk", "macosx", "--show-sdk-path")
	if err != nil {
		return a, err
	}
	a.SDKVersion, err = query("/usr/bin/xcrun", "--sdk", "macosx", "--show-sdk-version")
	if err != nil {
		return a, err
	}
	developer := filepath.Dir(filepath.Dir(a.SDKPath))
	a.RuntimeEnvironment = []string{"DYLD_FRAMEWORK_PATH=" + filepath.Join(developer, "Library", "Frameworks"), "DYLD_LIBRARY_PATH=" + filepath.Join(developer, "usr", "lib") + ":" + filepath.Join(filepath.Dir(filepath.Dir(a.Toolchain)), "lib", "swift", "macosx")}
	a.RuntimeImages = map[string]string{}
	for _, framework := range []string{"Testing", "XCTest"} {
		path, e := filepath.EvalSymlinks(filepath.Join(developer, "Library", "Frameworks", framework+".framework", framework))
		if e != nil {
			return a, e
		}
		digest, e := swiftFileHash(path)
		if e != nil {
			return a, e
		}
		a.RuntimeImages[path] = digest
	}
	common := []string{"--package-path", packagePath, "--scratch-path", filepath.Join(root, "scratch"), "--cache-path", filepath.Join(root, "cache"), "--config-path", filepath.Join(root, "config"), "--security-path", filepath.Join(root, "security"), "--manifest-cache", "local"}
	manifest, err := query(a.Toolchain, append([]string{"package"}, append(common, "dump-package")...)...)
	if err != nil {
		return a, err
	}
	if err = validateSwiftGraph([]byte(manifest)); err != nil {
		return a, err
	}
	a.Recipe = append([]string{"build"}, common...)
	a.Recipe = append(a.Recipe, "--build-tests", "--build-system", "native", "--configuration", "debug", "--sdk", a.SDKPath, "--disable-automatic-resolution", "--disable-index-store", "--jobs", "4")
	log, err := os.OpenFile(filepath.Join(output, "swift-build.private.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return a, err
	}
	defer log.Close()
	started := time.Now()
	cmd := exec.CommandContext(ctx, a.Toolchain, a.Recipe...)
	cmd.Env = env
	cmd.Stdout = log
	cmd.Stderr = log
	err = cmd.Run()
	a.BuildSeconds = time.Since(started).Seconds()
	if err != nil {
		return a, err
	}
	bin, err := query(a.Toolchain, append(a.Recipe, "--show-bin-path")...)
	if err != nil {
		return a, err
	}
	a.Executable = filepath.Join(bin, "BabyTrackerPackageTests.xctest", "Contents", "MacOS", "BabyTrackerPackageTests")
	a.ExecutableSHA256, err = swiftFileHash(a.Executable)
	if err != nil {
		return a, err
	}
	entries, err := os.ReadDir(bin)
	if err != nil {
		return a, err
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".bundle") {
			path := filepath.Join(bin, entry.Name())
			digest, e := swiftInputHash(path)
			if e != nil {
				return a, e
			}
			a.Resources[path] = digest
		}
	}
	if err = a.Validate(); err != nil {
		return a, err
	}
	a.Status = "attested_fresh_build"
	if err = writePrivateJSON(filepath.Join(output, "swift-build.json"), a); err != nil {
		return a, err
	}
	return a, nil
}

// Darwin test products are Mach-O bundles, loaded by the toolchain helper.
// Invoke it explicitly so SwiftPM cannot discover a different cached product.
func (a SwiftArtifact) testCommand(ctx context.Context, filter string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, a.Runner, "--test-bundle-path", a.Executable, "--testing-library", "swift-testing", "--filter", filter)
	cmd.Env = append([]string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + os.Getenv("HOME"), "TMPDIR=" + os.TempDir()}, a.RuntimeEnvironment...)
	return cmd
}
