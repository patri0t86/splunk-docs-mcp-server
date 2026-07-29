package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
	"gopkg.in/yaml.v3"
)

type frontmatter struct {
	URL          string   `yaml:"url"`
	Title        string   `yaml:"title"`
	Product      string   `yaml:"product"`
	Version      string   `yaml:"version"`
	Breadcrumbs  []string `yaml:"breadcrumbs"`
	LastModified string   `yaml:"last_modified"`
}

// breadcrumb flattens the frontmatter breadcrumbs list into the single
// "A > B > C" string stored in documents.breadcrumb.
func (fm frontmatter) breadcrumb() string {
	return strings.Join(fm.Breadcrumbs, " > ")
}

// sourceUpdated parses the docs site's own last-modified stamp. The corpus
// carries both RFC 3339 timestamps (help.splunk.com) and bare dates
// (lantern.splunk.com); anything else is treated as absent rather than
// failing the document.
func (fm frontmatter) sourceUpdated() *time.Time {
	value := strings.TrimSpace(fm.LastModified)
	if value == "" || value == "null" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, value); err == nil {
			return &t
		}
	}
	return nil
}

type chunk struct {
	heading string
	level   int // 2 or 3 for h2/h3 chunks, 0 for the heading-less intro chunk
	content string
}

type options struct {
	model        string
	ollamaURL    string
	parseWorkers int
	embedWorkers int
	writeWorkers int
	batchSize    int
}

type document struct {
	path       string
	fm         frontmatter
	hash       string
	chunks     []chunk
	embeddings [][]float32
	// metaOnly documents are already ingested with identical content and
	// only need their derived metadata (manual, canonical id, aliases,
	// section ids, literal tsvector) refreshed -- no embedding work.
	metaOnly bool
}

// key is the stable, reindex-surviving identity of a document, used as the
// prefix of every section_id it owns. It is derived from the URL rather than
// from a SERIAL column so section ids survive a full re-ingest.
func (d *document) key() string {
	return hashContent(d.fm.URL)[:16]
}

type parseResult struct {
	doc *document
	err error
}

type documentState struct {
	doc       *document
	remaining atomic.Int64
	mu        sync.Mutex
	err       error
}

type embedJob struct {
	state *documentState
	start int
	end   int
}

type outcome struct {
	url        string
	product    string
	chunks     int
	unchanged  bool
	backfilled bool
	err        error
}

type ollamaClient struct {
	url    string
	model  string
	client *http.Client
}

// embedCache deduplicates embedding work across documents. The corpus holds
// near-identical copies of every page across major versions, so most chunks
// produce byte-identical embed inputs; computing each unique input once cuts
// the dominant (embedding) cost roughly by the number of version copies.
// Vectors are stored once and shared read-only between documents.
type embedCache struct {
	mu      sync.RWMutex
	vectors map[string][]float32
	hits    atomic.Int64
}

func newEmbedCache() *embedCache {
	return &embedCache{vectors: make(map[string][]float32)}
}

func (c *embedCache) get(key string) ([]float32, bool) {
	c.mu.RLock()
	vec, ok := c.vectors[key]
	c.mu.RUnlock()
	if ok {
		c.hits.Add(1)
	}
	return vec, ok
}

func (c *embedCache) put(key string, vec []float32) {
	c.mu.Lock()
	c.vectors[key] = vec
	c.mu.Unlock()
}

func (c *embedCache) stats() (hits int64, unique int) {
	c.mu.RLock()
	unique = len(c.vectors)
	c.mu.RUnlock()
	return c.hits.Load(), unique
}

var headingRe = regexp.MustCompile(`^(#{2,3})\s+(.+)$`)

var h1Re = regexp.MustCompile(`^#\s`)

var versionDirRe = regexp.MustCompile(`^\d+(\.\d+)+$`)

// pathVersion returns the first path segment under root that looks like a
// version directory ("10.4", "9.4.13"), or "" if there is none.
func pathVersion(root, path string) []int {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return nil
	}
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		if versionDirRe.MatchString(seg) {
			parts := strings.Split(seg, ".")
			nums := make([]int, len(parts))
			for i, p := range parts {
				fmt.Sscanf(p, "%d", &nums[i])
			}
			return nums
		}
	}
	return nil
}

func compareVersions(a, b []int) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] - b[i]
		}
	}
	return len(a) - len(b)
}

