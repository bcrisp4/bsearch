//go:build unix

package socket_test

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bcrisp4/bsearch/internal/socket"
)

// shortDir returns a temp directory with a very short path. t.TempDir() is
// unusable for sockets: $TMPDIR alone is ~49 bytes on macOS and the test name
// is appended, which overruns sockaddr_un's 104-byte sun_path field and fails
// bind() with a bare EINVAL.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bs") //nolint:usetesting // t.TempDir() is too long for sun_path — see above
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("RemoveAll: %v", err)
		}
	})
	return dir
}

// paths returns socket and lock paths inside a fresh short temp directory,
// under a subdirectory the listener has to create itself.
func paths(t *testing.T) (socketPath, lockPath string) {
	t.Helper()
	dir := filepath.Join(shortDir(t), "d")
	return filepath.Join(dir, "s.sock"), filepath.Join(dir, "s.lock")
}

func TestListenServesConnections(t *testing.T) {
	socketPath, lockPath := paths(t)

	ln, err := socket.Listen(socketPath, lockPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close() //nolint:errcheck // closed again by the test below

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = conn.Write([]byte("hello"))
		_ = conn.Close()
	}()

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close() //nolint:errcheck // test client
	buf := make([]byte, 5)
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(buf) != "hello" {
		t.Errorf("read %q, want %q", buf, "hello")
	}
}

