package git

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"

	"charm.land/log/v2"
)

// Service is a Git daemon service.
type Service string

const (
	// UploadPackService is the upload-pack service.
	UploadPackService Service = "git-upload-pack"
	// UploadArchiveService is the upload-archive service.
	UploadArchiveService Service = "git-upload-archive"
	// ReceivePackService is the receive-pack service.
	ReceivePackService Service = "git-receive-pack"
	// LFSTransferService is the LFS transfer service.
	LFSTransferService Service = "git-lfs-transfer"
	// LFSAuthenticateService is the LFS authenticate service.
	LFSAuthenticateService = "git-lfs-authenticate"
)

// String returns the string representation of the service.
func (s Service) String() string {
	return string(s)
}

// Name returns the name of the service.
func (s Service) Name() string {
	return strings.TrimPrefix(s.String(), "git-")
}

// Handler is the service handler.
func (s Service) Handler(ctx context.Context, cmd ServiceCommand) error {
	switch s {
	case UploadPackService, UploadArchiveService, ReceivePackService:
		return gitServiceHandler(ctx, s, cmd)
	case LFSTransferService:
		return LFSTransfer(ctx, cmd)
	case LFSAuthenticateService:
		return LFSAuthenticate(ctx, cmd)
	default:
		return fmt.Errorf("unsupported service: %s", s)
	}
}

// ServiceHandler is a git service command handler.
type ServiceHandler func(ctx context.Context, cmd ServiceCommand) error

// gitServiceHandler is the default service handler using the git binary.
func gitServiceHandler(ctx context.Context, svc Service, scmd ServiceCommand) error {
	cmd := exec.CommandContext(ctx, "git")
	cmd.Dir = scmd.Dir
	cmd.Args = append(cmd.Args, []string{
		// Enable partial clones
		"-c", "uploadpack.allowFilter=true",
		// Enable push options
		"-c", "receive.advertisePushOptions=true",
		// Disable LFS filters
		"-c", "filter.lfs.required=", "-c", "filter.lfs.smudge=", "-c", "filter.lfs.clean=",
		svc.Name(),
	}...)
	if len(scmd.Args) > 0 {
		cmd.Args = append(cmd.Args, scmd.Args...)
	}

	cmd.Args = append(cmd.Args, ".")

	cmd.Env = os.Environ()
	if len(scmd.Env) > 0 {
		cmd.Env = append(cmd.Env, scmd.Env...)
	}

	if scmd.CmdFunc != nil {
		scmd.CmdFunc(cmd)
	}

	var (
		err    error
		stdin  io.WriteCloser
		stdout io.ReadCloser
		stderr io.ReadCloser
	)

	if scmd.Stdin != nil {
		stdin, err = cmd.StdinPipe()
		if err != nil {
			return err
		}
	}

	if scmd.Stdout != nil {
		stdout, err = cmd.StdoutPipe()
		if err != nil {
			return err
		}
	}

	if scmd.Stderr != nil {
		stderr, err = cmd.StderrPipe()
		if err != nil {
			return err
		}
	}

	if err := cmd.Start(); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrInvalidRepo
		}
		return err
	}

	wg := &sync.WaitGroup{}

	// stdin
	if scmd.Stdin != nil {
		go func() {
			defer stdin.Close() //nolint: errcheck
			if _, err := io.Copy(stdin, scmd.Stdin); err != nil {
				logCopyError(ctx, "stdin", err, gitGone(err))
			}
		}()
	}

	// stdout
	if scmd.Stdout != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := io.Copy(scmd.Stdout, stdout); err != nil {
				logCopyError(ctx, "stdout", err, false)
			}
		}()
	}

	// stderr
	if scmd.Stderr != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := io.Copy(scmd.Stderr, stderr); err != nil {
				logCopyError(ctx, "stderr", err, false)
			}
		}()
	}

	// Ensure all the output is written before waiting for the command to
	// finish.
	// Stdin is handled by the client side.
	wg.Wait()

	err = cmd.Wait()
	if err != nil && errors.Is(err, os.ErrNotExist) {
		return ErrInvalidRepo
	} else if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return fmt.Errorf("%s: %s", exitErr, exitErr.Stderr)
		}

		return err
	}

	return nil
}

