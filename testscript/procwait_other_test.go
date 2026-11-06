//go:build !windows

package testscript

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// waitExit waits until p has exited, without reaping it, so that testscript
// can still collect its exit status. testscript reaps it as soon as it exits,
// after which signalling it reports os.ErrProcessDone.
func waitExit(p *os.Process, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := p.Signal(syscall.Signal(0)); errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("process %d did not exit within %v", p.Pid, timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
