package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/subdiox/notion-cli/internal/notion"
)

type spaceUser struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Email      string `json:"email"`
	Membership string `json:"membership"`
}

// spaceUsers lists users visible in the current workspace.
func (e *env) spaceUsers() ([]spaceUser, error) {
	if e.users != nil {
		return e.users, nil
	}
	if e.client.SpaceID == "" {
		if _, err := e.currentSpace(); err != nil {
			return nil, err
		}
	}
	var resp struct {
		VisibleUsers []struct {
			UserID         string   `json:"userId"`
			Aliases        []string `json:"aliases"`
			MembershipType string   `json:"membershipType"`
		} `json:"visibleUsers"`
	}
	if err := e.client.Call(e.ctx, "getVisibleUsers", map[string]any{"spaceId": e.client.SpaceID}, &resp); err != nil {
		return nil, err
	}
	st := e.store()
	var ptrs []notion.Pointer
	for _, u := range resp.VisibleUsers {
		ptrs = append(ptrs, notion.Pointer{Table: "notion_user", ID: u.UserID})
	}
	if err := st.Fetch(e.ctx, ptrs, false); err != nil {
		return nil, err
	}
	for _, u := range resp.VisibleUsers {
		su := spaceUser{ID: u.UserID, Membership: u.MembershipType}
		if r := st.Records.Get("notion_user", u.UserID); r != nil {
			su.Name, su.Email = r.Str("name"), r.Str("email")
		}
		if su.Email == "" && len(u.Aliases) > 0 {
			su.Email = u.Aliases[0]
		}
		e.users = append(e.users, su)
	}
	return e.users, nil
}

// resolvePerson maps a user ID, email or name to a user ID.
func (e *env) resolvePerson(q string) (string, error) {
	q = strings.TrimSpace(strings.TrimPrefix(q, "user://"))
	if id := notion.NormalizeID(q); id != "" {
		return id, nil
	}
	if q == "me" {
		return e.client.UserID, nil
	}
	users, err := e.spaceUsers()
	if err != nil {
		return "", err
	}
	var partial []spaceUser
	for _, u := range users {
		if strings.EqualFold(u.Email, q) || strings.EqualFold(u.Name, q) {
			return u.ID, nil
		}
		if strings.Contains(strings.ToLower(u.Name), strings.ToLower(q)) {
			partial = append(partial, u)
		}
	}
	if len(partial) == 1 {
		return partial[0].ID, nil
	}
	return "", fmt.Errorf("cannot resolve user %q (use `notion user ls`)", q)
}

func newUserCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "user", Aliases: []string{"users"}, Short: "Workspace users"}
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List users visible in the current workspace",
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			if _, err := e.currentSpace(); err != nil {
				return err
			}
			users, err := e.spaceUsers()
			if err != nil {
				return err
			}
			if gf.json {
				return printJSON(users)
			}
			for _, u := range users {
				fmt.Printf("- %s <%s> %s (%s)\n", u.Name, u.Email, u.ID, u.Membership)
			}
			return nil
		},
	}
	me := &cobra.Command{
		Use:   "me",
		Short: "Show the active account",
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newEnv(cmd)
			if err != nil {
				return err
			}
			if err := e.applySpaceFlag(); err != nil {
				return err
			}
			accs, err := e.loadAccounts()
			if err != nil {
				return err
			}
			for _, a := range accs {
				if a.ID == e.client.UserID {
					if gf.json {
						return printJSON(a)
					}
					fmt.Printf("%s <%s> %s\n", a.Name, a.Email, a.ID)
				}
			}
			return nil
		},
	}
	cmd.AddCommand(ls, me)
	return cmd
}
