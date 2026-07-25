//go:build unix

// Package socket creates the daemon's unix listener: single-instance
// guarantee, stale-socket recovery, and the 0700-directory/0600-socket
// permission discipline that is v1's entire access-control story (ADR 0009).
//
// The daemon is handed the resulting net.Listener and knows nothing about how
// it was made — that indirection is what lets a TCP listener (with an auth
// story) be added later without touching the server.
package socket

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// MaxPathLen is the longest socket path the kernel accepts. A unix address is
// a struct with a fixed-size filename field (sockaddr_un.sun_path, 104 bytes
// on macOS) plus a NUL terminator, so an over-long path doesn't merely fail —
// bind() reports a bare EINVAL that reads like a bug in the daemon.
const MaxPathLen = 103

// ErrAlreadyRunning means another bsearch daemon holds the lock. It is a
// normal outcome (launchd may double-start, a user may run serve twice), not
// a fault, so callers can report it plainly instead of as a crash.
var ErrAlreadyRunning = errors.New("another bsearch daemon is already running")

// Listener is the daemon's accepting socket plus the lock file that proves it
// is the only daemon. Closing it releases both.
type Listener struct {
	net.Listener
	lock      *os.File
	closeOnce sync.Once
	closeErr  error
}

// Listen binds a unix socket at socketPath, guarded by a lock file at
// lockPath. It fails with ErrAlreadyRunning when another daemon holds the
// lock; only after taking the lock does it clear a stale socket, so two
// daemons racing at startup can never both decide the other's socket is
// stale.
func Listen(socketPath, lockPath string) (*Listener, error) {
	if n := len(socketPath); n > MaxPathLen {
		return nil, fmt.Errorf("socket path is %d bytes but the unix limit is %d — pass a shorter --socket: %s",
			n, MaxPathLen, socketPath)
	}
	if err := ensureDir(filepath.Dir(socketPath)); err != nil {
		return nil, fmt.Errorf("create socket directory: %w", err)
	}
	if err := ensureDir(filepath.Dir(lockPath)); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}

	lock, err := acquireLock(lockPath)
	if err != nil {
		return nil, err
	}
	// From here the lock is ours: on any failure it must be released, or a
	// restart would report a daemon that isn't running.
	if err := clearStaleSocket(socketPath); err != nil {
		_ = lock.Close()
		return nil, err
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("listen on %s: %w", socketPath, err)
	}
	// net.Listen creates the node at 0777 &^ umask. The window before this
	// chmod is covered by the 0700 parent — connect() needs the execute bit
	// on every path component (ADR 0009, accepted risk).
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = ln.Close()
		_ = lock.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}
	return &Listener{Listener: ln, lock: lock}, nil
}

// Close stops accepting and releases the single-instance lock. It is
// idempotent because http.Server.Shutdown closes the listener too, and a
// second unlink could delete a *successor* daemon's socket — Go's
// UnixListener already unlinks on its first Close, and this package never
// unlinks by hand.
func (l *Listener) Close() error {
	l.closeOnce.Do(func() {
		// Order matters: stop serving before giving up the right to be the
		// daemon, or a replacement could bind while this one still accepts.
		lnErr := l.Listener.Close()
		// Closing the file descriptor releases the flock; the lock file
		// itself stays on disk, since removing it would race a successor
		// that has already opened it.
		lockErr := l.lock.Close()
		l.closeErr = errors.Join(lnErr, lockErr)
	})
	return l.closeErr
}

// ensureDir creates dir and tightens it to 0700 when it is ours. MkdirAll is
// a no-op on an existing directory, so the explicit chmod is what actually
// guarantees the mode (the same reason sqlite.Open does it for the data
// directory).
//
// A directory we don't own is left alone rather than treated as an error.
// Putting the socket somewhere shared like /tmp is legitimate — sometimes the
// only choice that fits sun_path — and chmod'ing /tmp would fail here and be
// worse if it succeeded. Nothing is being skipped silently: the socket's own
// 0600 is the access control, and the directory mode only narrows the
// sub-millisecond window between bind() and that chmod.
func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	owned, err := ownedByUs(dir)
	if err != nil {
		return err
	}
	if !owned {
		return nil
	}
	return os.Chmod(dir, 0o700) // #nosec G302 -- directory: owner needs the execute bit
}

// ownedByUs reports whether path belongs to the effective user.
func ownedByUs(path string) (bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		// Unknown FileInfo backing: assume not ours and leave it be.
		return false, nil
	}
	return int(st.Uid) == os.Geteuid(), nil
}

// acquireLock takes an exclusive, non-blocking flock. The returned file must
// stay open for the daemon's lifetime: closing it — including having it
// garbage-collected — releases the lock. The kernel releases it on process
// death, including SIGKILL, so no stale-lock cleanup is ever needed.
func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- caller-chosen lock path
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w (lock held on %s)", ErrAlreadyRunning, path)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return f, nil
}

// clearStaleSocket removes a socket left behind by a daemon that died without
// closing it. Only a socket is removed: bind() reports EADDRINUSE for a
// regular file at the path too, and a "nothing answered, delete it" rule
// would destroy a user's file. Safe to do unconditionally because the caller
// already holds the lock, so no live daemon owns this path.
func clearStaleSocket(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect socket path: %w", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket — refusing to remove it; move it aside or pass a different --socket", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale socket: %w", err)
	}
	return nil
}
