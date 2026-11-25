# CLAUDE.md

Go CLI (`cmd/notion`) for Notion's **internal** `/api/v3` API, authenticated with the
user's `token_v2` session cookie. Primary users are AI agents: Markdown output by default,
`--json` everywhere, minimal-diff writes.

## Layout

- `internal/auth` — cookie import. Chromium-format stores (Notion desktop app = Electron,
  `~/Library/Application Support/Notion/Partitions/notion/Cookies`, keychain item
  "Notion Safe Storage"; Chrome/Arc/Brave/Edge/...) and Firefox.
- `internal/api` — HTTP client (`Call`, `Raw`, `Download`), retries on 429/5xx.
- `internal/notion` — record model, `Store` (record cache), Markdown renderer
  (`render.go`) and parser (`mdblock.go`, `mdinline.go`), `Writer` (create / diff-sync),
  `Tx` (transaction ops), collections and property encoding.
- `internal/cli` — cobra commands. `env.target(ref, table)` resolves a URL/ID and picks the
  logged-in account that can access it.

## Development

- `go test ./...` — includes an offline round-trip test (Markdown → ops → in-memory
  records → Markdown) that applies `Tx` ops locally. Keep it passing when touching the
  renderer or parser: **render and parse must stay exact inverses**, because diff sync
  compares existing blocks by rendering them and parsing the result back.
- `notion --debug <cmd>` logs every request/response; `notion raw <endpoint> '<json>'`
  probes endpoints. Use a scratch `NOTION_CLI_HOME` (e.g. `.dev/`, gitignored).
- Use `--dry-run` to inspect generated operations before sending writes.
- Never commit real tokens, user IDs, page IDs or workspace names (tests use fake UUIDs).

## Internal API knowledge (verified)

- Cookies now live on `app.notion.com`; `https://app.notion.com/api/v3/...` works.
  Canonical page URLs: `https://www.notion.so/<32hex>` (also `app.notion.com/p/<32hex>`).
- Headers: `x-notion-active-user-header` (user ID) selects the account in a multi-account
  session; `x-notion-space-id` selects the workspace.
- Record shape: `{spaceId, value: {role, value: {...}}}`. Inaccessible records come back
  as `{value: {role: "none"}}` — used to auto-select the right account.
- `syncRecordValues` works without `spaceId` in the pointer and returns it.
- `getSpaces` → per-user record maps; membership is in `space_user.membership_type`
  (`owner`, `member`, `page_guest`). Guest workspaces have no `space` record: use
  `getPublicSpaceData`, which **only answers the first ID per request** — call per space.
- Guests' entry points: `getUserSharedPagesInSpace {spaceId}`. Members: `team.pages`,
  `space_view.private_pages` / `bookmarked_pages`.
- `getVisibleUsers {spaceId}` lists workspace users (`getSpaceMembers` does not exist).
- `search` highlight text contains random tags like `<gzkNfoUU>`; strip them.
- Writes: `saveTransactionsFanout {requestId, transactions:[{id, spaceId, debug, operations}]}`.
  Plain `set` / `update` / `listAfter` / `listRemove` work for blocks, comments and
  discussions; setting `properties.title` is fine even though blocks now carry `crdt_data`.
- **Collection schema changes are rejected as plain `set`/`update` (400 ValidationError).**
  Use `updateCollectionPropertySchema` with
  `args: {primitiveOp: {command, args}}`, e.g. path `["schema", id]` + `update` (add),
  `["schema", id, "name"]` + `set` (rename), `["schema", id, "options"]` +
  `keyedObjectListAfter {value}` / `set`. Delete with `softDeleteOrReinstateCollectionProperty`,
  path `["deleted_schema", id]`, args `{propertySchema}`. Creating a whole new collection
  with a plain `set` is fine.
- **Relation values** must use `updateBlockPropertyValue` with primitive ops
  `addRelationAfter {id, spaceId}` / `removeRelation {id, spaceId}`; a plain `set` fails.
- Uploads: `getUploadFileUrl {bucket:"secure", name, contentType, record, contentLength}` →
  PUT to `signedPutUrl` → store `url` (`attachment:<fileId>:<name>`) in `properties.source`
  and `file_ids`. Download via `getSignedFileUrls {urls:[{url, permissionRecord}]}`.
- Async jobs (`exportBlock`, `duplicateBlock`): `enqueueTask` then poll `getTasks`.
- When an operation's shape is unknown, grep the web client's JS chunks
  (`https://app.notion.com/_assets/*.js`, listed in the `/login` HTML) for the command name.