func TestListenPermissions(t *testing.T) {
	socketPath, lockPath := paths(t)

	ln, err := socket.Listen(socketPath, lockPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close() //nolint:errcheck // best effort

	// 0600 on the socket is the whole access-control story in v1: no auth,
	// same-user only (DESIGN.md Security, threat 2).
	fi, err := os.Lstat(socketPath)
	if err != nil {
		t.Fatalf("Lstat socket: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("socket mode = %o, want 600", got)
	}
	// The parent directory backs it up: connect() needs the execute bit on
	// every component, which closes the window between bind() (umask-derived
	// mode) and the chmod above.
	di, err := os.Stat(filepath.Dir(socketPath))
	if err != nil {
		t.Fatalf("Stat dir: %v", err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("socket directory mode = %o, want 700", got)
	}
}

func TestListenLeavesAnExistingDirectoryAlone(t *testing.T) {
	// The socket path is the user's choice, and its parent may be somewhere
	// that is emphatically not ours to re-permission: --socket ~/bsearch.sock
	// would otherwise chmod $HOME to 0700, and a relative path would chmod
	// the working directory. Only directories this call creates are tightened.
	dir := shortDir(t)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	ln, err := socket.Listen(filepath.Join(dir, "s.sock"), filepath.Join(dir, "s.lock"))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close() //nolint:errcheck // best effort

	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o755 {
		t.Errorf("directory mode = %o, want it untouched at 755", got)
	}
}

func TestListenTightensDirectoriesItCreates(t *testing.T) {
	// The one it does own: a directory brought into existence for the socket
	// starts at 0700, narrowing the window between bind() (umask-derived
	// mode) and the socket's own chmod.
	socketPath, lockPath := paths(t) // parent does not exist yet

	ln, err := socket.Listen(socketPath, lockPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close() //nolint:errcheck // best effort

	fi, err := os.Stat(filepath.Dir(socketPath))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Errorf("created directory mode = %o, want 700", got)
	}
}

func TestCloseUnlinksSocket(t *testing.T) {
	socketPath, lockPath := paths(t)

	ln, err := socket.Listen(socketPath, lockPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Lstat(socketPath); !os.IsNotExist(err) {
		t.Errorf("socket still present after Close (stat err %v)", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	socketPath, lockPath := paths(t)

	ln, err := socket.Listen(socketPath, lockPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	// http.Server.Shutdown closes the listener, and the daemon's own defer
	// closes it again — the second must not error or unlink a successor's
	// socket.
	if err := ln.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Errorf("second Close: %v, want nil", err)
	}
}

func TestListenRefusesSecondInstance(t *testing.T) {
	socketPath, lockPath := paths(t)

	first, err := socket.Listen(socketPath, lockPath)
	if err != nil {
		t.Fatalf("first Listen: %v", err)
	}
	defer first.Close() //nolint:errcheck // best effort

	second, err := socket.Listen(socketPath, lockPath)
	if err == nil {
		_ = second.Close()
		t.Fatal("second Listen succeeded; want ErrAlreadyRunning")
	}
	if !errors.Is(err, socket.ErrAlreadyRunning) {
		t.Errorf("second Listen error = %v, want ErrAlreadyRunning", err)
	}
	// The loser must not have disturbed the winner.
	if _, err := net.Dial("unix", socketPath); err != nil {
		t.Errorf("first listener no longer reachable after a rejected second: %v", err)
	}
}

func TestListenAfterCloseSucceeds(t *testing.T) {
	socketPath, lockPath := paths(t)

	first, err := socket.Listen(socketPath, lockPath)
	if err != nil {
		t.Fatalf("first Listen: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := socket.Listen(socketPath, lockPath)
	if err != nil {
		t.Fatalf("second Listen after Close: %v, want success", err)
	}
	if err := second.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestListenReclaimsStaleSocket(t *testing.T) {
	socketPath, lockPath := paths(t)

	// A daemon killed with SIGKILL leaves the socket file behind: bind()
	// then fails with EADDRINUSE even though nothing is listening.
	// SetUnlinkOnClose(false) reproduces that leftover exactly.
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("test setup listen: %v", err)
	}
	raw.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := raw.Close(); err != nil {
		t.Fatalf("test setup close: %v", err)
	}
	if _, err := os.Lstat(socketPath); err != nil {
		t.Fatalf("test setup: expected a leftover socket file, got %v", err)
	}

	ln, err := socket.Listen(socketPath, lockPath)
	if err != nil {
		t.Fatalf("Listen over a stale socket: %v, want it reclaimed", err)
	}
	defer ln.Close() //nolint:errcheck // best effort
	if _, err := net.Dial("unix", socketPath); err != nil {
		t.Errorf("reclaimed socket not reachable: %v", err)
	}
}

func TestListenRefusesToRemoveNonSocket(t *testing.T) {
	socketPath, lockPath := paths(t)
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	// bind() reports EADDRINUSE for a regular file too, and connecting to it
	// is refused — so a naive "nothing answered, delete it" would destroy a
	// user's file.
	if err := os.WriteFile(socketPath, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}

	ln, err := socket.Listen(socketPath, lockPath)
	if err == nil {
		_ = ln.Close()
		t.Fatal("Listen over a regular file succeeded; want an error")
	}
	data, readErr := os.ReadFile(socketPath)
	if readErr != nil {
		t.Fatalf("the file was destroyed: %v", readErr)
	}
	if string(data) != "precious" {
		t.Errorf("file contents = %q, want them untouched", data)
	}
}

func TestListenRejectsOverlongPath(t *testing.T) {
	dir := shortDir(t)
	long := filepath.Join(dir, strings.Repeat("x", 120)+".sock")

	_, err := socket.Listen(long, filepath.Join(dir, "s.lock"))
	if err == nil {
		t.Fatal("Listen(over-long path) = nil error, want a length error")
	}
	// bind() would fail with a bare "invalid argument"; the message has to
	// name the real problem and the flag that fixes it.
	if !strings.Contains(err.Error(), "--socket") {
		t.Errorf("error %q does not name the --socket flag", err)
	}
	if !strings.Contains(err.Error(), "103") {
		t.Errorf("error %q does not give the length limit", err)
	}
}

func TestListenReportsLockDirectoryFailure(t *testing.T) {
	// A lock path whose parent cannot be created must fail loudly rather
	// than silently skipping the single-instance guarantee.
	dir := shortDir(t)
	blocker := filepath.Join(dir, "f")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := socket.Listen(filepath.Join(dir, "s.sock"), filepath.Join(blocker, "s.lock"))
	if err == nil {
		t.Fatal("Listen with an uncreatable lock directory = nil error, want error")
	}
}

func TestListenAcceptsADirectoryWeDoNotOwn(t *testing.T) {
	// /tmp is root-owned and world-writable, and putting the socket there
	// is legitimate — sometimes the only thing that fits sun_path. The
	// directory cannot be tightened, and that must not stop the daemon:
	// the socket's own 0600 is the access control.
	socketPath := filepath.Join("/tmp", fmt.Sprintf("bs-%d.sock", os.Getpid()))
	lockPath := socketPath + ".lock"
	t.Cleanup(func() {
		_ = os.Remove(socketPath)
		_ = os.Remove(lockPath)
	})

	ln, err := socket.Listen(socketPath, lockPath)
	if err != nil {
		t.Fatalf("Listen under /tmp: %v", err)
	}
	defer ln.Close() //nolint:errcheck // best effort

	fi, err := os.Lstat(socketPath)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("socket mode = %o, want 600", got)
	}
}

func TestListenRefusesAFileWhereADirectoryBelongs(t *testing.T) {
	// A regular file in the socket path's place makes every later call fail
	// with "not a directory" from somewhere further in; naming it here means
	// the error points at the path the user actually got wrong.
	dir := shortDir(t)
	blocker := filepath.Join(dir, "f")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := socket.Listen(filepath.Join(blocker, "s.sock"), filepath.Join(dir, "s.lock"))
	if err == nil {
		t.Fatal("Listen under a regular file = nil error, want an error")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("error %q does not say the path is not a directory", err)
	}
	if !strings.Contains(err.Error(), blocker) {
		t.Errorf("error %q does not name the offending path", err)
	}
}
