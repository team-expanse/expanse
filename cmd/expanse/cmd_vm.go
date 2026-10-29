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
	pb "github.com/expanse/expanse/proto"
)

// exitCode is an error that makes the CLI exit with that code and print nothing more.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

type execFunc func(ctx context.Context, sock, ref string, argv []string, stdin []byte, stdout, stderr io.Writer) error

// newVMCmd builds `expanse ctl vm ...`: suspend and resume, and exec through a VM's guest agent on this node.
func newVMCmd(opts *ctlOpts) *cobra.Command { return newVMCmdWith(opts, vmExec) }

func newVMCmdWith(opts *ctlOpts, run execFunc) *cobra.Command {
	vm := &cobra.Command{Use: "vm", Short: "Commands for vm/instance blocks"}
	for _, verb := range []string{"suspend", "resume"} {
		vm.AddCommand(newPowerCmd(opts, verb))
	}
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
			// Without interspersed flags, cobra leaves the "--" in args.
			argv := args[1:]
			if argv[0] == "--" {
				argv = argv[1:]
			}
			if len(argv) == 0 {
				return errors.New("vm exec: no command after the block name")
			}
			var stdin []byte
			if withStdin {
				b, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return fmt.Errorf("read stdin: %w", err)
				}
				stdin = b
			}
			sock := qga.SocketPath(systemd.Instance(bns, name, 0))
			return run(cmd.Context(), sock, bns+"/"+name, argv, stdin, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	// Flags after the block name belong to the guest command.
	exec.Flags().SetInterspersed(false)
	exec.Flags().StringVarP(&ns, "namespace", "n", "default", "namespace")
	exec.Flags().BoolVarP(&withStdin, "stdin", "i", false, "send this command's stdin to the guest command")
	vm.AddCommand(exec)
	return vm
}

// newPowerCmd builds `vm suspend` or `vm resume`.
func newPowerCmd(opts *ctlOpts, verb string) *cobra.Command {
	short := "Save a VM's running state to its node's disk and stop it"
	if verb == "resume" {
		short = "Start a suspended VM from its saved state (a VM moved since boots fresh)"
	}
	var ns string
	c := &cobra.Command{
		Use:   verb + " <[namespace/]name>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			bns, name := blockRef(ns, args[0])
			return withBlockClient(cmd, opts, func(ctx context.Context, c pb.BlockServiceClient) error {
				return setPower(ctx, c, bns, name, verb, cmd.OutOrStdout())
			})
		},
	}
	c.Flags().StringVarP(&ns, "namespace", "n", "default", "namespace")
	return c
}

// setPower asks for a block to be suspended or resumed; its node does the work shortly after.
func setPower(ctx context.Context, c pb.BlockServiceClient, ns, name, verb string, out io.Writer) error {
	p := pb.Power_POWER_SUSPENDED
	if verb == "resume" {
		p = pb.Power_POWER_ON
	}
	b, err := c.SetPower(ctx, &pb.SetPowerRequest{Namespace: ns, Name: name, Power: p})
	if err != nil {
		return fmt.Errorf("%s: %w", verb, err)
	}
	ref := b.GetMetadata().GetNamespace() + "/" + b.GetMetadata().GetName()
	if p == pb.Power_POWER_SUSPENDED {
		fmt.Fprintf(out, "suspending %s; its phase reads SUSPENDED once its node has saved it\n", ref)
	} else {
		fmt.Fprintf(out, "resuming %s\n", ref)
	}
	return nil
}

func vmExec(ctx context.Context, sock, ref string, argv []string, stdin []byte, stdout, stderr io.Writer) error {
	if _, err := os.Stat(sock); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("no guest agent socket %s: run this as root on the node that runs %s "+
			"(its block page names it); a VM started by an older Expanse needs a restart first", sock, ref)
	}
	res, err := qga.Exec(ctx, sock, argv, stdin)
	if err != nil {
		return execError(err)
	}
	return printResult(res, stdout, stderr)
}

// execError adds the install hint only when the agent did not answer.
func execError(err error) error {
	var refused *qga.AgentError
	if errors.As(err, &refused) {
		return err
	}
	return fmt.Errorf("%w (is qemu-guest-agent installed and running in the guest?)", err)
}

func printResult(res qga.Result, stdout, stderr io.Writer) error {
	_, _ = stdout.Write(res.Stdout)
	_, _ = stderr.Write(res.Stderr)
	if res.ExitCode != 0 {
		return exitCode(res.ExitCode)
	}
	return nil
}
