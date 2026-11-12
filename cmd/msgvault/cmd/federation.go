package cmd

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/federation"
	"go.kenn.io/msgvault/internal/providercredentials"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

const federationMinAPISchema = "5.2.0"

func init() {
	root := &cobra.Command{Use: "federation", Short: "Manage hub enrollment"}
	identity := &cobra.Command{Use: "identity", Short: "Print this archive's federation UID", Long: "Print this archive's federation UID. Set [federation] role to spoke or hub and restart the daemon first.", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, _, err := OpenHTTPStore(cmd.Context(), federationMinAPISchema)
		if err != nil {
			return err
		}
		out, err := c.FederationIdentity(cmd.Context())
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), out.ArchiveUID)
		return nil
	}}
	var spoke, member, name string
	enroll := &cobra.Command{Use: "enroll", Short: "Enroll a spoke on this hub", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if spoke == "" || member == "" {
			return errors.New("--spoke-instance and --member are required")
		}
		c, _, err := OpenHTTPStore(cmd.Context(), federationMinAPISchema)
		if err != nil {
			return err
		}
		result, err := c.CreateFederationEnrollment(cmd.Context(), spoke, member, name)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), result.Token); err != nil {
			return fmt.Errorf("enrollment %s was created but token delivery failed; revoke it and re-enroll: %w", result.Enrollment.EnrollmentUID, err)
		}
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Enrollment: %s\nMember: %s\n", result.Enrollment.EnrollmentUID, result.Enrollment.Member)
		if result.Enrollment.SpokeName != result.Enrollment.Member {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Spoke: %s\n", result.Enrollment.SpokeName)
		}
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Join: msgvault federation join <hub-url> --hub-instance %s --token-file <path>\n", result.HubInstance)
		return nil
	}}
	enroll.Flags().StringVar(&spoke, "spoke-instance", "", "Spoke archive UID (64 lowercase hexadecimal digits)")
	enroll.Flags().StringVar(&member, "member", "", "Member handle (any non-blank name without control characters)")
	enroll.Flags().StringVar(&name, "spoke-name", "", "Spoke display name (no control characters)")
	list := &cobra.Command{Use: "list", Short: "List spoke enrollments", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, _, err := OpenHTTPStore(cmd.Context(), federationMinAPISchema)
		if err != nil {
			return err
		}
		entries, err := c.ListFederationEnrollments(cmd.Context())
		if err != nil {
			return err
		}
		for _, e := range entries {
			state := accountStateActive
			if e.RevokedAt != nil {
				state = "revoked"
			}
			names := e.Member
			if e.SpokeName != e.Member {
				names += " (" + e.SpokeName + ")"
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n", e.EnrollmentUID, e.SpokeArchiveUID, names, state)
		}
		return nil
	}}
	enrollments := &cobra.Command{Use: "enrollments", Short: "List enrollments"}
	enrollments.AddCommand(list)
	revoke := &cobra.Command{Use: "revoke <enrollment-uid>", Short: "Revoke a spoke enrollment", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(args[0]) == "" {
			return errors.New("enrollment UID is required")
		}
		c, _, err := OpenHTTPStore(cmd.Context(), federationMinAPISchema)
		if err != nil {
			return err
		}
		if err = c.RevokeFederationEnrollment(cmd.Context(), args[0]); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Enrollment revoked.")
		return nil
	}}
	root.AddCommand(identity, enroll, enrollments, revoke, newFederationJoinCmd(), newFederationStatusCmd(), newFederationLeaveCmd())
	rootCmd.AddCommand(root)
}

func federationSpokeStore(cmd *cobra.Command) (*store.Store, func(), error) {
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return nil, nil, errors.New("configuration is unavailable")
	}
	if state.cfg.Federation.Role != "spoke" {
		return nil, nil, errors.New("federation role must be spoke")
	}
	return openWritableStoreAndInitForIngestInvocation(cmd.Context(), state)
}

func newFederationJoinCmd() *cobra.Command {
	var instance, tokenFile string
	cmd := &cobra.Command{Use: "join <hub-url>", Short: "Join a federation hub", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if instance == "" || tokenFile == "" {
			return errors.New("--hub-instance and --token-file are required")
		}
		if !isDaemonCLISubprocess() {
			var reader io.Reader = cmd.InOrStdin()
			if tokenFile != "-" {
				f, err := os.Open(tokenFile)
				if err != nil {
					return errors.New("cannot open enrollment token file")
				}
				defer func() { _ = f.Close() }()
				reader = f
			}
			token, err := providercredentials.ReadSecret(reader)
			if err != nil {
				return err
			}
			return runDaemonCLICommandHTTPFromCobraWithEnv(cmd, args, map[string]string{clirun.EnvFederationToken: token})
		}
		state := invocationFromCommand(cmd)
		st, cleanup, err := federationSpokeStore(cmd)
		if err != nil {
			return err
		}
		defer cleanup()
		if err := federation.Join(cmd.Context(), st, state.cfg.TokensDir(), args[0], instance, os.Getenv(clirun.EnvFederationToken)); err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "Joined hub; enrolled, 0 shares.")
		return err
	}}
	cmd.Flags().StringVar(&instance, "hub-instance", "", "Expected hub archive UID")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "Enrollment token file, or - for stdin")
	return cmd
}

func newFederationStatusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{Use: "status", Short: "Show federation links", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		state := invocationFromCommand(cmd)
		st, cleanup, err := federationSpokeStore(cmd)
		if err != nil {
			return err
		}
		defer cleanup()
		links, err := federation.Status(cmd.Context(), st, state.cfg.TokensDir())
		if err != nil {
			return err
		}
		return writeFederationStatus(cmd.OutOrStdout(), links, asJSON)
	}}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output JSON")
	return cmd
}

func newFederationLeaveCmd() *cobra.Command {
	var localOnly bool
	cmd := &cobra.Command{Use: "leave <hub-url>", Short: "Leave a federation hub", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		state := invocationFromCommand(cmd)
		st, cleanup, err := federationSpokeStore(cmd)
		if err != nil {
			return err
		}
		defer cleanup()
		uid, err := federation.Leave(cmd.Context(), st, state.cfg.TokensDir(), args[0], localOnly)
		if err != nil {
			return err
		}
		if localOnly {
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Local link removed; hub enrollment %s may still be active.\n", textutil.SanitizeTerminal(uid))
		} else {
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "Hub enrollment revoked and local link removed.")
		}
		return err
	}}
	cmd.Flags().BoolVar(&localOnly, "local-only", false, "Remove local state without contacting the hub")
	return cmd
}

func writeFederationStatus(out io.Writer, links []federation.LinkStatus, asJSON bool) error {
	if asJSON {
		data, err := json.Marshal(links)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, string(data))
		return err
	}
	for _, link := range links {
		policy, err := json.Marshal(link.Policy)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "%s: %s, %s, %d shares\nPolicy: %s\n", textutil.SanitizeTerminal(link.HubOrigin), textutil.SanitizeTerminal(link.Member), textutil.SanitizeTerminal(link.State), link.Shares, textutil.SanitizeTerminal(string(policy)))
		if err != nil {
			return err
		}
	}
	return nil
}
