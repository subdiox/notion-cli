package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/subdiox/notion-cli/internal/notion"
)

// crawlOpts configure `fetch --recursive`.
type crawlOpts struct {
	out         string
	concurrency int
	maxPages    int
	rows        bool // descend into database rows
}

type crawlJob struct {
	id    string
	depth int
	seq   int
	seed  []*notion.Record // records already known (the block itself, its parent, collection)
}

type crawlResult struct {
	seq   int
	id    string
	title string
	kind  string
	depth int
	md    []byte
	err   error
}

// sharedCache holds read-only records (users, collections, ...) that workers
// reuse so they are not refetched for every page.
type sharedCache struct {
	mu   sync.Mutex
	recs map[string]*notion.Record
}

func (c *sharedCache) seed(st *notion.Store) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.recs {
		st.Records.Put(r)
	}
}

func (c *sharedCache) absorb(st *notion.Store) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, table := range []string{"notion_user", "collection", "team", "space"} {
		for id, r := range st.Records[table] {
			if r.Value != nil {
				c.recs[table+":"+id] = r
			}
		}
	}
}

var unsafeName = regexp.MustCompile(`[/\\:*?"<>|\x00-\x1f]+`)

func fileNameFor(title, id string) string {
	t := strings.TrimSpace(unsafeName.ReplaceAllString(title, "_"))
	if r := []rune(t); len(r) > 60 {
		t = string(r[:60])
	}
	if t == "" {
		t = "Untitled"
	}
	return t + "_" + notion.CompactID(id)[:8] + ".md"
}

// fork returns an env that shares the client (and its rate-limit state) but
// has its own record store and output buffer.
func (e *env) fork(out *bytes.Buffer) *env {
	c := *e.client
	return &env{ctx: e.ctx, client: &c, cfg: e.cfg, ws: e.ws, out: out}
}

type crawler struct {
	e     *env
	o     fetchOpts
	co    crawlOpts
	cache *sharedCache

	mu      sync.Mutex
	seen    map[string]bool
	seq     int
	pending sync.WaitGroup
	jobs    chan crawlJob
}

func (c *crawler) enqueue(id string, depth int, seed ...*notion.Record) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen[id] || (c.co.maxPages > 0 && c.seq >= c.co.maxPages) {
		return
	}
	c.seen[id] = true
	c.seq++
	c.pending.Add(1)
	j := crawlJob{id: id, depth: depth, seq: c.seq, seed: seed}
	// Never block a worker on a full queue.
	select {
	case c.jobs <- j:
	default:
		go func() { c.jobs <- j }()
	}
}

func (c *crawler) run(j crawlJob) (res crawlResult) {
	res = crawlResult{seq: j.seq, id: j.id, depth: j.depth}
	var buf bytes.Buffer
	w := c.e.fork(&buf)
	st := w.store()
	c.cache.seed(st)
	for _, r := range j.seed {
		if r != nil {
			st.Records.Put(r)
		}
	}
	defer func() {
		c.cache.absorb(st)
		res.title = st.PageTitle(j.id)
		res.md = buf.Bytes()
	}()

	blk, err := st.Get(c.e.ctx, "block", j.id)
	if err != nil || blk == nil {
		res.err = fmt.Errorf("%s: not accessible (%v)", j.id, err)
		return
	}
	switch blk.Type() {
	case "collection_view_page", "collection_view":
		res.kind = "database"
		oo := c.o
		oo.rows = 1 << 20
		if err := w.printDatabase(blk, "", oo); err != nil {
			res.err = err
			return
		}
		if c.co.rows {
			coll, _, _ := st.CollectionFor(c.e.ctx, blk)
			for _, rid := range w.lastRowIDs {
				c.enqueue(rid, j.depth+1, coll, st.Records.Block(rid), blk)
			}
		}
	default:
		res.kind = "page"
		if err := w.printPage(blk, c.o); err != nil {
			res.err = err
			return
		}
		page := st.Records.Block(blk.ID)
		// Discover sub-pages and databases anywhere in the page tree.
		var walk func(id string, visited map[string]bool)
		walk = func(id string, visited map[string]bool) {
			if visited[id] {
				return
			}
			visited[id] = true
			b := st.Records.Block(id)
			if b == nil {
				return
			}
			for _, cid := range b.Content() {
				cb := st.Records.Block(cid)
				if cb == nil || !cb.Alive() {
					continue
				}
				switch cb.Type() {
				case "page", "collection_view_page", "collection_view":
					c.enqueue(cid, j.depth+1, cb, page)
				default:
					walk(cid, visited)
				}
			}
			if b.Type() == "transclusion_reference" {
				if p, ok := b.Format()["transclusion_reference_pointer"].(map[string]any); ok {
					walk(fmt.Sprint(p["id"]), visited)
				}
			}
		}
		walk(blk.ID, map[string]bool{})
	}
	return
}