// ServiceCommand is used to run a git service command.
type ServiceCommand struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Dir    string
	Env    []string
	Args   []string

	// Modifier functions
	CmdFunc func(*exec.Cmd)
}

// UploadPack runs the git upload-pack protocol against the provided repo.
func UploadPack(ctx context.Context, cmd ServiceCommand) error {
	return gitServiceHandler(ctx, UploadPackService, cmd)
}

// UploadArchive runs the git upload-archive protocol against the provided repo.
func UploadArchive(ctx context.Context, cmd ServiceCommand) error {
	return gitServiceHandler(ctx, UploadArchiveService, cmd)
}

// ReceivePack runs the git receive-pack protocol against the provided repo.
func ReceivePack(ctx context.Context, cmd ServiceCommand) error {
	return gitServiceHandler(ctx, ReceivePackService, cmd)
}

// logCopyError logs a failure to copy between git and the client. A client
// that goes away, closing the SSH channel or the HTTP connection, makes the
// copy fail, usually as the context is cancelled for it. That is expected,
// so it is logged at Debug, as is any failure the caller marks as expected;
// any other failure is logged at Error.
func logCopyError(ctx context.Context, stream string, err error, expected bool) {
	if expected || ctx.Err() != nil || clientGone(err) {
		log.Debugf("gitServiceHandler: failed to copy %s: %v", stream, err)
		return
	}
	log.Errorf("gitServiceHandler: failed to copy %s: %v", stream, err)
}

// gitGone reports whether err, returned by a write of the client's input to
// git, means git no longer reads it. Git can exit before the client is done
// sending, for example after a protocol error. The write then fails with a
// broken pipe, which clientGone covers, or with os.ErrClosed once the pipe
// was closed after git exited.
func gitGone(err error) bool {
	return errors.Is(err, os.ErrClosed)
}

// clientGone reports whether err, returned by a write to the client or a
// read from it, means the client closed its end. A closed SSH channel
// returns io.EOF, and a closed network connection a closed, broken pipe or
// reset error. A broken pipe also covers git's input once git exited.
func clientGone(err error) bool {
	return clientGoneOn(runtime.GOOS, err)
}

// clientGoneOn is clientGone for the operating system goos. On Windows,
// syscall.EPIPE and syscall.ECONNRESET are values Go made up, which the
// system never returns, so the Windows error codes are checked as well.
func clientGoneOn(goos string, err error) bool {
	if errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	if goos != "windows" {
		return false
	}
	for _, errno := range windowsClientGoneErrnos {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}

// Windows error codes for a peer that closed its end. They are spelled out
// as numbers so that clientGoneOn builds, and can be tested, on every
// system; syscall does not define ERROR_NO_DATA at all.
const (
	windowsErrorBrokenPipe = syscall.Errno(109)   // ERROR_BROKEN_PIPE
	windowsErrorNoData     = syscall.Errno(232)   // ERROR_NO_DATA
	windowsWSAEConnAborted = syscall.Errno(10053) // WSAECONNABORTED
	windowsWSAEConnReset   = syscall.Errno(10054) // WSAECONNRESET
)

// windowsClientGoneErrnos are the Windows error codes clientGoneOn treats
// as the peer having gone away: a reset or aborted socket connection, a
// pipe whose other end was closed (ERROR_BROKEN_PIPE) and a write to a
// pipe that is being closed (ERROR_NO_DATA).
var windowsClientGoneErrnos = []syscall.Errno{
	windowsErrorBrokenPipe,
	windowsErrorNoData,
	windowsWSAEConnAborted,
	windowsWSAEConnReset,
}
