package testsock_test

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/testsock"
)

func TestASocketBindsEvenUnderALongTMPDIR(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("d", 100))
	if err := os.MkdirAll(long, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", long)
	ln, err := net.Listen("unix", testsock.Path(t, "agent.sock"))
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
}

func TestTheSocketDirectoryGoesAwayWithTheTest(t *testing.T) {
	var dir string
	t.Run("inner", func(t *testing.T) { dir = filepath.Dir(testsock.Path(t, "a.sock")) })
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("%s still exists after the test: %v", dir, err)
	}
}

func TestAcceptingReturnsOnceTheSocketListens(t *testing.T) {
	path := testsock.Path(t, "a.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := testsock.Accepting(path, time.Second); err != nil {
		t.Fatalf("Accepting: %v", err)
	}
}

func TestAcceptingWaitsPastABoundButUnlistenedSocket(t *testing.T) {
	path := testsock.Path(t, "a.sock")
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
		t.Fatal(err)
	}
	if err := testsock.Accepting(path, 200*time.Millisecond); err == nil {
		t.Fatal("Accepting returned nil for a socket that only exists on disk")
	}
}
