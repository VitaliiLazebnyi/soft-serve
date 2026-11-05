package cmd

import (
	"fmt"

	"charm.land/log/v2"
	gitm "github.com/aymanbagabas/git-module"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/spf13/cobra"
)

func branchCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "branch",
		Short: "Manage repository branches",
	}

	cmd.AddCommand(
		branchListCommand(),
		branchDefaultCommand(),
		branchDeleteCommand(),
	)

	return cmd
}

func branchListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:               "list REPOSITORY",
		Short:             "List repository branches",
		Args:              cobra.ExactArgs(1),
		PersistentPreRunE: requireRepo(access.ReadOnlyAccess),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			rr, _, err := repoFromContext(ctx)
			if err != nil {
				return err
			}

			r, err := rr.Open()
			if err != nil {
				return err
			}

			branches, _ := r.Branches()
			for _, b := range branches {
				cmd.Println(b)
			}

			return nil
		},
	}

	return cmd
}

func branchDefaultCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:               "default REPOSITORY [BRANCH]",
		Short:             "Set or get the default branch",
		Args:              cobra.RangeArgs(1, 2),
		PersistentPreRunE: requireRepo(access.ReadOnlyAccess),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			rr, _, err := repoFromContext(ctx)
			if err != nil {
				return err
			}

			r, err := rr.Open()
			if err != nil {
				return err
			}

			switch len(args) {
			case 1:
				head, err := r.HEAD()
				if err != nil {
					return err
				}

				cmd.Println(head.Name().Short())
			case 2:
				if err := requireLevel(ctx, access.ReadWriteAccess); err != nil {
					return err
				}

				branch := args[1]
				branches, _ := r.Branches()
				var exists bool
				for _, b := range branches {
					if branch == b {
						exists = true
						break
					}
				}

				if !exists {
					return git.ErrReferenceNotExist
				}

				if _, err := r.SymbolicRef(git.HEAD, gitm.RefsHeads+branch, gitm.SymbolicRefOptions{
					CommandOptions: gitm.CommandOptions{
						Context: ctx,
					},
				}); err != nil {
					return err
				}

				// TODO: move this to backend?
				user := proto.UserFromContext(ctx)
				// The default branch is already changed, so a failed event is
				// logged rather than reported as a failed command.
				wh, err := webhook.NewRepositoryEvent(ctx, user, rr, webhook.RepositoryEventActionDefaultBranchChange)
				if err != nil {
					log.Error("default branch changed but webhook event skipped", "err", err)
					return nil
				}

				// Sent in the background like the backend's own events, so the
				// session neither waits on receivers nor cancels the delivery.
				backend.FromContext(ctx).SendEventAfterChange(ctx, wh)
				return nil
			}

			return nil
		},
	}

	return cmd
}

func branchDeleteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:               "delete REPOSITORY BRANCH",
		Aliases:           []string{"remove", "rm", "del"},
		Short:             "Delete a branch",
		Args:              cobra.ExactArgs(2),
		PersistentPreRunE: requireRepo(access.ReadWriteAccess),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			rr, _, err := repoFromContext(ctx)
			if err != nil {
				return err
			}

			r, err := rr.Open()
			if err != nil {
				return err
			}

			branch := args[1]
			branches, _ := r.Branches()
			var exists bool
			for _, b := range branches {
				if branch == b {
					exists = true
					break
				}
			}

			if !exists {
				return git.ErrReferenceNotExist
			}

			head, err := r.HEAD()
			if err != nil {
				return err
			}

			if head.Name().Short() == branch {
				return fmt.Errorf("cannot delete the default branch")
			}

			branchCommit, err := r.BranchCommit(branch)
			if err != nil {
				return err
			}

			if err := r.DeleteBranch(branch, gitm.DeleteBranchOptions{Force: true}); err != nil {
				return err
			}

			// The branch is already deleted, so a failed event is logged rather
			// than reported as a failed command.
			wh, err := webhook.NewBranchTagEvent(ctx, proto.UserFromContext(ctx), rr, git.RefsHeads+branch, branchCommit.ID.String(), git.ZeroID)
			if err != nil {
				log.Error("branch deleted but webhook event skipped", "err", err)
				return nil
			}

			// Sent in the background like the backend's own events, so the
			// session neither waits on receivers nor cancels the delivery.
			backend.FromContext(ctx).SendEventAfterChange(ctx, wh)
			return nil
		},
	}

	return cmd
}
