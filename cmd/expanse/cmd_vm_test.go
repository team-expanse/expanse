package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/blocks/vm/qga"
)

func TestVMExecSaysWhereToRunWhenTheSocketIsMissing(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "absent.sock")
	err := vmExec(context.Background(), sock, "default/vm1", []string{"true"}, nil, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "the node that runs default/vm1") {
		t.Errorf("vmExec error = %v, want a hint naming the VM's node", err)
	}
}

func TestVMExecPassesOnTheGuestsOutputAndExitCode(t *testing.T) {
	var out, errOut bytes.Buffer
	err := printResult(qga.Result{ExitCode: 3, Stdout: []byte("hi\n"), Stderr: []byte("oops")}, &out, &errOut)
	var code exitCode
	if !errors.As(err, &code) || code != 3 {
		t.Errorf("printResult error = %v, want exit code 3", err)
	}
	if out.String() != "hi\n" || errOut.String() != "oops" {
		t.Errorf("output = %q / %q", out.String(), errOut.String())
	}
	if err := printResult(qga.Result{}, &out, &errOut); err != nil {
		t.Errorf("printResult of a zero exit = %v, want nil", err)
	}
}
