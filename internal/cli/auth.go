package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/subdiox/notion-cli/internal/auth"
	"github.com/subdiox/notion-cli/internal/config"
)

func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "auth", Short: "Manage the Notion session (token_v2)"}

	var from, profile string
	importCmd := &cobra.Command{
		Use:   "import",
		Short: "Import the session from the Notion desktop app or a browser",
		Long: "Import token_v2 from a local cookie store. Sources: " + sourceNames() + `.
Every profile of the selected source(s) is tried and each session found is
verified against the API; the working one with the most accounts wins
(ties go to the first found: desktop app first, then browsers in the order above).
On macOS, Chromium-based sources read their "Safe Storage" key from the
keychain, which may show an access prompt. On Windows (notion.exe, or the Linux
build under WSL) they are read from the Windows profile and the key is
unprotected with DPAPI; quit the app first, since Windows locks its cookie
store while it runs. Chrome/Edge's app-bound encryption cannot be read: use the
desktop app or Firefox there.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cands, err := auth.Candidates(from)
			if err != nil {
				return err
			}
			if profile != "" {
				var keep []auth.Candidate
				for _, c := range cands {
					if strings.EqualFold(c.Profile(), profile) || strings.Contains(c.Path, profile) {
						keep = append(keep, c)
					}
				}
				if len(keep) == 0 {
					return fmt.Errorf("no Notion login in profile %q (found: %s)", profile, candidateLabels(cands))
				}
				cands = keep
			}
			var best *auth.Candidate
			var bestUsers []account
			for i := range cands {
				c := &cands[i]
				label := c.Session.Source + "/" + c.Profile()
				e, err := newEnvWithSession(cmd, c.Session)
				if err != nil {
					return err
				}
				// A stored active user may belong to another session; verify as
				// this session's own (first) user unless --user was given.
				if gf.user == "" {
					e.client.UserID = ""
					if len(c.Session.UserIDs) > 0 {
						e.client.UserID = c.Session.UserIDs[0]
					}
				}
				users, err := e.loadAccounts()
				if err != nil {
					fmt.Fprintf(os.Stderr, "  %s: rejected (%v)\n", label, err)
					continue
				}
				fmt.Fprintf(os.Stderr, "  %s: %d account(s)\n", label, len(users))
				if best == nil || len(users) > len(bestUsers) {
					best, bestUsers = c, users
				}
			}
			if best == nil {
				return fmt.Errorf("found %d session(s) but none was accepted by Notion (expired?)", len(cands))
			}
			s := best.Session
			if len(s.UserIDs) == 0 {
				for _, u := range bestUsers {
					s.UserIDs = append(s.UserIDs, u.ID)
				}
			}
			if err := config.SaveSession(s); err != nil {
				return err
			}
			fmt.Printf("Imported session from %s/%s (%s)\n", s.Source, best.Profile(), s.Origin)
			for _, u := range bestUsers {
				fmt.Printf("  %s <%s> %s\n", u.Name, u.Email, u.ID)
			}
			return nil
		},
	}
	importCmd.Flags().StringVar(&from, "from", "auto", "source: auto (all), "+sourceNames())
	importCmd.Flags().StringVar(&profile, "profile", "", `only this profile (directory name like "Profile 1", or a path substring)`)

	tokenCmd := &cobra.Command{
		Use:   "set-token <token_v2>",
		Short: "Store a token_v2 value copied manually from browser devtools",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			origin, _ := cmd.Flags().GetString("origin")
			s := &auth.Session{Source: "manual", TokenV2: strings.TrimSpace(args[0]), Origin: origin}
			if err := config.SaveSession(s); err != nil {
				return err
			}
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			users, err := e.loadAccounts()
			if err != nil {
				return err
			}
			s.UserIDs = nil
			for _, u := range users {
				s.UserIDs = append(s.UserIDs, u.ID)
			}
			return config.SaveSession(s)
		},
	}
	tokenCmd.Flags().String("origin", "https://www.notion.so", "Notion origin")

	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show the logged-in accounts",
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			users, err := e.loadAccounts()
			if err != nil {
				return err
			}
			if gf.json {
				return printJSON(map[string]any{"source": e.client.Session.Source, "origin": e.client.Session.Origin, "active_user_id": e.client.UserID, "users": users})
			}
			fmt.Printf("source: %s (%s)\n", e.client.Session.Source, e.client.Session.Origin)
			for _, u := range users {
				mark := " "
				if u.ID == e.client.UserID {
					mark = "*"
				}
				fmt.Printf("%s %s <%s> %s\n", mark, u.Name, u.Email, u.ID)
			}
			return nil
		},
	}

	logoutCmd := &cobra.Command{
		Use:   "logout",
		Short: "Delete the stored session (does not log out the app/browser)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return config.DeleteSession()
		},
	}

	cmd.AddCommand(importCmd, tokenCmd, statusCmd, logoutCmd)
	return cmd
}

func sourceNames() string {
	names := make([]string, len(auth.Sources))
	for i, s := range auth.Sources {
		names[i] = s.Name
		if len(s.Aliases) > 0 {
			names[i] += " (" + strings.Join(s.Aliases, ", ") + ")"
		}
	}
	return strings.Join(names, ", ")
}

func candidateLabels(cands []auth.Candidate) string {
	var out []string
	for _, c := range cands {
		out = append(out, c.Session.Source+"/"+c.Profile())
	}
	return strings.Join(out, ", ")
}
