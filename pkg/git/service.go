package git

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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

	// killed records that git was killed because the context was done. On
	// Windows a killed process exits with status 1, so the exit status cannot
	// tell a kill from a failure there. It is not used on Unix: killing a git
	// that already exited but was not yet reaped also succeeds, and there the
	// signal in the exit status tells the two apart.
	var killed atomic.Bool
	cmd.Cancel = func() error {
		err := cmd.Process.Kill()
		if err == nil {
			killed.Store(true)
		}
		return err
	}
	// Kill git as a fallback if it is still running this long after the
	// context is done, for when CmdFunc replaces Cancel.
	cmd.WaitDelay = waitDelay

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
				log.Errorf("gitServiceHandler: failed to copy stdin: %v", err)
			}
		}()
	}

	// stdout
	if scmd.Stdout != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := io.Copy(scmd.Stdout, stdout); err != nil {
				logCopyError(ctx, "stdout", err)
			}
		}()
	}

	// stderr
	if scmd.Stderr != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := io.Copy(scmd.Stderr, stderr); err != nil {
				logCopyError(ctx, "stderr", err)
			}
		}()
	}

	// Ensure all the output is written before waiting for the command to
	// finish.
	// Stdin is handled by the client side.
	copied := make(chan struct{})
	go func() {
		wg.Wait()
		close(copied)
	}()
	select {
	case <-copied:
	case <-ctx.Done():
		// git is killed for the context, and its output has nowhere to go.
		// A process git started can still hold the output pipes open after
		// git is gone, as the git wrapper on Windows does, so the copies may
		// never finish on their own. Wait closes the pipes once git exits,
		// which ends them.
	}

	err = cmd.Wait()
	<-copied
	if err != nil && stoppedByContext(ctx, err, killed.Load()) {
		// The context was cancelled, usually because the client went away,
		// and git was killed for it. Report the cancellation rather than
		// the kill so callers can tell it apart from a git failure.
		return fmt.Errorf("%s: %w (%v)", svc, ctx.Err(), err)
	} else if err != nil && errors.Is(err, os.ErrNotExist) {
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

// logCopyError logs a failure to copy git's output. Once the context is done
// the client is gone and the pipes are closed under the copy, so the failure
// is expected and logged at Debug.
func logCopyError(ctx context.Context, stream string, err error) {
	if ctx.Err() != nil {
		log.Debugf("gitServiceHandler: failed to copy %s: %v", stream, err)
		return
	}
	log.Errorf("gitServiceHandler: failed to copy %s: %v", stream, err)
}

// waitDelay is how long after the context is done git is killed as a
// fallback if Cancel did not stop it.
const waitDelay = 5 * time.Second

// stoppedByContext reports whether err, returned by Wait for a command run
// with ctx, comes from the command being stopped because ctx was done rather
// than from the command failing on its own. Wait returns the context error
// itself when the command exits successfully after it was told to stop.
//
// killed reports that the command was killed for the context, and is only
// trusted on Windows, where a killed process exits with a status. On Unix a
// command killed for the context was terminated by a signal; one that exited
// with a status, even after the context was done, failed on its own and its
// error is the one to report.
func stoppedByContext(ctx context.Context, err error, killed bool) bool {
	ctxErr := ctx.Err()
	if ctxErr == nil {
		return false
	}
	if (killed && runtime.GOOS == "windows") || errors.Is(err, ctxErr) {
		return true
	}
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && !exitErr.Exited()
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
