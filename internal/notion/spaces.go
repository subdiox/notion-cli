package notion

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/subdiox/notion-cli/internal/api"
)

type Account struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type Space struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Domain     string `json:"domain,omitempty"`
	UserID     string `json:"user_id"`
	UserEmail  string `json:"user_email"`
	Membership string `json:"membership"` // owner, member, page_guest, ...
	Plan       string `json:"plan,omitempty"`
	Icon       string `json:"icon,omitempty"`
}

func (s Space) IsGuest() bool { return s.Membership == "page_guest" || s.Membership == "guest" }

// Workspaces is the result of getSpaces across all logged-in accounts.
type Workspaces struct {
	Accounts []Account
	Spaces   []Space
	Records  map[string]RecordMap // per user ID
}

func LoadWorkspaces(ctx context.Context, c *api.Client) (*Workspaces, error) {
	var raw map[string]json.RawMessage
	if err := c.Call(ctx, "getSpaces", map[string]any{}, &raw); err != nil {
		return nil, err
	}
	ws := &Workspaces{Records: map[string]RecordMap{}}
	var unknown []string
	for _, uid := range sortedKeys(raw) {
		rm, err := ParseRecordMap(raw[uid])
		if err != nil {
			return nil, err
		}
		ws.Records[uid] = rm
		acc := Account{ID: uid}
		if u := rm.Get("notion_user", uid); u != nil {
			acc.Name, acc.Email = u.Str("name"), u.Str("email")
		}
		ws.Accounts = append(ws.Accounts, acc)
		for _, su := range rm["space_user"] {
			if su.Value == nil {
				continue
			}
			sid := su.Str("space_id")
			sp := Space{ID: sid, UserID: uid, UserEmail: acc.Email, Membership: su.Str("membership_type")}
			if rec := rm.Get("space", sid); rec != nil {
				sp.Name, sp.Domain, sp.Plan, sp.Icon = rec.Str("name"), rec.Str("domain"), rec.Str("plan_type"), rec.Str("icon")
			} else {
				unknown = append(unknown, sid)
			}
			ws.Spaces = append(ws.Spaces, sp)
		}
	}
	// getPublicSpaceData only answers the first ID of a batch, so ask per space.
	for _, sid := range unknown {
		var pub struct {
			Results []map[string]any `json:"results"`
		}
		if err := c.Call(ctx, "getPublicSpaceData", map[string]any{"type": "space-ids", "spaceIds": []string{sid}}, &pub); err != nil {
			continue
		}
		for _, r := range pub.Results {
			for i, sp := range ws.Spaces {
				if sp.ID == str(r["id"]) && sp.Name == "" {
					ws.Spaces[i].Name, ws.Spaces[i].Domain, ws.Spaces[i].Icon = str(r["name"]), str(r["domain"]), str(r["icon"])
				}
			}
		}
	}
	sort.SliceStable(ws.Spaces, func(i, j int) bool {
		if ws.Spaces[i].UserEmail != ws.Spaces[j].UserEmail {
			return ws.Spaces[i].UserEmail < ws.Spaces[j].UserEmail
		}
		if ws.Spaces[i].IsGuest() != ws.Spaces[j].IsGuest() {
			return !ws.Spaces[i].IsGuest()
		}
		return ws.Spaces[i].Name < ws.Spaces[j].Name
	})
	return ws, nil
}

// SpaceView returns the user's space_view record for a space (holds private_pages, bookmarks).
func (ws *Workspaces) SpaceView(userID, spaceID string) *Record {
	for _, r := range ws.Records[userID]["space_view"] {
		if r.Value != nil && r.Str("space_id") == spaceID {
			return r
		}
	}
	return nil
}
