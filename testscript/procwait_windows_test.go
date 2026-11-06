package testscript

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

// waitExit waits until p has exited, without reaping it, so that testscript
// can still collect its exit status.
func waitExit(p *os.Process, timeout time.Duration) error {
	h, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(p.Pid)) //nolint:gosec
	if err != nil {
		return nil // the process has already exited and been released
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
