package notion

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"strings"
)

// NeedsSigning reports whether a file source is stored by Notion (and so needs
// a signed URL or the session cookie to download).
func NeedsSigning(src string) bool {
	if strings.HasPrefix(src, "attachment:") {
		return true
	}
	u, err := url.Parse(src)
	if err != nil {
		return false
	}
	h := u.Host
	return strings.Contains(h, "secure.notion-static.com") || strings.Contains(h, "prod-files-secure") ||
		strings.HasSuffix(h, "notionusercontent.com") || (strings.Contains(h, "s3") && strings.Contains(u.Path, "secure.notion-static.com"))
}

// AbsoluteSource turns relative Notion asset paths (/images/...) into URLs.
func AbsoluteSource(src string) string {
	if strings.HasPrefix(src, "/") {
		return "https://www.notion.so" + src
	}
	return src
}

type signReq struct {
	URL              string         `json:"url"`
	PermissionRecord map[string]any `json:"permissionRecord"`
	UseCase          string         `json:"useCase,omitempty"`
}

// SignedURL returns a temporary download URL for a file stored in Notion.
func (s *Store) SignedURLs(ctx context.Context, blocks []*Record, srcs []string) ([]string, error) {
	reqs := make([]signReq, len(srcs))
	for i, src := range srcs {
		b := blocks[i]
		reqs[i] = signReq{URL: src, PermissionRecord: map[string]any{"table": b.Table, "id": b.ID, "spaceId": b.SpaceID}}
	}
	var resp struct {
		SignedURLs []string `json:"signedUrls"`
	}
	if err := s.C.Call(ctx, "getSignedFileUrls", map[string]any{"urls": reqs}, &resp); err != nil {
		return nil, err
	}
	if len(resp.SignedURLs) != len(srcs) {
		return nil, fmt.Errorf("getSignedFileUrls returned %d URLs for %d files", len(resp.SignedURLs), len(srcs))
	}
	return resp.SignedURLs, nil
}

// FileURL resolves a file source to something downloadable.
func (s *Store) FileURL(ctx context.Context, owner *Record, src string) (string, error) {
	src = AbsoluteSource(src)
	if !NeedsSigning(src) {
		return src, nil
	}
	urls, err := s.SignedURLs(ctx, []*Record{owner}, []string{src})
	if err != nil {
		return "", err
	}
	if urls[0] == "" {
		// Fall back to the cookie-authenticated image proxy.
		return fmt.Sprintf("https://www.notion.so/image/%s?table=%s&id=%s&spaceId=%s", url.QueryEscape(src), owner.Table, owner.ID, owner.SpaceID), nil
	}
	return urls[0], nil
}

// FileName guesses a file name for a source.
func FileName(src string, fallback string) string {
	if strings.HasPrefix(src, "attachment:") {
		parts := strings.SplitN(src, ":", 3)
		if len(parts) == 3 && parts[2] != "" {
			return parts[2]
		}
	}
	if u, err := url.Parse(src); err == nil {
		if base := path.Base(u.Path); base != "" && base != "/" && base != "." {
			if dec, err := url.PathUnescape(base); err == nil {
				return dec
			}
			return base
		}
	}
	return fallback
}

// BlockFileSource returns the file/image source of a media block.
func BlockFileSource(b *Record) string {
	if src := PlainText(b.Prop("source")); src != "" {
		return src
	}
	return str(b.Format()["display_source"])
}
