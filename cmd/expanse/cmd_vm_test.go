package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/blocks/vm/qga"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/grpc"
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

func TestVMExecHandsTheGuestTheCommandAfterTheDashes(t *testing.T) {
	for _, args := range [][]string{
		{"exec", "-n", "apps", "vm1", "--", "ls", "-la"},
		{"exec", "-n", "apps", "vm1", "ls", "-la"},
	} {
		var gotSock string
		var gotArgv []string
		cmd := newVMCmdWith(nil, func(_ context.Context, sock, _ string, argv []string, _ []byte, _, _ io.Writer) error {
			gotSock, gotArgv = sock, argv
			return nil
		})
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%q: %v", args, err)
		}
		if gotSock != qga.SocketPath("apps-vm1-0") || strings.Join(gotArgv, " ") != "ls -la" {
			t.Errorf("%q ran %q on %s, want ls -la on apps-vm1-0's socket", args, gotArgv, gotSock)
		}
	}
}

func TestVMExecNeedsACommand(t *testing.T) {
	cmd := newVMCmdWith(nil, func(context.Context, string, string, []string, []byte, io.Writer, io.Writer) error {
		t.Error("ran with no command")
		return nil
	})
	cmd.SetArgs([]string{"exec", "vm1", "--"})
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil {
		t.Error("vm exec vm1 -- succeeded, want a missing-command error")
	}
}

func TestVMExecBlamesTheAgentOnlyWhenItDoesNotAnswer(t *testing.T) {
	refused := &qga.AgentError{Command: "guest-exec", Class: "GenericError", Desc: "Failed to execute child process"}
	if msg := execError(refused).Error(); strings.Contains(msg, "installed") {
		t.Errorf("an agent's own refusal got the install hint: %s", msg)
	}
	if msg := execError(context.DeadlineExceeded).Error(); !strings.Contains(msg, "installed") {
		t.Errorf("a silent agent got no install hint: %s", msg)
	}
}

type powerClient struct {
	pb.BlockServiceClient
	got *pb.SetPowerRequest
}

func (c *powerClient) SetPower(_ context.Context, r *pb.SetPowerRequest, _ ...grpc.CallOption) (*pb.Block, error) {
	c.got = r
	return &pb.Block{Metadata: &pb.Metadata{Namespace: r.GetNamespace(), Name: r.GetName()}}, nil
}

func TestVMSuspendAndResumeSetTheBlocksPower(t *testing.T) {
	for verb, want := range map[string]pb.Power{"suspend": pb.Power_POWER_SUSPENDED, "resume": pb.Power_POWER_ON} {
		c := &powerClient{}
		var out bytes.Buffer
		if err := setPower(context.Background(), c, "default", "vm1", verb, &out); err != nil {
			t.Fatalf("%s: %v", verb, err)
		}
		if c.got.GetPower() != want || c.got.GetName() != "vm1" || c.got.GetNamespace() != "default" {
			t.Errorf("%s sent %v", verb, c.got)
		}
		if !strings.Contains(out.String(), "default/vm1") {
			t.Errorf("%s printed %q", verb, out.String())
		}
	}
}
