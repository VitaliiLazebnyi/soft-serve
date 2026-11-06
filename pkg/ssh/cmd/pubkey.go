package cmd

import (
	"errors"
	"strings"

	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/spf13/cobra"
)

// errListedAdminKey is returned when a non-admin tries to attach a key that
// is listed in initial_admin_keys.
var errListedAdminKey = errors.New("this key is listed in initial_admin_keys; only an admin can attach it to an account (user add-pubkey)")

// PubkeyCommand returns a command that manages user public keys.
func PubkeyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "pubkey",
		Aliases: []string{"pubkeys", "publickey", "publickeys"},
		Short:   "Manage your public keys",
	}

	pubkeyAddCommand := &cobra.Command{
		Use:   "add AUTHORIZED_KEY",
		Short: "Add a public key",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			be := backend.FromContext(ctx)
			pk := sshutils.PublicKeyFromContext(ctx)
			user, err := be.UserByPublicKey(ctx, pk)
			if err != nil {
				return err
			}

			apk, _, err := sshutils.ParseAuthorizedKey(strings.Join(args, " "))
			if err != nil {
				return err
			}

			// Adding a key proves nothing about holding its private half,
			// so a non-admin may not attach a key listed in
			// initial_admin_keys. (Sessions with a listed key owned by a
			// non-admin do not act as that account anyway; see
			// Backend.UserByPublicKey.) The error says why, so a new admin
			// knows to ask an admin rather than retry. That tells the
			// caller the key is listed, which is harmless: it is a public
			// key, and listing grants nothing without the private half.
			if !isServerAdmin(ctx) && IsPublicKeyAdmin(config.FromContext(ctx), apk) {
				return errListedAdminKey
			}

			return be.AddPublicKey(ctx, user.Username(), apk)
		},
	}

	pubkeyRemoveCommand := &cobra.Command{
		Use:   "remove AUTHORIZED_KEY",
		Args:  cobra.MinimumNArgs(1),
		Short: "Remove a public key",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			be := backend.FromContext(ctx)
			pk := sshutils.PublicKeyFromContext(ctx)
			user, err := be.UserByPublicKey(ctx, pk)
			if err != nil {
				return err
			}

			apk, _, err := sshutils.ParseAuthorizedKey(strings.Join(args, " "))
			if err != nil {
				return err
			}

			return be.RemovePublicKey(ctx, user.Username(), apk)
		},
	}

	pubkeyListCommand := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List public keys",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			be := backend.FromContext(ctx)
			pk := sshutils.PublicKeyFromContext(ctx)
			user, err := be.UserByPublicKey(ctx, pk)
			if err != nil {
				return err
			}

			pks := user.PublicKeys()
			for _, pk := range pks {
				cmd.Println(sshutils.MarshalAuthorizedKey(pk))
			}

			return nil
		},
	}

	cmd.AddCommand(
		pubkeyAddCommand,
		pubkeyRemoveCommand,
		pubkeyListCommand,
	)

	return cmd
}
