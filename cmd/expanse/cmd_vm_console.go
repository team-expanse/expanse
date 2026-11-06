package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	pb "github.com/expanse/expanse/proto"
)

// escapeKey (Ctrl-]) leaves an interactive console, as in telnet.
const escapeKey = 0x1d

// pipedLinger is how long a console fed from a pipe keeps printing output after its input ends.
const pipedLinger = 2 * time.Second

// newVMConsoleCmd builds `vm console`.
func newVMConsoleCmd(opts *ctlOpts) *cobra.Command {
	var ns string
	var replica int32
	c := &cobra.Command{
		Use:   "console <[namespace/]name>",
		Short: "Attach to a VM's serial console, from any node",
		Long: `console attaches this terminal to the VM's serial console (the guest's ttyS0) on whichever node
runs it. It first shows the console's recent output. Press Ctrl-] to leave; the guest keeps running.
Input from a pipe is sent as it is, and the console closes shortly after the input ends.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			bns, name := blockRef(ns, args[0])
			conn, err := dial(opts)
			if err != nil {
				return err
			}
			defer conn.Close()
			first := &pb.ConsoleInput{Namespace: bns, Name: name, Replica: replica}
			fd := int(os.Stdin.Fd())
			if !term.IsTerminal(fd) {
				return attachConsole(cmd.Context(), pb.NewConsoleServiceClient(conn), first, os.Stdin, cmd.OutOrStdout(), false)
			}
			old, err := term.MakeRaw(fd)
			if err != nil {
				return fmt.Errorf("console: raw terminal: %w", err)
			}
			defer func() { _ = term.Restore(fd, old) }()
			fmt.Fprintf(cmd.ErrOrStderr(), "Connected to %s/%s's console. Press Ctrl-] to leave.\r\n", bns, name)
			err = attachConsole(cmd.Context(), pb.NewConsoleServiceClient(conn), first, os.Stdin, cmd.OutOrStdout(), true)
			fmt.Fprint(cmd.ErrOrStderr(), "\r\nConsole closed.\r\n")
			return err
		},
	}
	c.Flags().StringVarP(&ns, "namespace", "n", "default", "namespace")
	c.Flags().Int32Var(&replica, "replica", 0, "replica index")
	return c
}

// attachConsole relays in to the console and its output to out until the console ends, in ends
// (after pipedLinger), or, with escape set, in sends Ctrl-].
func attachConsole(ctx context.Context, client pb.ConsoleServiceClient, first *pb.ConsoleInput, in io.Reader, out io.Writer, escape bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := client.Attach(ctx)
	if err != nil {
		return fmt.Errorf("console: %w", err)
	}
	if err := stream.Send(first); err != nil {
		return fmt.Errorf("console: %w", err)
	}
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		buf := make([]byte, 1024)
		for {
			n, err := in.Read(buf)
			p := buf[:n]
			if i := bytes.IndexByte(p, escapeKey); escape && i >= 0 {
				_ = stream.Send(&pb.ConsoleInput{Data: append([]byte(nil), p[:i]...)})
				cancel()
				return
			}
			if n > 0 {
				if serr := stream.Send(&pb.ConsoleInput{Data: append([]byte(nil), p...)}); serr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	outputDone := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				outputDone <- err
				return
			}
			if _, err := out.Write(msg.GetData()); err != nil {
				outputDone <- err
				return
			}
		}
	}()
	select {
	case err = <-outputDone:
	case <-inputDone:
		err = nil
		if ctx.Err() == nil { // input ended rather than the escape key: let the output finish
			select {
			case err = <-outputDone:
			case <-time.After(pipedLinger):
			}
		}
	}
	if err == nil || errors.Is(err, io.EOF) || ctx.Err() != nil {
		return nil
	}
	return fmt.Errorf("console: %w", err)
}