// pathProduct returns the product directory: the first path segment under
// root, or "" when root is itself a product directory.
func pathProduct(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return ""
	}
	segs := strings.Split(rel, string(filepath.Separator))
	if len(segs) < 2 || versionDirRe.MatchString(segs[0]) {
		return ""
	}
	return segs[0]
}

// filterLatestPatch drops files under patch-version directories (x.y.z)
// superseded by a newer patch of the same product and minor (x.y). The patch
// directories hold the per-patch configuration-file references, which are
// near-identical between patches; indexing all of them floods search results
// with clones of the same chunk. Minor-version directories ("10.4") are
// always kept, and products never supersede each other's patches (Splunk
// Cloud trains like 10.4.2604 must not shadow Splunk Enterprise 10.4.x).
func filterLatestPatch(root string, paths []string) []string {
	key := func(p string, v []int) string {
		return fmt.Sprintf("%s %d.%d", pathProduct(root, p), v[0], v[1])
	}
	latest := map[string][]int{}
	for _, p := range paths {
		v := pathVersion(root, p)
		if len(v) < 3 {
			continue
		}
		if cur, ok := latest[key(p, v)]; !ok || compareVersions(v, cur) > 0 {
			latest[key(p, v)] = v
		}
	}
	kept := paths[:0]
	dropped := 0
	for _, p := range paths {
		v := pathVersion(root, p)
		if len(v) >= 3 && compareVersions(v, latest[key(p, v)]) != 0 {
			dropped++
			continue
		}
		kept = append(kept, p)
	}
	if dropped > 0 {
		fmt.Printf("skipping %d files in superseded patch-version directories (use -all-versions to keep them)\n", dropped)
	}
	return kept
}

// parseFrontmatter splits a "---\n...yaml...\n---\nbody" file into its parts.
func parseFrontmatter(raw []byte) (frontmatter, string, error) {
	s := string(raw)
	if !strings.HasPrefix(s, "---\n") {
		return frontmatter{}, "", fmt.Errorf("no frontmatter found")
	}
	parts := strings.SplitN(s[4:], "\n---\n", 2)
	if len(parts) != 2 {
		return frontmatter{}, "", fmt.Errorf("malformed frontmatter")
	}
	var fm frontmatter
	if err := yaml.Unmarshal([]byte(parts[0]), &fm); err != nil {
		return frontmatter{}, "", err
	}
	return fm, strings.TrimSpace(parts[1]), nil
}

// splitChunks breaks the body on h2/h3 headings. Content before the first
// heading — the page intro, typically its most retrieval-relevant text —
// becomes a heading-less chunk; the h1 line is dropped from it because the
// title is stored on the document and prepended by embedInput. Heading-like
// lines inside fenced code blocks (conf-file samples and the like) are not
// chunk boundaries. A page with no headings yields one heading-less chunk.
func splitChunks(body string) []chunk {
	var chunks []chunk
	var cur []string
	heading, level := "", 0
	inFence := false
	flush := func() {
		if content := strings.TrimSpace(strings.Join(cur, "\n")); content != "" {
			chunks = append(chunks, chunk{heading: heading, level: level, content: content})
		}
		cur = cur[:0]
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") {
			inFence = !inFence
		} else if !inFence {
			if m := headingRe.FindStringSubmatch(line); m != nil {
				flush()
				heading, level = m[2], len(m[1])
				continue
			}
			if heading == "" && h1Re.MatchString(line) {
				continue
			}
		}
		cur = append(cur, line)
	}
	flush()
	return chunks
}

