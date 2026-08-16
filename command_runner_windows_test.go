//go:build windows

package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unxed/f4/vfs"
)

func TestLocalCommandRunnerWindowsStreamsMergedLinesAndExitStatus(t *testing.T) {
	t.Setenv("COMSPEC", "cmd.exe")
	dir := t.TempDir()
	var got []string
	code, err := NewLocalCommandRunner().RunCommand(
		context.Background(),
		dir,
		`cd & echo out-line & echo err-line 1>&2 & <nul set /p =tail & exit /b 7`,
		func(line string) { got = append(got, line) },
	)
	if err != nil {
		t.Fatalf("RunCommand: %v", err)
	}
	if code != 7 {
		t.Fatalf("exit code = %d, want 7", code)
	}

	// stdout and stderr are separate pipes, so their lines may be delivered
	// in either order; cmd also pads echo and set /p output with trailing
	// spaces. Assert that each stream's line and the unterminated tail line
	// were all delivered, rather than a specific cross-stream ordering.
	var sawDir, sawOut, sawErr, sawTail bool
	for _, line := range got {
		line = strings.TrimSpace(line)
		switch {
		case strings.EqualFold(line, dir):
			sawDir = true
		case line == "out-line":
			sawOut = true
		case line == "err-line":
			sawErr = true
		case line == "tail":
			sawTail = true
		}
	}
	if !sawDir || !sawOut || !sawErr || !sawTail {
		t.Fatalf("lines = %#v, want dir, out-line (stdout), err-line (stderr) and tail present", got)
	}

	info := NewLocalCommandRunner().CommandRunnerInfo()
	if info.Dialect != vfs.CommandDialectCmd || info.MaxParallel != 0 {
		t.Fatalf("runner info = %+v", info)
	}
}

func TestLocalCommandRunnerWindowsCancellationKillsProcessTree(t *testing.T) {
	t.Setenv("COMSPEC", "cmd.exe")
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	var once sync.Once
	done := make(chan error, 1)
	go func() {
		_, err := NewLocalCommandRunner().RunCommand(ctx, "", `ping.exe -n 30 127.0.0.1`, func(string) {
			once.Do(func() { close(started) })
		})
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		t.Fatal("ping child did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunCommand error = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunCommand did not return after process-tree cancellation")
	}
}
