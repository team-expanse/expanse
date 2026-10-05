package testsock_test

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
