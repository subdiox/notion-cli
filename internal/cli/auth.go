package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/subdiox/notion-cli/internal/auth"
	"github.com/subdiox/notion-cli/internal/config"
)

func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "auth", Short: "Manage the Notion session (token_v2)"}

	var from string
	importCmd := &cobra.Command{
		Use:   "import",
		Short: "Import the session from the Notion desktop app or a browser",
		Long: "Import token_v2 from a local cookie store. Sources: " + sourceNames() + `.
Chromium-based sources read their "Safe Storage" key from the macOS keychain,
which may show an access prompt.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := auth.Import(from)
			if err != nil {
				return err
			}
			if err := config.SaveSession(s); err != nil {
				return err
			}
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			users, err := e.loadAccounts()
			if err != nil {
				return fmt.Errorf("session imported but verification failed: %w", err)
			}
			fmt.Printf("Imported session from %s (%s)\n", s.Source, s.Origin)
			for _, u := range users {
				fmt.Printf("  %s <%s> %s\n", u.Name, u.Email, u.ID)
			}
			return nil
		},
	}
	importCmd.Flags().StringVar(&from, "from", "auto", "source: auto (first that works), "+sourceNames())

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
	}
	return strings.Join(names, ", ")
}
