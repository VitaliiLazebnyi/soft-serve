package cmd

import (
	"errors"
	"fmt"

	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/spf13/cobra"
)

// createCommand is the command for creating a new repository.
func createCommand() *cobra.Command {
	var private bool
	var description string
	var projectName string
	var hidden bool

	cmd := &cobra.Command{
		Use:               "create REPOSITORY",
		Short:             "Create a new repository",
		Args:              cobra.ExactArgs(1),
		PersistentPreRunE: checkIfRepoCollab,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg := config.FromContext(ctx)
			be := backend.FromContext(ctx)
			user := proto.UserFromContext(ctx)
			name := args[0]
			private, err := privateOrDefault(cmd, private)
			if err != nil {
				return err
			}
			r, err := be.CreateRepository(ctx, name, user, proto.RepositoryOptions{
				Private:     private,
				Description: description,
				ProjectName: projectName,
				Hidden:      hidden,
			})
			if err != nil {
				return err
			}

			cloneurl := fmt.Sprintf("%s/%s.git", cfg.SSH.PublicURL, r.Name())
			cmd.PrintErrf("Created repository %s\n", r.Name())
			cmd.Println(cloneurl)

			return nil
		},
	}

	cmd.Flags().BoolVarP(&private, "private", "p", false, "make the repository private (default: the server's default_private)")
	cmd.Flags().StringVarP(&description, "description", "d", "", "set the repository description")
	cmd.Flags().StringVarP(&projectName, "name", "n", "", "set the project name")
	cmd.Flags().BoolVarP(&hidden, "hidden", "H", false, "hide the repository from the UI")

	return cmd
}

// privateOrDefault returns the visibility for a new repository: the value of
// the --private flag when it was given, and the server's default otherwise
// (see Backend.DefaultPrivate). A bool flag alone cannot tell
// "--private=false" from no flag.
func privateOrDefault(cmd *cobra.Command, private bool) (bool, error) {
	if cmd.Flags().Changed("private") {
		return private, nil
	}
	ctx := cmd.Context()
	private, err := backend.FromContext(ctx).DefaultPrivate(ctx, proto.UserFromContext(ctx))
	if errors.Is(err, proto.ErrAnonymousPrivate) {
		return false, fmt.Errorf("%w; use a key registered to an account, or pass --private=false to create a public repository", err)
	}
	return private, err
}
