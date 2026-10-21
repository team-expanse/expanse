package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/spf13/cobra"

	"github.com/expanse/expanse/internal/blocks/runtime/systemd"
	"github.com/expanse/expanse/internal/blocks/vm/qga"
)

// exitCode is an error that makes the CLI exit with that code and print nothing more.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// newVMCmd builds `expanse ctl vm ...`, which talks to a VM's guest agent on this node.
func newVMCmd() *cobra.Command {
	vm := &cobra.Command{Use: "vm", Short: "Commands for vm/instance blocks running on this node"}
	var ns string
	var withStdin bool
	exec := &cobra.Command{
		Use:   "exec <block> -- <command> [args...]",
		Short: "Run a command in a VM's guest as root, through its QEMU guest agent",
		Long: `exec runs a command in the guest through qemu-guest-agent, which needs no network.
Run it as root on the node that runs the VM. The command's output is printed once it exits,
and its exit code becomes this command's. Ctrl-C stops waiting; the guest command keeps running.`,
		Args: cobra.MinimumNArgs(2),
		// main logs other errors; a guest's non-zero exit is not one to print.
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			bns, name := blockRef(ns, args[0])
			var stdin []byte
			if withStdin {
				b, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return fmt.Errorf("read stdin: %w", err)
				}
				stdin = b
			}
			sock := qga.SocketPath(systemd.Instance(bns, name, 0))
			return vmExec(cmd.Context(), sock, bns+"/"+name, args[1:], stdin, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	// Flags after the block name belong to the guest command.
	exec.Flags().SetInterspersed(false)
	exec.Flags().StringVarP(&ns, "namespace", "n", "default", "namespace")
	exec.Flags().BoolVarP(&withStdin, "stdin", "i", false, "send this command's stdin to the guest command")
	vm.AddCommand(exec)
	return vm
}

func vmExec(ctx context.Context, sock, ref string, argv []string, stdin []byte, stdout, stderr io.Writer) error {
	if _, err := os.Stat(sock); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("no guest agent socket %s: run this as root on the node that runs %s "+
			"(its block page names it); a VM started by an older Expanse needs a restart first", sock, ref)
	}
	res, err := qga.Exec(ctx, sock, argv, stdin)
	if err != nil {
		return fmt.Errorf("%w (is qemu-guest-agent installed and running in the guest?)", err)
	}
	return printResult(res, stdout, stderr)
}

func printResult(res qga.Result, stdout, stderr io.Writer) error {
	_, _ = stdout.Write(res.Stdout)
	_, _ = stderr.Write(res.Stderr)
	if res.ExitCode != 0 {
		return exitCode(res.ExitCode)
	}
	return nil
}
