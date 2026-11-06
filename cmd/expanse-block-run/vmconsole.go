package main

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/expanse/expanse/internal/blocks/vm/console"
)

// journalLineIdle is how long a guest's unfinished console line waits before the journal gets it.
const journalLineIdle = 200 * time.Millisecond

// serialPipes carry the guest's serial console (qemu's -serial stdio) to and from this process.
type serialPipes struct {
	in      *os.File // our end of the guest's input
	out     *os.File // our end of the guest's output
	qemuIn  *os.File
	qemuOut *os.File
}

// newSerialPipes points cmd's stdin and stdout at pipes this process keeps the other ends of.
func newSerialPipes(cmd *exec.Cmd) (*serialPipes, error) {
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("serial input pipe: %w", err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		_ = inR.Close()
		_ = inW.Close()
		return nil, fmt.Errorf("serial output pipe: %w", err)
	}
	cmd.Stdin, cmd.Stdout = inR, outW
	return &serialPipes{in: inW, out: outR, qemuIn: inR, qemuOut: outW}, nil
}

func (p *serialPipes) close() {
	for _, f := range []*os.File{p.in, p.out, p.qemuIn, p.qemuOut} {
		_ = f.Close()
	}
}

// serveConsole copies the guest's output to the journal and serves its console socket; the
// returned stop closes both. A socket that cannot be opened leaves the journal copy running.
func serveConsole(instance string, p *serialPipes) (stop func()) {
	// qemu holds its own copies now; ours would keep the output pipe from ever reaching EOF.
	_ = p.qemuIn.Close()
	_ = p.qemuOut.Close()
	journal := console.NewLineWriter(os.Stdout, journalLineIdle)
	mux := console.NewMux(p.in, journal)
	go func() {
		_ = mux.Pump(p.out)
		journal.Flush()
	}()
	path := console.SocketPath(instance)
	ln, err := console.Listen(path)
	if err != nil {
		fmt.Printf("expanse-block-run: vm %s has no console socket: %v\n", instance, err)
		return func() { _ = p.in.Close() }
	}
	go func() { _ = mux.Serve(ln) }()
	return func() {
		_ = ln.Close()
		_ = os.Remove(path)
		_ = p.in.Close()
	}
}
