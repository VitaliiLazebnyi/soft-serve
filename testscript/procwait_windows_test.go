package testscript

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// errorInvalidParameter is ERROR_INVALID_PARAMETER, which OpenProcess
// returns for a process ID that no process has.
const errorInvalidParameter = syscall.Errno(87)

// waitExit waits until p has exited, without reaping it, so that testscript
// can still collect its exit status.
func waitExit(p *os.Process, timeout time.Duration) error {
	h, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(p.Pid)) //nolint:gosec
	if errors.Is(err, errorInvalidParameter) {
		return nil // no such process: it has already exited and been released
	}
	if err != nil {
		// For example access denied: the process may still be running.
		return fmt.Errorf("open process %d: %w", p.Pid, err)
	}
	defer syscall.CloseHandle(h) //nolint:errcheck
	ev, err := syscall.WaitForSingleObject(h, uint32(timeout.Milliseconds()))
	switch ev {
	case syscall.WAIT_OBJECT_0:
		return nil
	case syscall.WAIT_TIMEOUT:
		return fmt.Errorf("process %d did not exit within %v", p.Pid, timeout)
	default:
		return fmt.Errorf("wait for process %d: %v", p.Pid, err)
	}
}