// crawl reads root and everything below it: sub-pages, databases, every
// database row and their sub-pages. Pages are fetched concurrently.
func (e *env) crawl(root *notion.Record, o fetchOpts, co crawlOpts) error {
	start := time.Now()
	c := &crawler{e: e, o: o, co: co, cache: &sharedCache{recs: map[string]*notion.Record{}},
		seen: map[string]bool{root.ID: true}, jobs: make(chan crawlJob, 4096)}
	n := co.concurrency
	if n <= 0 {
		n = 8
	}
	isTTY := false
	if fi, err := os.Stderr.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		isTTY = true
	}

	var rmu sync.Mutex
	var all []crawlResult
	for i := 0; i < n; i++ {
		go func() {
			for j := range c.jobs {
				r := c.run(j)
				rmu.Lock()
				all = append(all, r)
				count := len(all)
				rmu.Unlock()
				if isTTY {
					fmt.Fprintf(os.Stderr, "\r%d pages read…", count)
				} else if count%100 == 0 {
					fmt.Fprintf(os.Stderr, "%d pages read (%s)\n", count, time.Since(start).Round(time.Second))
				}
				c.pending.Done()
			}
		}()
	}
	c.pending.Add(1)
	c.jobs <- crawlJob{id: root.ID, seq: 0, seed: []*notion.Record{root}}
	c.pending.Wait()
	close(c.jobs)
	if isTTY {
		fmt.Fprintln(os.Stderr)
	}

	sort.Slice(all, func(i, j int) bool { return all[i].seq < all[j].seq })
	// Order the output as a tree (parents before children, discovery order).
	errs := 0
	if co.out != "" {
		if err := os.MkdirAll(co.out, 0o755); err != nil {
			return err
		}
		var index strings.Builder
		fmt.Fprintf(&index, "# %s\n\n", all[0].title)
		for _, r := range all {
			if r.err != nil {
				errs++
				fmt.Fprintf(&index, "- ⚠️ %s: %v\n", r.id, r.err)
				continue
			}
			name := fileNameFor(r.title, r.id)
			if err := os.WriteFile(filepath.Join(co.out, name), r.md, 0o644); err != nil {
				return err
			}
			kind := ""
			if r.kind == "database" {
				kind = " (database)"
			}
			fmt.Fprintf(&index, "- [%s](%s)%s — depth %d\n", notion.EscapeMarkdown(r.title), filepath.ToSlash(name), kind, r.depth)
		}
		if err := os.WriteFile(filepath.Join(co.out, "index.md"), []byte(index.String()), 0o644); err != nil {
			return err
		}
	} else {
		for i, r := range all {
			if r.err != nil {
				errs++
				fmt.Fprintf(os.Stderr, "warning: %v\n", r.err)
				continue
			}
			if i > 0 {
				fmt.Printf("\n<!-- ===== %s %s ===== -->\n", r.kind, notion.PageURL(r.id))
			}
			os.Stdout.Write(r.md)
		}
	}
	fmt.Fprintf(os.Stderr, "read %d pages/databases (%d errors) in %s\n", len(all)-errs, errs, time.Since(start).Round(time.Millisecond))
	if errs > 0 {
		return fmt.Errorf("%d pages could not be read", errs)
	}
	return nil
}
