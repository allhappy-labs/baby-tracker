package main

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// A protected MCP secret file holds exactly the credential a desktop client
// presents: the 87 ASCII bytes btmcp1.<grant UUID>.<unpadded base64url secret>,
// with no trailing newline. It lives only in an existing, owner-only
// directory, is created exclusively with mode 0600, and is never overwritten,
// followed through a symlink, or rewritten by the admin tool.
const secretFileLimit = 128

var (
	errSecretPath = errors.New("secret_file_unsafe: secret file paths must be absolute, clean, and inside an existing directory that is not a symlink, is owned by the current user, and grants no group or other access (0700)")
	errSecretFile = errors.New("secret_file_unsafe: --secret-file must be a regular, nonsymlink file owned by the current user with mode exactly 0600")
)

// checkSecretDestination accepts only a path that does not yet exist in a
// private directory. createSecretFile repeats the decisive checks atomically.
func checkSecretDestination(path string) error {
	if err := checkPrivateParent(path); err != nil {
		return err
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		return errors.New("secret_file_exists: --secret-output must name a new file; an existing path is never overwritten")
	}
	return nil
}

func checkPrivateParent(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Dir(path) == path {
		return errSecretPath
	}
	info, err := os.Lstat(filepath.Dir(path))
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(info) {
		return errSecretPath
	}
	return nil
}

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid()
}

func protectedSecretMode(info os.FileInfo) bool {
	const special = os.ModeSetuid | os.ModeSetgid | os.ModeSticky
	return info.Mode().IsRegular() && info.Mode().Perm() == 0o600 && info.Mode()&special == 0 && ownedByCurrentUser(info)
}

// createSecretFile creates path exclusively (never through a symlink) with
// mode 0600 regardless of umask, writes content and makes it durable. Any
// failure removes the partially written file it created.
func createSecretFile(path string, content []byte) error {
	if err := checkPrivateParent(path); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return errors.New("secret_file_exists: --secret-output must name a new file; an existing path is never overwritten")
	}
	if err != nil {
		return errors.New("secret_file_unsafe: --secret-output could not be created exclusively")
	}
	complete := false
	defer func() {
		if !complete {
			_ = file.Close()
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return errors.New("secret_file_unsafe: --secret-output could not be restricted to mode 0600")
	}
	if _, err := file.Write(content); err != nil {
		return errors.New("secret_file_unsafe: --secret-output could not be written")
	}
	if err := file.Sync(); err != nil {
		return errors.New("secret_file_unsafe: --secret-output could not be synced")
	}
	opened, openErr := file.Stat()
	named, namedErr := os.Lstat(path)
	if openErr != nil || namedErr != nil || !protectedSecretMode(opened) || !os.SameFile(opened, named) {
		return errors.New("secret_file_unsafe: --secret-output changed while it was written")
	}
	if err := file.Close(); err != nil {
		return errors.New("secret_file_unsafe: --secret-output could not be closed")
	}
	complete = true
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return errors.New("secret_file_unsafe: the --secret-output directory could not be synced")
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return errors.New("secret_file_unsafe: the --secret-output directory could not be synced")
	}
	return nil
}

// readSecretFile returns the exact file contents after checking, before and
// after opening without following symlinks, that the same protected file was
// read. Contents are validated by the caller and never echoed.
func readSecretFile(path string) (string, error) {
	if err := checkPrivateParent(path); err != nil {
		return "", err
	}
	before, err := os.Lstat(path)
	if err != nil || !protectedSecretMode(before) {
		return "", errSecretFile
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", errSecretFile
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !protectedSecretMode(opened) || !os.SameFile(before, opened) {
		return "", errSecretFile
	}
	data, err := io.ReadAll(io.LimitReader(file, secretFileLimit+1))
	if err != nil || len(data) > secretFileLimit {
		return "", errors.New("secret_file_invalid: --secret-file does not contain exactly one btmcp1 credential written by mcp issue prepare")
	}
	final, err := file.Stat()
	if err != nil || !protectedSecretMode(final) || !os.SameFile(opened, final) {
		return "", errSecretFile
	}
	return string(data), nil
}