func hashContent(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// hashVersion salts content hashes so that changes to the embedding input
// format or tsv weighting force re-ingestion of otherwise unchanged files.
// Bump it whenever embedInput, splitChunks, or the tsv UPDATE in
// writeDocument changes. v3: intro chunks, fence-aware splitting, heading
// levels, product column, num_ctx 8192.
const hashVersion = "v3"

// embedInput builds the text that gets embedded for chunk i. The title and
// heading are included because chunk bodies often lack the words that
// identify them (a spec list under a "Mid-range indexer specification"
// heading never says "mid-range"). nomic-embed-text is trained with task
// prefixes: documents use "search_document:", queries "search_query:".
func (d *document) embedInput(i int) string {
	c := d.chunks[i]
	head := d.fm.Title
	if c.heading != "" {
		head += " > " + c.heading
	}
	return "search_document: " + head + "\n" + c.content
}

func newOllamaClient(url, model string, workers int) *ollamaClient {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = workers * 2
	transport.MaxIdleConnsPerHost = workers
	transport.MaxConnsPerHost = workers
	return &ollamaClient{
		url:   strings.TrimRight(url, "/"),
		model: model,
		client: &http.Client{
			Transport: transport,
			Timeout:   10 * time.Minute,
		},
	}
}

func (c *ollamaClient) embed(ctx context.Context, inputs []string) ([][]float32, error) {
	// num_ctx 8192 matches nomic-embed-text's trained context; Ollama's
	// default (2048) silently truncates long chunks such as conf references,
	// leaving only their head represented in the embedding.
	payload, err := json.Marshal(struct {
		Model     string         `json:"model"`
		Input     []string       `json:"input"`
		KeepAlive string         `json:"keep_alive"`
		Options   map[string]any `json:"options"`
	}{c.model, inputs, "30m", map[string]any{"num_ctx": 8192}})
	if err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/api/embed", bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.client.Do(req)
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				return nil, readErr
			}
			if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
				var result struct {
					Embeddings [][]float32 `json:"embeddings"`
				}
				if err := json.Unmarshal(body, &result); err != nil {
					return nil, err
				}
				if len(result.Embeddings) != len(inputs) {
					return nil, fmt.Errorf("Ollama returned %d embeddings for %d inputs", len(result.Embeddings), len(inputs))
				}
				return result.Embeddings, nil
			}
			lastErr = fmt.Errorf("Ollama returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
			if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < http.StatusInternalServerError {
				return nil, lastErr
			}
		} else {
			lastErr = err
		}
		if attempt < 3 {
			select {
			case <-time.After(time.Duration(1<<attempt) * 250 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return nil, lastErr
}

func (c *ollamaClient) gpuResident(ctx context.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url+"/api/ps", nil)
	if err != nil {
		return false, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return false, fmt.Errorf("Ollama returned %s from /api/ps", resp.Status)
	}
	var result struct {
		Models []struct {
			Name     string `json:"name"`
			Model    string `json:"model"`
			SizeVRAM int64  `json:"size_vram"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, err
	}
	for _, model := range result.Models {
		if (model.Name == c.model || model.Model == c.model || strings.HasPrefix(model.Name, c.model+":")) && model.SizeVRAM > 0 {
			return true, nil
		}
	}
	return false, nil
}

func parseDocument(path string) (*document, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fm, body, err := parseFrontmatter(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if fm.URL == "" {
		return nil, fmt.Errorf("%s: frontmatter has no URL", path)
	}
	// Title is part of the hash because it feeds embedInput and the tsv.
	hash := hashContent(hashVersion + "\x00" + fm.Title + "\x00" + body)
	return &document{path: path, fm: fm, hash: hash, chunks: splitChunks(body)}, nil
}

// urlProduct returns the product slug for known Splunk docs hosts.
func urlProduct(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	switch strings.ToLower(u.Hostname()) {
	case "lantern.splunk.com":
		return "splunk-lantern"
	case "dev.splunk.com":
		return "splunk-dev"
	case "splunkui.splunk.com":
		return "splunk-ui"
	case "help.splunk.com":
		path := strings.Trim(u.Path, "/")
		parts := strings.Split(path, "/")
		if len(parts) < 2 || parts[0] != "en" {
			return ""
		}
		return parts[1]
	default:
		return ""
	}
}

// versionSegRe matches the version segment of a help.splunk.com path: a
// dotted release number ("10.4", "9.4.13", "10.5.2605") or the moving
// "latest" alias.
var versionSegRe = regexp.MustCompile(`^(latest|\d+(\.\d+)*)$`)

// docPathSegments returns the meaningful path segments of a documentation
// URL: for help.splunk.com the "/en/<product>" prefix is stripped, for the
// single-product hosts the whole path is meaningful.
func docPathSegments(rawURL string) []string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	path := strings.Trim(u.Path, "/")
	if path == "" {
		return nil
	}
	segs := strings.Split(path, "/")
	if strings.EqualFold(u.Hostname(), "help.splunk.com") && len(segs) >= 2 && segs[0] == "en" {
		segs = segs[2:]
	}
	return segs
}

// docLocation derives the manual a page belongs to and its canonical,
// version-independent identity.
//
// The corpus holds a near-identical copy of nearly every page for every
// product version, and hundreds of distinct pages share a title
// ("Troubleshooting", "Prerequisites", "Fixed issues"). Collapsing version
// copies therefore has to key on page identity, not on the title: two
// unrelated "Troubleshooting" pages must not suppress each other, while the
// 10.2 and 10.4 copies of one page must collapse into one result.
//
// help.splunk.com paths are ".../<category>/<manual>/<version>/<page path>",
// so the manual is the segment before the version and the canonical path is
// everything else with the version segment removed. lantern/dev/ui pages are
// unversioned, so the whole path is canonical and the first segment is the
// closest thing they have to a manual.
func docLocation(rawURL, product, version string) (manual, canonicalID string) {
	segs := docPathSegments(rawURL)
	if len(segs) == 0 {
		return "", ""
	}
	versionAt := -1
	for i, seg := range segs {
		if seg == version || versionSegRe.MatchString(seg) {
			versionAt = i
			break
		}
	}
	canonical := segs
	if versionAt >= 0 {
		canonical = append(append([]string{}, segs[:versionAt]...), segs[versionAt+1:]...)
		if versionAt > 0 {
			manual = segs[versionAt-1]
		}
	}
	if manual == "" {
		manual = canonical[0]
	}
	if product == "" {
		product = urlProduct(rawURL)
	}
	return manual, strings.ToLower(product + "/" + strings.Join(canonical, "/"))
}

var aliasCleanRe = regexp.MustCompile(`[^a-z0-9]+`)

// normalizeAlias lowercases and strips every non-alphanumeric character.
// help.splunk.com cross-references pages as ?resourceId=Splunk_X_Topicid,
// where the topic id is the page slug with its punctuation removed
// ("Referencehardware" for "reference-hardware"), so stripping punctuation on
// both sides makes the two forms compare equal.
func normalizeAlias(s string) string {
	return aliasCleanRe.ReplaceAllString(strings.ToLower(s), "")
}

// documentAliases returns the lookup keys under which a page can be found by
// a URL that is not itself indexed. Storing these lets the server resolve
// cross-reference links with an exact index lookup instead of a substring
// scan over the url column, which could silently match the wrong page for a
// generic slug.
func documentAliases(rawURL, canonicalID string) []string {
	segs := docPathSegments(rawURL)
	seen := map[string]bool{}
	var aliases []string
	add := func(a string) {
		if a != "" && !seen[a] {
			seen[a] = true
			aliases = append(aliases, a)
		}
	}
	if len(segs) > 0 {
		add("slug:" + normalizeAlias(segs[len(segs)-1]))
	}
	if canonicalID != "" {
		add("path:" + normalizeAlias(canonicalID))
	}
	return aliases
}

// pruneStale deletes documents (chunks follow via cascade) whose URLs were
// not seen in the current run. Scoped to the products the run actually saw
// on disk, so indexing one product's directory never deletes another's rows.
func pruneStale(ctx context.Context, pool *pgxpool.Pool, liveURLs []string) (int64, error) {
	products := map[string]bool{}
	for _, u := range liveURLs {
		if p := urlProduct(u); p != "" {
			products[p] = true
		}
	}
	slugs := make([]string, 0, len(products))
	for p := range products {
		slugs = append(slugs, p)
	}
	tag, err := pool.Exec(ctx, `
		DELETE FROM documents
		WHERE product = ANY($1)
		  AND NOT (url = ANY($2))
	`, slugs, liveURLs)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// documentRecord is the ingestion state of one already-indexed URL: its
// content hash, plus whether the derived metadata added after the initial
// release is present. A document whose content is unchanged but whose
// metadata is missing is backfilled without re-embedding it.
type documentRecord struct {
	hash   string
	metaOK bool
}

func loadHashes(ctx context.Context, pool *pgxpool.Pool) (map[string]documentRecord, error) {
	rows, err := pool.Query(ctx, `
		SELECT d.url, d.content_hash,
		       d.canonical_id <> '' AND NOT EXISTS (
		           SELECT 1 FROM chunks c
		           WHERE c.document_id = d.id
		             AND (c.section_id IS NULL OR c.tsv_simple IS NULL)
		       ) AS meta_ok
		FROM documents d
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := make(map[string]documentRecord)
	for rows.Next() {
		var url, hash string
		var metaOK bool
		if err := rows.Scan(&url, &hash, &metaOK); err != nil {
			return nil, err
		}
		records[url] = documentRecord{hash: hash, metaOK: metaOK}
	}
	return records, rows.Err()
}

// refreshChunkMetadata recomputes everything about a page's chunks that is
// derived rather than embedded: the two tsvectors and the stable section id.
//
// tsv is stemmed English, which is right for prose but destroys the
// punctuation-heavy identifiers Splunk documentation is full of; tsv_simple
// keeps props.conf, _time and tstats intact so they can be matched literally.
//
// section_id is <document key>#<heading anchor>, deduplicated within the page,
// so it stays valid across re-ingests (unlike the SERIAL chunk id, which is
// regenerated every time the page is rewritten).
func refreshChunkMetadata(ctx context.Context, tx pgx.Tx, docID int, docKey, title string) error {
	_, err := tx.Exec(ctx, `
		WITH base AS (
			SELECT c.id, c.chunk_index,
			       CASE
			           WHEN coalesce(c.heading, '') = '' THEN 'overview'
			           ELSE coalesce(nullif(trim(both '-' from
			                    regexp_replace(lower(c.heading), '[^a-z0-9]+', '-', 'g')), ''), 'section')
			       END AS anchor
			FROM chunks c
			WHERE c.document_id = $1
		),
		numbered AS (
			SELECT b.*, row_number() OVER (PARTITION BY b.anchor ORDER BY b.chunk_index) AS dup
			FROM base b
		)
		UPDATE chunks c SET
			anchor     = n.anchor,
			section_id = $3 || '#' || n.anchor ||
			             CASE WHEN n.dup > 1 THEN '-' || n.dup::text ELSE '' END,
			tsv        = setweight(to_tsvector('english', $2), 'A') ||
			             setweight(to_tsvector('english', coalesce(c.heading, '')), 'B') ||
			             setweight(to_tsvector('english', c.content), 'C'),
			tsv_simple = setweight(to_tsvector('simple', $2), 'A') ||
			             setweight(to_tsvector('simple', coalesce(c.heading, '')), 'B') ||
			             setweight(to_tsvector('simple', c.content), 'C')
		FROM numbered n
		WHERE n.id = c.id
	`, docID, title, docKey)
	return err
}

func writeAliases(ctx context.Context, tx pgx.Tx, docID int, aliases []string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM document_aliases WHERE document_id = $1`, docID); err != nil {
		return err
	}
	if len(aliases) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO document_aliases (alias, document_id)
		SELECT unnest($1::text[]), $2
		ON CONFLICT DO NOTHING
	`, aliases, docID)
	return err
}

// backfillDocument refreshes derived metadata for a page whose content has
// not changed, so schema additions don't require re-embedding the corpus.
func backfillDocument(ctx context.Context, pool *pgxpool.Pool, doc *document) error {
	manual, canonicalID := docLocation(doc.fm.URL, doc.fm.Product, doc.fm.Version)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var docID int
	err = tx.QueryRow(ctx, `
		UPDATE documents
		SET manual = $2, canonical_id = $3, source_updated_at = $4
		WHERE url = $1
		RETURNING id
	`, doc.fm.URL, manual, canonicalID, doc.fm.sourceUpdated()).Scan(&docID)
	if err != nil {
		return err
	}
	if err := writeAliases(ctx, tx, docID, documentAliases(doc.fm.URL, canonicalID)); err != nil {
		return err
	}
	if err := refreshChunkMetadata(ctx, tx, docID, doc.key(), doc.fm.Title); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func writeDocument(ctx context.Context, pool *pgxpool.Pool, doc *document) error {
	manual, canonicalID := docLocation(doc.fm.URL, doc.fm.Product, doc.fm.Version)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var docID int
	err = tx.QueryRow(ctx, `
		INSERT INTO documents (url, title, product, version, manual, canonical_id, breadcrumb, content_hash, source_updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (url) DO UPDATE
			SET title = EXCLUDED.title,
			    product = EXCLUDED.product,
			    version = EXCLUDED.version,
			    manual = EXCLUDED.manual,
			    canonical_id = EXCLUDED.canonical_id,
			    breadcrumb = EXCLUDED.breadcrumb,
			    content_hash = EXCLUDED.content_hash,
			    source_updated_at = EXCLUDED.source_updated_at,
			    updated_at = now()
		RETURNING id
	`, doc.fm.URL, doc.fm.Title, doc.fm.Product, doc.fm.Version, manual, canonicalID,
		doc.fm.breadcrumb(), doc.hash, doc.fm.sourceUpdated()).Scan(&docID)
	if err != nil {
		return err
	}
	if err := writeAliases(ctx, tx, docID, documentAliases(doc.fm.URL, canonicalID)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM chunks WHERE document_id = $1`, docID); err != nil {
		return err
	}

	copyRows := make([][]any, len(doc.chunks))
	for i, item := range doc.chunks {
		copyRows[i] = []any{docID, i, item.heading, item.level, item.content, pgvector.NewVector(doc.embeddings[i])}
	}
	if len(copyRows) > 0 {
		_, err = tx.CopyFrom(ctx, pgx.Identifier{"chunks"},
			[]string{"document_id", "chunk_index", "heading", "heading_level", "content", "embedding"},
			pgx.CopyFromRows(copyRows))
		if err != nil {
			return err
		}
		// The tsvectors are weighted so that a query matching a page title or
		// section heading outranks one that only matches body text. COPY
		// cannot compute server-side expressions, hence the separate UPDATE.
		if err := refreshChunkMetadata(ctx, tx, docID, doc.key(), doc.fm.Title); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func finishEmbedJob(ctx context.Context, job embedJob, err error, completed chan<- *document, outcomes chan<- outcome) {
	if err != nil {
		job.state.mu.Lock()
		if job.state.err == nil {
			job.state.err = err
		}
		job.state.mu.Unlock()
	}
	if job.state.remaining.Add(-1) != 0 {
		return
	}
	job.state.mu.Lock()
	docErr := job.state.err
	job.state.mu.Unlock()
	if docErr != nil {
		sendOutcome(ctx, outcomes, outcome{url: job.state.doc.fm.URL, err: docErr})
		return
	}
	select {
	case completed <- job.state.doc:
	case <-ctx.Done():
	}
}

func sendOutcome(ctx context.Context, outcomes chan<- outcome, result outcome) {
	select {
	case outcomes <- result:
	case <-ctx.Done():
	}
}

func runPipeline(ctx context.Context, pool *pgxpool.Pool, client *ollamaClient, cache *embedCache, paths []string, hashes map[string]documentRecord, opts options) <-chan outcome {
	parseJobs := make(chan string, opts.parseWorkers*2)
	parsed := make(chan parseResult, opts.parseWorkers*2)
	embedJobs := make(chan embedJob, opts.embedWorkers*2)
	completed := make(chan *document, opts.writeWorkers*2)
	outcomes := make(chan outcome, opts.parseWorkers+opts.embedWorkers+opts.writeWorkers)

	var parseWG sync.WaitGroup
	for range opts.parseWorkers {
		parseWG.Add(1)
		go func() {
			defer parseWG.Done()
			for path := range parseJobs {
				doc, err := parseDocument(path)
				select {
				case parsed <- parseResult{doc: doc, err: err}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(parseJobs)
		for _, path := range paths {
			select {
			case parseJobs <- path:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		parseWG.Wait()
		close(parsed)
	}()

	go func() {
		defer close(embedJobs)
		for result := range parsed {
			if result.err != nil {
				sendOutcome(ctx, outcomes, outcome{err: result.err})
				continue
			}
			doc := result.doc
			if record, ok := hashes[doc.fm.URL]; ok && record.hash == doc.hash {
				if record.metaOK {
					sendOutcome(ctx, outcomes, outcome{url: doc.fm.URL, product: doc.fm.Product, unchanged: true})
					continue
				}
				// Content is unchanged but derived metadata is missing, so
				// the page only needs a cheap rewrite -- skip embedding.
				doc.metaOnly = true
				select {
				case completed <- doc:
				case <-ctx.Done():
					return
				}
				continue
			}
			doc.embeddings = make([][]float32, len(doc.chunks))
			if len(doc.chunks) == 0 {
				select {
				case completed <- doc:
				case <-ctx.Done():
				}
				continue
			}
			state := &documentState{doc: doc}
			batches := (len(doc.chunks) + opts.batchSize - 1) / opts.batchSize
			state.remaining.Store(int64(batches))
			for start := 0; start < len(doc.chunks); start += opts.batchSize {
				end := min(start+opts.batchSize, len(doc.chunks))
				select {
				case embedJobs <- embedJob{state: state, start: start, end: end}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	var embedWG sync.WaitGroup
	for range opts.embedWorkers {
		embedWG.Add(1)
		go func() {
			defer embedWG.Done()
			for job := range embedJobs {
				doc := job.state.doc
				var idxs []int
				var keys, inputs []string
				for i := job.start; i < job.end; i++ {
					text := doc.embedInput(i)
					key := hashContent(text)
					if vec, ok := cache.get(key); ok {
						doc.embeddings[i] = vec
						continue
					}
					idxs = append(idxs, i)
					keys = append(keys, key)
					inputs = append(inputs, text)
				}
				var err error
				if len(inputs) > 0 {
					var embeddings [][]float32
					embeddings, err = client.embed(ctx, inputs)
					if err == nil {
						for j, i := range idxs {
							doc.embeddings[i] = embeddings[j]
							cache.put(keys[j], embeddings[j])
						}
					} else {
						err = fmt.Errorf("embedding chunks %d-%d of %s: %w", job.start, job.end-1, doc.fm.URL, err)
					}
				}
				finishEmbedJob(ctx, job, err, completed, outcomes)
			}
		}()
	}
	go func() {
		embedWG.Wait()
		close(completed)
	}()

	var writeWG sync.WaitGroup
	for range opts.writeWorkers {
		writeWG.Add(1)
		go func() {
			defer writeWG.Done()
			for doc := range completed {
				if doc.metaOnly {
					err := backfillDocument(ctx, pool, doc)
					if err != nil {
						err = fmt.Errorf("backfilling %s: %w", doc.fm.URL, err)
					}
					sendOutcome(ctx, outcomes, outcome{url: doc.fm.URL, product: doc.fm.Product, backfilled: true, err: err})
					continue
				}
				err := writeDocument(ctx, pool, doc)
				if err != nil {
					err = fmt.Errorf("writing %s: %w", doc.fm.URL, err)
				}
				sendOutcome(ctx, outcomes, outcome{url: doc.fm.URL, product: doc.fm.Product, chunks: len(doc.chunks), err: err})
			}
		}()
	}
	go func() {
		writeWG.Wait()
		close(outcomes)
	}()
	return outcomes
}

func main() {
	cpuCount := runtime.GOMAXPROCS(0)
	parseWorkers := flag.Int("parse-workers", 0, "markdown parser workers (0 = logical CPU count)")
	embedWorkers := flag.Int("embed-workers", 0, "concurrent Ollama requests (0 = auto-detect)")
	writeWorkers := flag.Int("write-workers", 0, "concurrent PostgreSQL writers (0 = auto-detect)")
	batchSize := flag.Int("batch-size", 0, "texts per Ollama request (0 = auto-detect)")
	model := flag.String("model", envDefault("EMBEDDING_MODEL", "nomic-embed-text"), "Ollama embedding model")
	ollamaURL := flag.String("ollama-url", envDefault("OLLAMA_URL", "http://localhost:11434"), "Ollama base URL")
	allVersions := flag.Bool("all-versions", false, "also index patch-version directories superseded by a newer patch")
	prune := flag.Bool("prune", false, "after a fully successful run, delete indexed documents (per product seen on disk) whose source files are gone")
	flag.Parse()
	if flag.NArg() != 1 || *parseWorkers < 0 || *embedWorkers < 0 || *writeWorkers < 0 || *batchSize < 0 {
		fmt.Fprintln(os.Stderr, "usage: ingest [options] <markdown-dir>")
		flag.PrintDefaults()
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	probeClient := newOllamaClient(*ollamaURL, *model, 1)
	if _, err := probeClient.embed(ctx, []string{"throughput probe"}); err != nil {
		fmt.Fprintf(os.Stderr, "Ollama probe failed: %v\n", err)
		os.Exit(1)
	}
	gpu, err := probeClient.gpuResident(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not detect Ollama GPU usage: %v\n", err)
	}
	if *parseWorkers == 0 {
		*parseWorkers = cpuCount
	}
	if *embedWorkers == 0 {
		if gpu {
			*embedWorkers = 2
		} else {
			*embedWorkers = max(1, min(4, cpuCount/4))
		}
	}
	if *writeWorkers == 0 {
		*writeWorkers = max(2, min(8, cpuCount/4))
	}
	if *batchSize == 0 {
		if gpu {
			*batchSize = 64
		} else {
			*batchSize = 16
		}
	}
	opts := options{
		model: *model, ollamaURL: *ollamaURL, parseWorkers: *parseWorkers,
		embedWorkers: *embedWorkers, writeWorkers: *writeWorkers, batchSize: *batchSize,
	}

	config, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	config.MaxConns = int32(max(opts.writeWorkers+2, 4))
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return pgxvec.RegisterTypes(ctx, conn)
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer pool.Close()

	var paths []string
	err = filepath.WalkDir(flag.Arg(0), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(path), ".md") {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	if !*allVersions {
		paths = filterLatestPatch(flag.Arg(0), paths)
	}
	hashes, err := loadHashes(ctx, pool)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading existing document hashes: %v\n", err)
		os.Exit(1)
	}

	device := "CPU"
	if gpu {
		device = "GPU"
	}
	fmt.Printf("found %d markdown files; device=%s cpu=%d parse=%d embed=%d batch=%d write=%d\n",
		len(paths), device, cpuCount, opts.parseWorkers, opts.embedWorkers, opts.batchSize, opts.writeWorkers)
	client := newOllamaClient(opts.ollamaURL, opts.model, opts.embedWorkers)
	cache := newEmbedCache()
	completed, indexed, unchanged, backfilled, failed := 0, 0, 0, 0, 0
	var liveURLs []string
	start := time.Now()
	// Heartbeat so slow (CPU-bound) runs show liveness and rate between the
	// per-100 progress lines, which can be many minutes apart.
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()
	outcomes := runPipeline(ctx, pool, client, cache, paths, hashes, opts)
	for outcomes != nil {
		select {
		case result, ok := <-outcomes:
			if !ok {
				outcomes = nil
				continue
			}
			completed++
			switch {
			case result.err != nil:
				failed++
				fmt.Fprintf(os.Stderr, "error: %v\n", result.err)
			case result.unchanged:
				unchanged++
				liveURLs = append(liveURLs, result.url)
			case result.backfilled:
				backfilled++
				liveURLs = append(liveURLs, result.url)
			case result.url != "":
				indexed++
				liveURLs = append(liveURLs, result.url)
			}
			if completed%100 == 0 || completed == len(paths) {
				fmt.Printf("progress: %d/%d indexed=%d unchanged=%d backfilled=%d failed=%d\n",
					completed, len(paths), indexed, unchanged, backfilled, failed)
			}
		case <-heartbeat.C:
			elapsed := time.Since(start)
			rate := float64(completed) / elapsed.Seconds()
			remaining := "unknown"
			if rate > 0 {
				remaining = (time.Duration(float64(len(paths)-completed)/rate) * time.Second).Round(time.Minute).String()
			}
			hits, unique := cache.stats()
			fmt.Printf("heartbeat: %d/%d done in %s (%.1f files/s, ~%s remaining, embed cache: %d hits / %d unique)\n",
				completed, len(paths), elapsed.Round(time.Second), rate, remaining, hits, unique)
		}
	}
	if hits, unique := cache.stats(); hits > 0 || unique > 0 {
		fmt.Printf("embed cache: %d chunks reused, %d embedded\n", hits, unique)
	}
	if ctx.Err() != nil {
		fmt.Fprintf(os.Stderr, "ingestion cancelled: %d files did not complete\n", len(paths)-completed)
	}
	if *prune {
		switch {
		case ctx.Err() != nil || failed > 0:
			fmt.Fprintln(os.Stderr, "skipping -prune: run did not complete cleanly")
		case len(liveURLs) == 0:
			fmt.Fprintln(os.Stderr, "skipping -prune: no documents were seen on disk")
		default:
			removed, err := pruneStale(ctx, pool, liveURLs)
			if err != nil {
				fmt.Fprintf(os.Stderr, "prune failed: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("pruned %d stale documents\n", removed)
		}
	}
	if failed > 0 {
		os.Exit(1)
	}
}

func envDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
