package cli

import (
	"fmt"
	"strings"

	"github.com/subdiox/notion-cli/internal/notion"
)

type account = notion.Account

func (e *env) workspaces() (*notion.Workspaces, error) {
	if e.ws != nil {
		return e.ws, nil
	}
	ws, err := notion.LoadWorkspaces(e.ctx, e.client)
	if err != nil {
		return nil, err
	}
	e.ws = ws
	return ws, nil
}

func (e *env) loadAccounts() ([]account, error) {
	ws, err := e.workspaces()
	if err != nil {
		return nil, err
	}
	if err := e.resolveUser(ws); err != nil {
		return nil, err
	}
	return ws.Accounts, nil
}

// resolveUser turns --user (email, name or ID) into a user ID.
func (e *env) resolveUser(ws *notion.Workspaces) error {
	want := e.client.UserID
	if want == "" {
		return nil
	}
	for _, a := range ws.Accounts {
		if a.ID == want || strings.EqualFold(a.Email, want) || strings.EqualFold(a.Name, want) {
			e.client.UserID = a.ID
			return nil
		}
	}
	return fmt.Errorf("unknown account %q", want)
}

// findSpace matches a workspace by ID, domain or (partial) name.
func findSpace(ws *notion.Workspaces, q string) (*notion.Space, error) {
	if id := notion.NormalizeID(q); id != "" {
		q = id
	}
	var matches []notion.Space
	for _, s := range ws.Spaces {
		if s.ID == q || strings.EqualFold(s.Domain, q) || strings.EqualFold(s.Name, q) {
			return &s, nil
		}
	}
	for _, s := range ws.Spaces {
		if strings.Contains(strings.ToLower(s.Name), strings.ToLower(q)) {
			matches = append(matches, s)
		}
	}
	switch len(matches) {
	case 1:
		return &matches[0], nil
	case 0:
		return nil, fmt.Errorf("no workspace matches %q (see `notion space ls`)", q)
	default:
		names := []string{}
		for _, m := range matches {
			names = append(names, fmt.Sprintf("%s (%s)", m.Name, m.ID))
		}
		return nil, fmt.Errorf("workspace %q is ambiguous: %s", q, strings.Join(names, ", "))
	}
}

// applySpaceFlag applies --space/--user (or the configured defaults) to the client.
func (e *env) applySpaceFlag() error {
	if gf.user != "" {
		ws, err := e.workspaces()
		if err != nil {
			return err
		}
		if err := e.resolveUser(ws); err != nil {
			return err
		}
	}
	if gf.space == "" {
		return nil
	}
	ws, err := e.workspaces()
	if err != nil {
		return err
	}
	sp, err := findSpace(ws, gf.space)
	if err != nil {
		return err
	}
	e.client.SpaceID = sp.ID
	e.client.UserID = sp.UserID
	return nil
}

// currentSpace returns the active workspace (flag > config), erroring if none.
func (e *env) currentSpace() (*notion.Space, error) {
	if err := e.applySpaceFlag(); err != nil {
		return nil, err
	}
	ws, err := e.workspaces()
	if err != nil {
		return nil, err
	}
	if e.client.SpaceID == "" {
		for _, s := range ws.Spaces {
			if s.UserID == e.client.UserID {
				e.client.SpaceID = s.ID
				return &s, nil
			}
		}
		return nil, fmt.Errorf("no workspace selected: run `notion space use <name>` or pass --space")
	}
	for _, s := range ws.Spaces {
		if s.ID == e.client.SpaceID && s.UserID == e.client.UserID {
			return &s, nil
		}
	}
	for _, s := range ws.Spaces {
		if s.ID == e.client.SpaceID {
			e.client.UserID = s.UserID
			return &s, nil
		}
	}
	return nil, fmt.Errorf("active workspace %s not found", e.client.SpaceID)
}

// store returns a record store for the current client.
func (e *env) store() *notion.Store {
	if e.st == nil {
		e.st = notion.NewStore(e.client)
	}
	return e.st
}

// target resolves a URL/ID to a record, switching to whichever logged-in
// account can access it. The client's space header is set to its workspace.
func (e *env) target(ref, table string) (*notion.Record, error) {
	if err := e.applySpaceFlag(); err != nil {
		return nil, err
	}
	id, _, _, err := notion.ParseRef(ref)
	if err != nil {
		return nil, err
	}
	candidates := []string{e.client.UserID}
	if gf.user == "" && gf.space == "" {
		for _, u := range e.client.Session.UserIDs {
			if u != e.client.UserID {
				candidates = append(candidates, u)
			}
		}
	}
	for _, uid := range candidates {
		e.client.UserID = uid
		e.st = nil
		st := e.store()
		role, spaceID, err := st.Role(e.ctx, table, id)
		if err != nil {
			return nil, err
		}
		if role != "none" && role != "" {
			e.client.SpaceID = spaceID
			return st.Records.Get(table, id), nil
		}
	}
	return nil, fmt.Errorf("%s %s not found or not accessible from any logged-in account", table, id)
}
