package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/subdiox/notion-cli/internal/api"
	"github.com/subdiox/notion-cli/internal/config"
	"github.com/subdiox/notion-cli/internal/notion"
)

type globalFlags struct {
	json  bool
	debug bool
	space string
	user  string
}

var gf globalFlags

func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "notion",
		Short: "Read and write everything in Notion (including guest workspaces) via the internal API",
		Long: `notion is a CLI for Notion that authenticates with the session of the
Notion desktop app or a browser (token_v2), so it can access every workspace
the user can see — including workspaces where the user is only a guest.

Designed for AI agents: Markdown output by default, --json for raw records.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := root.PersistentFlags()
	pf.BoolVar(&gf.json, "json", false, "output JSON instead of Markdown/text")
	pf.BoolVar(&gf.debug, "debug", os.Getenv("NOTION_DEBUG") != "", "log API requests/responses to stderr")
	pf.StringVar(&gf.space, "space", os.Getenv("NOTION_SPACE"), "workspace (name, domain or ID) for this command")
	pf.StringVar(&gf.user, "user", os.Getenv("NOTION_USER"), "account (email or user ID) for this command")

	root.AddCommand(newAuthCmd(), newSpaceCmd(), newFetchCmd(), newSearchCmd(), newPageCmd(), newDBCmd(), newUserCmd(), newExportCmd(), newRawCmd())
	return root
}

// env bundles the resolved client and config for a command.
type env struct {
	ctx    context.Context
	client *api.Client
	cfg    *config.Config
	ws     *notion.Workspaces
	st     *notion.Store
	users  []spaceUser
}

// newEnv builds an API client. Space resolution happens lazily via resolveSpace.
func newEnv(cmd *cobra.Command) (*env, error) {
	s, err := config.LoadSession()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	userID := cfg.ActiveUserID
	if gf.user != "" {
		userID = gf.user // resolved to an ID later if it is an email
	}
	if userID == "" && len(s.UserIDs) > 0 {
		userID = s.UserIDs[0]
	}
	c := api.New(s, userID, cfg.ActiveSpaceID)
	c.Debug = gf.debug
	return &env{ctx: cmd.Context(), client: c, cfg: cfg}, nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func Execute() {
	if err := NewRoot().ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
