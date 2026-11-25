# notion-cli

A single-binary Notion CLI (Go) that talks to Notion's internal `/api/v3` API with
the session of the **Notion desktop app or your browser** (`token_v2`). It can
therefore read and write everything you can in the app — including
**workspaces where you are only a guest**, which the official API/MCP cannot.

Built for AI agents (Claude Code etc.): pages are read and written as
Notion-flavored Markdown, every command has `--json`, and edits are applied as a
minimal diff so block IDs, comments and links survive.

> ⚠️ This uses Notion's undocumented internal API. It can break when Notion
> changes, and automating your user session may be against Notion's terms.
> Use it for your own data.

## Install

```sh
go install github.com/subdiox/notion-cli/cmd/notion@latest
# or
go build -o ~/bin/notion ./cmd/notion
```

## Login

```sh
notion auth import                 # auto: desktop app, then Chrome/Arc/Brave/Edge/Vivaldi/Chromium/Firefox
notion auth import --from chrome   # a specific source
notion auth set-token <token_v2>   # manual (e.g. Linux/Windows; copy the cookie from devtools)
notion auth status                 # logged-in accounts (multi-account sessions are supported)
```

Cookie import is macOS-only for now. Chromium-based sources read their
"Safe Storage" key from the keychain, so macOS asks for permission once.
The session is stored in `~/.config/notion-cli/session.json` (0600).
`NOTION_TOKEN_V2` / `NOTION_CLI_HOME` override it.

## Workspaces and accounts

```sh
notion space ls                    # every workspace of every account, incl. page_guest ones
notion space use "Acme Inc"          # default workspace (name, domain or ID)
notion space pages                 # top-level pages: teamspaces, favorites, private, shared-with-you
notion search "roadmap"            # in the current workspace
notion search -a "roadmap"         # across all workspaces
```

You rarely need `space use`: any command given a URL/ID automatically picks the
logged-in account that can access it. `--space` / `--user` override per command.

## Reading

```sh
notion fetch <url|id>                      # page → front matter + Markdown body
notion fetch <url> --download-assets ./img # also download images/files, link local paths
notion fetch <url> --signed-urls           # temporary URLs for Notion-hosted files
notion fetch <database-url>                # schema, views and the first rows
notion fetch <url> -r -o ./dump            # EVERYTHING below: sub-pages, databases, every row's content
                                           #   (concurrent; ~650 pages / 90 DBs / 1,800 rows in ~35 s)
notion db query <db> -w "Status=Done" -w "Due<=today" --sort Due:desc -n 200
notion block ls <page>                     # child blocks with IDs
notion comment ls <page>
notion file download <block> -o ./out/
notion export <page> -r --format markdown  # Notion's own exporter (computes formulas in CSV)
notion user ls
```

## Writing

```sh
notion page create --parent <page|db> -t "Title" -f body.md   # local images in body.md are uploaded
notion page update <page> -f edited.md     # minimal diff; front matter updates title/properties
notion page update <row> -p Status=Done -p Tags=a,b -p Due=2025-01-31 -p Owner=me
notion page append <page> --md "- new item"
notion page move <page> --to <page|db> [--after <block>]
notion page trash <page> / notion page restore <page>
notion page duplicate <page> [--to <page>]

notion block update <block> --md "New **text**"
notion block append <block> --md "child"   # --after <sibling>, --prepend
notion block rm <block>

notion db create --parent <page> -t Tasks -p "Status:status" -p "Tags:multi_select=bug,feature" -p "Due:date"
notion db row add <db> -t "Fix login" -p Status=Todo
notion db prop add <db> "Project:relation=<other-db>" "Estimate:number"
notion db prop update <db> Tags --rename Labels --add-option urgent
notion db prop rm <db> Estimate

notion comment add <page|block> "Looks good **to me**"
notion comment reply <discussion-id> "thanks"
notion comment resolve <discussion-id>

notion file upload ./diagram.png --to <page>
```

Every write command accepts `--dry-run` to print the transaction instead of sending it.
`notion raw <endpoint> '<json>'` calls any internal endpoint directly.

### Agent workflow

```sh
notion fetch <page> > page.md      # edit page.md
notion page update <page> -f page.md
```

Unchanged blocks are untouched, edited blocks are updated in place (keeping their
IDs), and only real insertions/deletions are created/removed. Sub-pages and
databases missing from the Markdown are kept unless `--allow-delete-pages`.

## Notion-flavored Markdown

| Notion | Markdown |
| --- | --- |
| headings (toggle, color) | `# H1`, `## H2`, `### H3`, `# H {toggle="true" color="red"}` |
| lists / to-do | `- a`, `1. a`, `- [ ] a`, `- [x] a`; children indented with a tab |
| quote | `> text` (`<br>` for line breaks) |
| toggle | `<details><summary>Title</summary>` … `</details>` |
| callout | `<callout icon="💡" color="gray_background">` … `</callout>` |
| code / equation / divider | fenced code, `$$ … $$`, `---` |
| table | `<table header-row="true"><tr><td>…</td></tr></table>` or a GFM pipe table |
| columns | `<columns><column width="0.5">…</column></columns>` |
| media | `![caption](url-or-local-path)`, `<file src="…">`, `<pdf>`, `<video>`, `<audio>` |
| bookmark / embed | `<bookmark url="…">title</bookmark>`, `<embed src="…">` |
| sub-page / database | `<page url="…">Title</page>`, `<database url="…" inline="true">Title</database>` |
| link to page | `<link-to-page url="…">Title</link-to-page>` |
| synced block | `<synced_block url="…">…</synced_block>`, `<synced_block_reference url="…">` |
| misc | `<table_of_contents/>`, `<breadcrumb/>`, `<empty-block/>` |
| inline | `**b**`, `*i*`, `~~s~~`, `` `c` ``, `[l](u)`, `` $`x^2`$ ``, `<span color="red">`, `<span underline="true">` |
| mentions | `<mention-page url="…">`, `<mention-user url="user://id">`, `<mention-date start="2025-01-01" end="…"/>` |

New pages: a `<page>Title</page>` without `url` creates a sub-page.

## Limitations

- Formula / rollup values are computed client-side by Notion; `fetch` shows them
  only when cached. Use `notion export` for computed values.
- Database views (filters, sorts, layouts) can be read and used for queries, but not edited
  (except via `raw`).
- Pages cannot be created directly at the top level of a teamspace (create under a page,
  or as a private page and move).
- Comments are page/block-level; comments anchored to a text selection are read-only.
- Moving pages between workspaces is not supported.

## Development

```sh
go test ./...
notion --debug <command>   # logs every API request/response
```

## License

[MIT](LICENSE)
