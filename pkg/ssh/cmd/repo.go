package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/spf13/cobra"
)

// RepoCommand returns a command for managing repositories. Each leaf owns
// its PersistentPreRunE gate because cobra runs only the nearest one.
func RepoCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "repo",
		Aliases: []string{"repos", "repository", "repositories"},
		Short:   "Manage repositories",
	}

	cmd.AddCommand(
		blobCommand(),
		branchCommand(),
		collabCommand(),
		commitCommand(),
		createCommand(),
		deleteCommand(),
		descriptionCommand(),
		hiddenCommand(),
		importCommand(),
		listCommand(),
		mirrorCommand(),
		privateCommand(),
		projectName(),
		renameCommand(),
		tagCommand(),
		treeCommand(),
		webhookCommand(),
	)

	cmd.AddCommand(
		&cobra.Command{
			Use:               "info REPOSITORY",
			Short:             "Get information about a repository",
			Args:              cobra.ExactArgs(1),
			PersistentPreRunE: requireRepo(access.ReadOnlyAccess),
			RunE: func(cmd *cobra.Command, _ []string) error {
				ctx := cmd.Context()
				be := backend.FromContext(ctx)
				rr, _, err := repoFromContext(ctx)
				if err != nil {
					return err
				}

				r, err := rr.Open()
				if err != nil {
					return err
				}

				// An empty repository, or one whose HEAD names a branch
				// that does not exist, has no default branch to show.
				head, err := r.HEAD()
				if err != nil && !errors.Is(err, git.ErrReferenceNotExist) {
					return err
				}

				var owner proto.User
				if rr.UserID() > 0 {
					owner, err = be.UserByID(ctx, rr.UserID())
					if err != nil {
						return err
					}
				}

				branches, _ := r.Branches()
				tags, _ := r.Tags()

				// project name and description are optional, handle trailing
				// whitespace to avoid breaking tests.
				cmd.Println(strings.TrimSpace(fmt.Sprint("Project Name: ", rr.ProjectName())))
				cmd.Println("Repository:", rr.Name())
				cmd.Println(strings.TrimSpace(fmt.Sprint("Description: ", rr.Description())))
				cmd.Println("Private:", rr.IsPrivate())
				cmd.Println("Hidden:", rr.IsHidden())
				cmd.Println("Mirror:", rr.IsMirror())
				if owner != nil {
					cmd.Println(strings.TrimSpace(fmt.Sprint("Owner: ", owner.Username())))
				}
				if head != nil {
					cmd.Println("Default Branch:", head.Name().Short())
				}
				if len(branches) > 0 {
					cmd.Println("Branches:")
					for _, b := range branches {
						cmd.Println("  -", b)
					}
				}
				if len(tags) > 0 {
					cmd.Println("Tags:")
					for _, t := range tags {
						cmd.Println("  -", t)
					}
				}

				return nil
			},
		},
	)

	return cmd
}

// errRepoEmpty is returned for an empty repository by commands that need a
// default branch.
var errRepoEmpty = errors.New("repository is empty")

// repoHead returns the repository's HEAD reference, the default branch. An
// empty repository, or a HEAD that names a branch that does not exist, such
// as in a mirror whose remote renamed its default branch, gets an error that
// says so rather than "reference does not exist".
func repoHead(r *git.Repository) (*git.Reference, error) {
	head, err := r.HEAD()
	if !errors.Is(err, git.ErrReferenceNotExist) {
		return head, err
	}
	if bs, _ := r.Branches(); len(bs) == 0 {
		return nil, errRepoEmpty
	}
	name, _ := r.SymbolicRef(git.HEAD, "")
	return nil, fmt.Errorf("default branch %q does not exist", git.ReferenceName(name).Short())
}
