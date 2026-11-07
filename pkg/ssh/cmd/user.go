package cmd

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
)

// splitUserCreateKey returns the username and public key for
// "user create" when -k is given. args holds the positional arguments.
//
// When the key was not quoted, the server's command splitting leaves the
// key type in the flag and the rest of the key in args, either after the
// username ("create bob -k ssh-ed25519 AAAA") or before it ("create -k
// ssh-ed25519 AAAA bob"). Both readings are tried; the one that gives a
// valid key is used. Input that fits neither, or both, is rejected.
func splitUserCreateKey(key string, args []string) (string, ssh.PublicKey, error) {
	if len(args) == 1 {
		pk, _, err := sshutils.ParseAuthorizedKey(key)
		return args[0], pk, err
	}

	// A complete key in the flag leaves no key words in args, so the
	// extra arguments are a mistake.
	if _, _, err := sshutils.ParseAuthorizedKey(key); err == nil {
		return "", nil, fmt.Errorf("accepts 1 arg(s), received %d", len(args))
	}

	last := len(args) - 1
	pkFirst, _, errFirst := sshutils.ParseAuthorizedKey(key + " " + strings.Join(args[1:], " "))
	pkLast, _, errLast := sshutils.ParseAuthorizedKey(key + " " + strings.Join(args[:last], " "))
	switch {
	case errFirst == nil && errLast == nil:
		return "", nil, errors.New("cannot tell the username from the public key; quote the key, for example -k \"ssh-ed25519 AAAA...\"")
	case errFirst == nil:
		return args[0], pkFirst, nil
	case errLast == nil:
		return args[last], pkLast, nil
	default:
		return "", nil, fmt.Errorf("invalid public key: %w", errFirst)
	}
}

// UserCommand returns the user subcommand.
func UserCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "user",
		Aliases: []string{"users"},
		Short:   "Manage users",
		// Gate the whole command tree rather than each subcommand. Cobra
		// runs the nearest persistent pre-run found when walking up from
		// the invoked command, so subcommands added later are gated by
		// default instead of silently unprotected.
		PersistentPreRunE: checkIfServerAdmin,
	}

	var admin bool
	var key string
	userCreateCommand := &cobra.Command{
		Use:   "create USERNAME",
		Short: "Create a new user",
		// The SSH server splits the remote command on spaces, so an
		// unquoted key such as "-k ssh-ed25519 AAAA..." leaves the rest
		// of the key as extra arguments. Allow them when -k is given and
		// put the key back together in RunE.
		Args: func(cmd *cobra.Command, args []string) error {
			if key != "" {
				return cobra.MinimumNArgs(1)(cmd, args)
			}
			return cobra.ExactArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			var pubkeys []ssh.PublicKey
			ctx := cmd.Context()
			be := backend.FromContext(ctx)
			username := args[0]
			if key != "" {
				var pk ssh.PublicKey
				var err error
				username, pk, err = splitUserCreateKey(key, args)
				if err != nil {
					return err
				}

				pubkeys = []ssh.PublicKey{pk}
			}

			opts := proto.UserOptions{
				Admin:      admin,
				PublicKeys: pubkeys,
			}

			_, err := be.CreateUser(ctx, username, opts)
			return err
		},
	}

	userCreateCommand.Flags().BoolVarP(&admin, "admin", "a", false, "make the user an admin")
	userCreateCommand.Flags().StringVarP(&key, "key", "k", "", "add a public key to the user")

	userDeleteCommand := &cobra.Command{
		Use:   "delete USERNAME",
		Short: "Delete a user",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			be := backend.FromContext(ctx)
			username := args[0]

			return be.DeleteUser(ctx, username)
		},
	}

	userListCommand := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List users",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			be := backend.FromContext(ctx)
			users, err := be.Users(ctx)
			if err != nil {
				return err
			}

			sort.Strings(users)
			for _, u := range users {
				cmd.Println(u)
			}

			return nil
		},
	}

	userAddPubkeyCommand := &cobra.Command{
		Use:   "add-pubkey USERNAME AUTHORIZED_KEY",
		Short: "Add a public key to a user",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			be := backend.FromContext(ctx)
			username := args[0]
			pubkey := strings.Join(args[1:], " ")
			pk, _, err := sshutils.ParseAuthorizedKey(pubkey)
			if err != nil {
				return err
			}

			return be.AddPublicKey(ctx, username, pk)
		},
	}

	userRemovePubkeyCommand := &cobra.Command{
		Use:   "remove-pubkey USERNAME AUTHORIZED_KEY",
		Short: "Remove a public key from a user",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			be := backend.FromContext(ctx)
			username := args[0]
			pubkey := strings.Join(args[1:], " ")
			pk, _, err := sshutils.ParseAuthorizedKey(pubkey)
			if err != nil {
				return err
			}

			return be.RemovePublicKey(ctx, username, pk)
		},
	}

	userSetAdminCommand := &cobra.Command{
		Use:   "set-admin USERNAME [true|false]",
		Short: "Make a user an admin",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			be := backend.FromContext(ctx)
			username := args[0]

			return be.SetAdmin(ctx, username, args[1] == "true")
		},
	}

	userInfoCommand := &cobra.Command{
		Use:   "info USERNAME",
		Short: "Show information about a user",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			be := backend.FromContext(ctx)
			username := args[0]

			user, err := be.User(ctx, username)
			if err != nil {
				return err
			}

			isAdmin := user.IsAdmin()

			cmd.Printf("Username: %s\n", user.Username())
			cmd.Printf("Admin: %t\n", isAdmin)
			cmd.Printf("Public keys:\n")
			for _, pk := range user.PublicKeys() {
				cmd.Printf("  %s\n", sshutils.MarshalAuthorizedKey(pk))
			}

			return nil
		},
	}

	userSetUsernameCommand := &cobra.Command{
		Use:   "set-username USERNAME NEW_USERNAME",
		Short: "Change a user's username",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			be := backend.FromContext(ctx)
			username := args[0]
			newUsername := args[1]

			return be.SetUsername(ctx, username, newUsername)
		},
	}

	cmd.AddCommand(
		userCreateCommand,
		userAddPubkeyCommand,
		userInfoCommand,
		userListCommand,
		userDeleteCommand,
		userRemovePubkeyCommand,
		userSetAdminCommand,
		userSetUsernameCommand,
	)

	return cmd
}
