package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/expanse/expanse/internal/console"
	"github.com/expanse/expanse/internal/install"
	"github.com/expanse/expanse/internal/tuikit"
)

// tuiOptions carry the install flags the TUI passes through to install.Run.
type tuiOptions struct {
	TargetFlake       string
	SkipSystemInstall bool
}

// runTUI drives the interactive installer on the controlling terminal: keys, install
// output, a one-second clock and resizes all arrive as events on one loop.
func runTUI(opts tuiOptions) error {
	term, err := tuikit.Open()
	if err != nil {
		return fmt.Errorf("put terminal in raw mode: %w", err)
	}
	defer term.Close()

	disks, err := install.DetectDisks()
	m := newTUIModel(disks)
	if err != nil {
		m.errmsg = err.Error()
	}
	logs := make(chan string, 256)
	done := make(chan installResult, 1)
	installing := false
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		term.Draw(m.View(term.Size(), term.Glyphs()))
		if m.screen == screenProgress && !installing {
			installing = true
			cfg := m.installConfig()
			go func() { done <- runTUIInstall(cfg, opts, &lineWriter{lines: logs}) }()
		}
		select {
		case k, ok := <-term.Keys():
			if !ok || m.Update(k) {
				return nil
			}
		case l := <-logs:
			m.appendLog(l)
			drainLog(m, logs)
		case r := <-done:
			m.finish(r)
		case <-ticker.C:
			m.tick(time.Now())
		case <-term.Resized():
		}
	}
}

// drainLog appends every log line already queued, so a burst repaints once.
func drainLog(m *tuiModel, logs <-chan string) {
	for {
		select {
		case l := <-logs:
			m.appendLog(l)
		default:
			return
		}
	}
}

// lineWriter splits install output into lines and sends each to the model's channel.
type lineWriter struct {
	lines chan<- string
	buf   string
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf += string(p)
	for {
		line, rest, ok := strings.Cut(w.buf, "\n")
		if !ok {
			return len(p), nil
		}
		w.buf = rest
		w.lines <- strings.TrimRight(line, "\r")
	}
}

// runTUIInstall runs the install from the model's choices, echoing all output to screen and installLogPath.
func runTUIInstall(cfg *install.Config, opts tuiOptions, screen io.Writer) installResult {
	logFile, err := os.Create(installLogPath)
	if err != nil {
		return installResult{err: err}
	}
	defer func() { _ = logFile.Close() }()
	out := io.MultiWriter(screen, logFile)

	cfgFile, err := os.CreateTemp("", "expanse-install-*.yaml")
	if err != nil {
		return installResult{err: err}
	}
	defer func() { _ = os.Remove(cfgFile.Name()) }()
	if err := cfg.WriteYAML(cfgFile.Name()); err != nil {
		return installResult{err: err}
	}
	err = install.Run(install.Options{
		ConfigPath:        cfgFile.Name(),
		Force:             true,
		TargetFlake:       opts.TargetFlake,
		SkipSystemInstall: opts.SkipSystemInstall,
		Out:               out,
		Logger:            func(f string, a ...any) { _, _ = fmt.Fprintf(out, f+"\n", a...) },
	})
	if err != nil {
		return installResult{err: err}
	}
	res := installResult{ips: console.LocalIPs()}
	if id, err := install.LoadIdentity("/mnt/persist/expanse/identity"); err == nil {
		res.nodeID = id.NodeID.String() // else the done screen says the ID is unreadable
	}
	return res
}
