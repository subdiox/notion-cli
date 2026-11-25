---
name: notion
description: Read and write Notion (pages, databases, comments, files) with the `notion` CLI, including workspaces where the user is a guest. Use whenever the user mentions Notion, pastes a notion.so / notion.com URL, or asks to find, summarize, create or edit Notion content.
---

# Notion via `notion` CLI

The `notion` CLI uses the user's own Notion session, so it sees exactly what the
user sees in the app — all accounts and workspaces, including guest workspaces.
Prefer it over the Notion MCP server.

## Find things

- `notion search "query" -a` — search all workspaces (omit `-a` for the current one).
  Add `--titles` for title-only matches, `--sort edited` for recent pages, `--in <page>` to scope.
- `notion space ls` / `notion space pages` — workspaces and their top-level pages.
- URLs and IDs work everywhere; the right account is picked automatically.

## Read

- `notion fetch <url>` — page as YAML front matter + Notion-flavored Markdown.
  Databases print schema, views and rows.
- Images: `notion fetch <url> --download-assets <dir>` then read the downloaded files.
- `notion db query <db> -w "Status=Done" -w "Due>=today" --sort Due:desc -n 200 [--json]`.
  Operators: `= != ~ > < >= <= ?(not empty) !?(empty)`.
- `notion comment ls <page>`, `notion block ls <page>` (block IDs for targeted edits).

## Write

- Edit a page: `notion fetch <url> > /tmp/p.md`, edit the file, then
  `notion page update <url> -f /tmp/p.md`. This is a minimal diff that keeps block IDs.
  Keep the `<page url=…>`/`<database url=…>` lines you are not changing.
- New page: `notion page create --parent <page-or-db> -t "Title" -f body.md`
  (`-p Prop=value` for database properties; local image paths are uploaded).
- Append: `notion page append <page> --md "…"`; single block: `notion block update <id> --md "…"`.
- Properties: `notion db row update <row> -p Status=Done -p Tags=a,b -p Owner=me -p Due=2025-02-01`.
- Comments: `notion comment add <page|block> "text"`, `notion comment reply <discussion-id> "text"`.
- Use `--dry-run` first for large or destructive changes. `notion page trash` is reversible with `notion page restore`.

## Markdown format (same for reading and writing)

Headings `#`..`###` (`{toggle="true" color="…"}`), lists with tab-indented children,
`- [ ]` to-dos, `> quote`, `<details><summary>`, `<callout icon="…">`, fenced code,
`$$` equations, `<table header-row="true"><tr><td>`, `<columns><column>`,
`![cap](src)`, `<file src>`, `<bookmark url>`, `<page url>`, `<mention-page url>`,
`<mention-user url="user://id">`, `<mention-date start="…"/>`, `<span color="red">`,
`<empty-block/>`.

Add `--json` to any command for machine-readable output.
