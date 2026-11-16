//go:build linux || darwin

package historypriority

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// Each admission opens its own descriptions: flock on a shared description
// would be reentrant. Scheduling files own no application data.
func openGate(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("history scheduling gate: %w", err)
	}
	info, err := f.Stat()
	if err == nil {
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || !ok || owner.Uid != uint32(os.Geteuid()) || owner.Nlink != 1 {
			err = errors.New("history scheduling gate must be a private owner-only regular file")
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func unlockGate(f *os.File) error {
	return errors.Join(syscall.Flock(int(f.Fd()), syscall.LOCK_UN), f.Close())
}

// AcquirePath acquires an isolated private scheduling file for qualification.
// Services use Acquire's fixed Linux runtime path. A foreground shared
// reservation prevents a new background turn while local authentication waits.
func AcquirePath(ctx context.Context, path string) (*Turn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reservation, err := openGate(path + ".foreground")
	if err != nil {
		return nil, err
	}
	f, err := openGate(path)
	if err != nil {
		_ = reservation.Close()
		return nil, err
	}
	fail := func(err error) (*Turn, error) { _ = f.Close(); _ = reservation.Close(); return nil, err }
	for {
		if err = ctx.Err(); err != nil {
			return fail(err)
		}
		err = syscall.Flock(int(reservation.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			reservationErr := syscall.Flock(int(reservation.Fd()), syscall.LOCK_UN)
			if reservationErr != nil {
				return fail(fmt.Errorf("history scheduling reservation: %w", reservationErr))
			}
			if err == nil {
				if err = ctx.Err(); err != nil {
					return fail(err)
				}
				if err = reservation.Close(); err != nil {
					return fail(err)
				}
				return &Turn{release: func() error { return unlockGate(f) }}, nil
			}
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR) {
			return fail(fmt.Errorf("history scheduling gate: %w", err))
		}
		pause := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			pause.Stop()
			return fail(ctx.Err())
		case <-pause.C:
		}
	}
}

// AcquireForegroundPath qualifies foreground scheduling against a private test
// path. The shared reservation remains held while waiting for and owning the
// mutation turn; background workers cannot start another turn in that interval.
// Only mandatory local SQL belongs here, never network or radio operations.
func AcquireForegroundPath(path string) (*Turn, error) {
	reservation, err := openGate(path + ".foreground")
	if err != nil {
		return nil, err
	}
	f, err := openGate(path)
	if err != nil {
		_ = reservation.Close()
		return nil, err
	}
	fail := func(err error) (*Turn, error) { _ = f.Close(); _ = reservation.Close(); return nil, err }
	lock := func(f *os.File, mode int) error {
		for {
			err := syscall.Flock(int(f.Fd()), mode)
			if !errors.Is(err, syscall.EINTR) {
				return err
			}
		}
	}
	if err = lock(reservation, syscall.LOCK_SH); err != nil {
		return fail(fmt.Errorf("foreground storage reservation: %w", err))
	}
	if err = lock(f, syscall.LOCK_EX); err != nil {
		return fail(fmt.Errorf("foreground storage turn: %w", err))
	}
	return &Turn{foreground: true, release: func() error { return errors.Join(unlockGate(f), unlockGate(reservation)) }}, nil
}
