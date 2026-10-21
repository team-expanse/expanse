package qga

import (
	"context"
	"errors"
	"time"
)

// Result is a guest command's exit code, 128+signal when a signal ended it, and its output.
type Result struct {
	ExitCode       int
	Stdout, Stderr []byte
}

// pollEvery spaces the exit-status polls of a running guest command.
const pollEvery = 200 * time.Millisecond

// Exec runs argv in the guest as the agent's user (root), feeding it stdin, and waits for it to exit.
func Exec(ctx context.Context, sock string, argv []string, stdin []byte) (Result, error) {
	if len(argv) == 0 {
		return Result{}, errors.New("guest agent exec: no command")
	}
	var started struct{ PID int64 }
	err := request(ctx, sock, "guest-exec", struct {
		Path    string   `json:"path"`
		Arg     []string `json:"arg,omitempty"`
		Input   []byte   `json:"input-data,omitempty"`
		Capture bool     `json:"capture-output"`
	}{argv[0], argv[1:], stdin, true}, &started)
	if err != nil {
		return Result{}, err
	}
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	for {
		var st struct {
			Exited   bool
			ExitCode int    `json:"exitcode"`
			Signal   int    `json:"signal"`
			Out      []byte `json:"out-data"`
			Err      []byte `json:"err-data"`
		}
		if err := request(ctx, sock, "guest-exec-status", struct {
			PID int64 `json:"pid"`
		}{started.PID}, &st); err != nil {
			return Result{}, err
		}
		if st.Exited {
			code := st.ExitCode
			if st.Signal != 0 {
				code = 128 + st.Signal
			}
			return Result{ExitCode: code, Stdout: st.Out, Stderr: st.Err}, nil
		}
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-tick.C:
		}
	}
}
