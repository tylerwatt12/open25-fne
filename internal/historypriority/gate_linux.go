//go:build linux

package historypriority

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// The installation's controllers share one user and one host runtime directory.
// Keep the lock on internal runtime storage; never accept an environment-selected
// external data root or silently proceed without the host scheduling gate.
func Acquire(ctx context.Context) (*Turn, error) {
	return acquireRuntime(ctx, filepath.Join("/run/user", strconv.Itoa(os.Geteuid())))
}

// AcquireForeground reserves the next available storage turn for mandatory local
// authentication SQL. It does not lower the caller's thread priority.
func AcquireForeground() (*Turn, error) {
	path, err := runtimeGatePath(filepath.Join("/run/user", strconv.Itoa(os.Geteuid())))
	if err != nil {
		return nil, err
	}
	return AcquireForegroundPath(path)
}

// The host runtime parent may grant group access. Our one scheduling namespace
// is a separate private directory, created once without repairing existing paths.
func acquireRuntime(ctx context.Context, directory string) (*Turn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := runtimeGatePath(directory)
	if err != nil {
		return nil, err
	}
	return AcquirePath(ctx, path)
}

func runtimeGatePath(directory string) (string, error) {
	info, err := os.Lstat(directory)
	if err != nil {
		return "", err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm()&0002 != 0 || !ok || owner.Uid != uint32(os.Geteuid()) {
		return "", errors.New("history scheduling requires an owned real runtime parent that is not world writable")
	}
	private := filepath.Join(directory, "open25-fne-history")
	if err = os.Mkdir(private, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err = os.Lstat(private)
	if err != nil {
		return "", err
	}
	owner, ok = info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || !ok || owner.Uid != uint32(os.Geteuid()) {
		return "", errors.New("history scheduling requires a private owned FNE runtime directory")
	}
	return filepath.Join(private, "mutation.lock"), nil
}
