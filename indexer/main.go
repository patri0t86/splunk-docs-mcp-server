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
	URL         string   `yaml:"url"`
	Title       string   `yaml:"title"`
	Product     string   `yaml:"product"`
	Version     string   `yaml:"version"`
	Breadcrumbs []string `yaml:"breadcrumbs"`
}

// breadcrumb flattens the frontmatter breadcrumbs list into the single
// "A > B > C" string stored in documents.breadcrumb.
func (fm frontmatter) breadcrumb() string {
	return strings.Join(fm.Breadcrumbs, " > ")
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
	url       string
	product   string
	chunks    int
	unchanged bool
	err       error
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

func loadHashes(ctx context.Context, pool *pgxpool.Pool) (map[string]string, error) {
	rows, err := pool.Query(ctx, `SELECT url, content_hash FROM documents`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hashes := make(map[string]string)
	for rows.Next() {
		var url, hash string
		if err := rows.Scan(&url, &hash); err != nil {
			return nil, err
		}
		hashes[url] = hash
	}
	return hashes, rows.Err()
}

func writeDocument(ctx context.Context, pool *pgxpool.Pool, doc *document) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var docID int
	err = tx.QueryRow(ctx, `
		INSERT INTO documents (url, title, product, version, breadcrumb, content_hash)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (url) DO UPDATE
			SET title = EXCLUDED.title,
			    product = EXCLUDED.product,
			    version = EXCLUDED.version,
			    breadcrumb = EXCLUDED.breadcrumb,
			    content_hash = EXCLUDED.content_hash,
			    updated_at = now()
		RETURNING id
	`, doc.fm.URL, doc.fm.Title, doc.fm.Product, doc.fm.Version, doc.fm.breadcrumb(), doc.hash).Scan(&docID)
	if err != nil {
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
		// Weighted so that a query matching a page title or section heading
		// outranks one that only matches body text. COPY cannot compute
		// server-side expressions, hence the separate UPDATE.
		_, err = tx.Exec(ctx, `
			UPDATE chunks SET tsv =
				setweight(to_tsvector('english', $2), 'A') ||
				setweight(to_tsvector('english', coalesce(heading, '')), 'B') ||
				setweight(to_tsvector('english', content), 'C')
			WHERE document_id = $1
		`, docID, doc.fm.Title)
		if err != nil {
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

func runPipeline(ctx context.Context, pool *pgxpool.Pool, client *ollamaClient, cache *embedCache, paths []string, hashes map[string]string, opts options) <-chan outcome {
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
			if hashes[doc.fm.URL] == doc.hash {
				sendOutcome(ctx, outcomes, outcome{url: doc.fm.URL, product: doc.fm.Product, unchanged: true})
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
	completed, indexed, unchanged, failed := 0, 0, 0, 0
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
			case result.url != "":
				indexed++
				liveURLs = append(liveURLs, result.url)
			}
			if completed%100 == 0 || completed == len(paths) {
				fmt.Printf("progress: %d/%d indexed=%d unchanged=%d failed=%d\n",
					completed, len(paths), indexed, unchanged, failed)
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
