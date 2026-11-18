package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/emailtags"
)

type messageArchiveBackend interface {
	SupportsAPISchemaVersion(ctx context.Context, version string) (bool, error)
	ArchiveMessage(ctx context.Context, id int64, change emailtags.ArchiveChange) (*emailtags.ArchiveResult, error)
}

func newMessageArchiveCommand(archived bool, backend messageArchiveBackend) *cobra.Command {
	var id int64
	var dryRun, asJSON, wholeChat bool
	var mailbox string
	var uidvalidity, uid uint32
	name, short := "archive", "Archive a Gmail or IMAP message or a Beeper chat"
	if !archived {
		name, short = "unarchive", "Restore a Gmail or IMAP message or a Beeper chat to its inbox"
	}
	cmd := &cobra.Command{
		Use: name + " --message <id>", Short: short, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if id <= 0 {
				return errors.New("--message must be a positive archived message ID")
			}
			var selection *emailtags.IMAPCopy
			present := 0
			for _, name := range []string{"mailbox", "uidvalidity", "uid"} {
				if cmd.Flags().Changed(name) {
					present++
				}
			}
			if present != 0 {
				selection = &emailtags.IMAPCopy{Mailbox: mailbox, UIDValidity: uidvalidity, UID: uid}
				if present != 3 || !selection.Valid() {
					return errors.New("--mailbox, --uidvalidity and --uid require nonzero values together")
				}
			}
			client := backend
			if client == nil {
				st, _, err := OpenHTTPStore(cmd.Context())
				if err != nil {
					return err
				}
				defer func() { _ = st.Close() }()
				client = st
			}
			supported, err := client.SupportsAPISchemaVersion(cmd.Context(), daemonclient.MessageArchiveMinAPISchemaVersion)
			if err != nil {
				return fmt.Errorf("check daemon archive support: %w", err)
			}
			if !supported {
				return fmt.Errorf("upgrade the daemon to API schema %s or newer for message archiving", daemonclient.MessageArchiveMinAPISchemaVersion)
			}
			result, err := client.ArchiveMessage(cmd.Context(), id, emailtags.ArchiveChange{Archived: &archived, DryRun: dryRun, WholeChat: wholeChat, IMAPCopy: selection})
			if asJSON {
				if writeErr := writeMessageTagJSON(cmd, result, err, emailtags.Failure("request_failed", "Cannot set provider archive state", nil, err), "write message archive JSON"); writeErr != nil {
					return writeErr
				}
				return err
			}
			if err != nil {
				return err
			}
			if len(result.IMAPCandidates) > 0 {
				return writeMessageTagJSON(cmd, result, nil, nil, "write IMAP archive result")
			}
			if result.ChatID != "" {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "Beeper chat %q: archived=%t changed=%t dry_run=%t\n", result.ChatID, result.Archived, result.Changed, result.DryRun)
				if err != nil {
					return fmt.Errorf("write Beeper archive result: %w", err)
				}
				return nil
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Message %d: archived=%t changed=%t dry_run=%t\n", id, result.Archived, result.Changed, result.DryRun)
			if err != nil {
				return fmt.Errorf("write message archive result: %w", err)
			}
			if selection != nil && result.IMAPCopy != nil {
				destination := result.IMAPCopy
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "imap_copy: mailbox=%q uidvalidity=%d uid=%d\n", destination.Mailbox, destination.UIDValidity, destination.UID); err != nil {
					return fmt.Errorf("write message archive result: %w", err)
				}
			}
			return nil
		},
	}
	cmd.Flags().Int64Var(&id, "message", 0, "Archived message ID")
	cmd.Flags().StringVar(&mailbox, "mailbox", "", "Exact IMAP mailbox (requires --uidvalidity and --uid)")
	cmd.Flags().Uint32Var(&uidvalidity, "uidvalidity", 0, "IMAP mailbox UIDVALIDITY")
	cmd.Flags().Uint32Var(&uid, "uid", 0, "IMAP copy UID")
	cmd.Flags().BoolVar(&wholeChat, "whole-chat", false, "Archive the entire Beeper chat containing this message")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview without writing")
	cmd.Flags().BoolVar(&asJSON, flagJSON, false, "Output result as JSON")
	return cmd
}

func init() {
	rootCmd.AddCommand(newMessageArchiveCommand(true, nil), newMessageArchiveCommand(false, nil))
}
