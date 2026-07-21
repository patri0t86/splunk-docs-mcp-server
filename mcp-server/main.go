package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pgvector/pgvector-go"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
)

var pool *pgxpool.Pool

// ollamaURL points at the Ollama instance serving embeddings. Override with
// the OLLAMA_URL env var when it's not running on the same host as this server.
var ollamaURL = getEnvDefault("OLLAMA_URL", "http://localhost:11434")

// embedModel must match the model the indexer ingested with (same env var),
// since query and document vectors need to live in the same space.
var embedModel = getEnvDefault("EMBEDDING_MODEL", "nomic-embed-text")

// knownProducts holds the distinct documents.product values, loaded at
// startup, so a bad product filter fails with the valid options instead of
// silently returning nothing.
var knownProducts = map[string]bool{}

func productList() []string {
	products := make([]string, 0, len(knownProducts))
	for p := range knownProducts {
		products = append(products, p)
	}
	sort.Strings(products)
	return products
}

func loadKnownProducts(ctx context.Context) error {
	rows, err := pool.Query(ctx, `SELECT DISTINCT product FROM documents`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return err
		}
		if p != "" {
			knownProducts[p] = true
		}
	}
	return rows.Err()
}

// ollamaClient bounds embedding calls so a wedged Ollama surfaces as an
// error instead of hanging the tool call until the MCP client gives up.
var ollamaClient = &http.Client{Timeout: 30 * time.Second}

// toolTimeout caps each tool call server-side so failures are logged here
// rather than only manifesting as a silent client-side timeout.
const toolTimeout = 60 * time.Second

// maxQueryLen bounds search queries so arbitrarily large input can't be
// relayed to Ollama and Postgres. Real search queries are a few hundred
// bytes at most.
const maxQueryLen = 4096

// maxURLLen bounds get_page URLs; real documentation URLs are far shorter.
const maxURLLen = 2048

// maxBodyBytes caps inbound request bodies before they reach the MCP
// handler. Tool-call payloads are tiny; anything near this limit is abuse.
const maxBodyBytes = 1 << 20

// maxResultsCap bounds search_docs max_results; the SQL already limits
// candidates to 50 per leg, this just keeps responses sane.
const maxResultsCap = 20

// toolSem bounds concurrent tool calls so a burst of clients can't pile
// unbounded embedding work onto Ollama or saturate the DB pool. Callers
// queue (bounded by their ctx deadline) rather than failing immediately.
var toolSem = make(chan struct{}, 8)

func acquireToolSlot(ctx context.Context) (release func(), err error) {
	select {
	case toolSem <- struct{}{}:
		return func() { <-toolSem }, nil
	case <-ctx.Done():
		return nil, errors.New("server is at capacity, try again shortly")
	}
}

func getEnvDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

type SearchArgs struct {
	Query      string `json:"query" jsonschema:"the search query"`
	Product    string `json:"product,omitempty" jsonschema:"optional product to filter by: splunk-enterprise, splunk-cloud-platform, splunk-enterprise-security-8, splunk-soar, or splunk-it-service-intelligence"`
	Version    string `json:"version,omitempty" jsonschema:"optional doc version to filter by, e.g. 10.2 (Splunk Enterprise), 10.5.2605 (Splunk Cloud release train), 8.5 (ES)"`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"maximum number of results to return, default 5"`
}

type GetPageArgs struct {
	URL string `json:"url" jsonschema:"the documentation page URL to fetch, usually taken from a search_docs result"`
}

// embed calls a local Ollama instance.
func embed(ctx context.Context, text string) ([]float32, error) {
	payload, _ := json.Marshal(map[string]string{
		"model":  embedModel,
		"prompt": text,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		ollamaURL+"/api/embeddings", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := ollamaClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama at %s unreachable: %w", ollamaURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("ollama returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var result struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if len(result.Embedding) == 0 {
		return nil, fmt.Errorf("ollama returned an empty embedding (is model %s pulled?)", embedModel)
	}
	return result.Embedding, nil
}

// orQuery rejoins the query terms with OR so the keyword leg can still rank
// partial matches when the full query is too strict to match any chunk.
// ts_rank still rewards chunks matching more of the terms, so this degrades
// gracefully rather than switching the keyword leg off entirely.
func orQuery(q string) string {
	fields := strings.Fields(q)
	if len(fields) < 2 {
		return q
	}
	return strings.Join(fields, " OR ")
}

// searchDocs runs keyword search (ts_rank) and semantic search (cosine
// distance) independently, then merges them with reciprocal rank fusion
// so neither search mode's score scale dominates the other. Results are
// deduplicated by (product, title), keeping the newest version of each
// page, because the corpus holds near-identical copies of every page
// across versions — but pages in different products that happen to share
// a title ("Fixed issues") must never collapse into one result.
func searchDocs(ctx context.Context, req *mcp.CallToolRequest, args SearchArgs) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()

	args.Query = strings.TrimSpace(args.Query)
	if args.Query == "" {
		return nil, nil, errors.New("query must not be empty")
	}
	if len(args.Query) > maxQueryLen {
		return nil, nil, fmt.Errorf("query too long (max %d bytes)", maxQueryLen)
	}
	if args.MaxResults <= 0 {
		args.MaxResults = 5
	}
	if args.MaxResults > maxResultsCap {
		args.MaxResults = maxResultsCap
	}
	if args.Product != "" && !knownProducts[args.Product] {
		return nil, nil, fmt.Errorf("unknown product %q; indexed products: %s",
			args.Product, strings.Join(productList(), ", "))
	}
	log.Printf("search_docs: query=%q product=%q version=%q max_results=%d", args.Query, args.Product, args.Version, args.MaxResults)

	release, err := acquireToolSlot(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer release()

	start := time.Now()
	// nomic-embed-text queries must carry the search_query prefix to match
	// the search_document-prefixed vectors written by the indexer.
	queryVec, err := embed(ctx, "search_query: "+args.Query)
	if err != nil {
		log.Printf("search_docs: ERROR embedding query after %s: %v", time.Since(start).Round(time.Millisecond), err)
		return nil, nil, errors.New("search backend unavailable, try again shortly")
	}
	log.Printf("search_docs: embedded query in %s (%d dims)", time.Since(start).Round(time.Millisecond), len(queryVec))

	dbStart := time.Now()
	kwQuery := args.Query
	var strictHits bool
	// The precheck applies the same product/version filters as the keyword
	// leg: strict hits that exist only outside the filter scope must still
	// trigger the OR fallback.
	err = pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM chunks c
			JOIN documents d ON d.id = c.document_id
			WHERE c.tsv @@ websearch_to_tsquery('english', $1)
			  AND ($2 = '' OR d.product = $2)
			  AND ($3 = '' OR d.version = $3)
		)`,
		args.Query, args.Product, args.Version).Scan(&strictHits)
	if err != nil {
		log.Printf("search_docs: ERROR keyword precheck: %v", err)
		return nil, nil, errors.New("search failed, try again shortly")
	}
	if !strictHits {
		kwQuery = orQuery(args.Query)
		log.Printf("search_docs: no strict keyword hits, retrying keyword leg as %q", kwQuery)
	}

	// Product/version filters run inside each leg, not after: with five
	// products in one index, a post-filter would let the dominant products
	// crowd a filtered query's candidates out of the fixed-size pools.
	rows, err := pool.Query(ctx, `
		WITH keyword AS (
			SELECT id, row_number() OVER (ORDER BY r DESC) AS rank
			FROM (
				SELECT c.id, ts_rank(c.tsv, websearch_to_tsquery('english', $1)) AS r
				FROM chunks c
				JOIN documents d ON d.id = c.document_id
				WHERE c.tsv @@ websearch_to_tsquery('english', $1)
				  AND ($3 = '' OR d.product = $3)
				  AND ($4 = '' OR d.version = $4)
				ORDER BY r DESC
				LIMIT 50
			) k
		),
		semantic AS (
			SELECT id, row_number() OVER (ORDER BY dist) AS rank
			FROM (
				SELECT c.id, c.embedding <=> $2 AS dist
				FROM chunks c
				JOIN documents d ON d.id = c.document_id
				WHERE ($3 = '' OR d.product = $3)
				  AND ($4 = '' OR d.version = $4)
				ORDER BY dist
				LIMIT 50
			) s
		),
		hits AS (
			SELECT COALESCE(k.id, s.id) AS id,
			       COALESCE(1.0/(60+k.rank), 0) + COALESCE(1.0/(60+s.rank), 0) AS score,
			       k.id IS NOT NULL AS keyword_hit
			FROM keyword k
			FULL OUTER JOIN semantic s ON s.id = k.id
		),
		scored AS (
			SELECT d.title, d.url, d.product, d.version, d.breadcrumb, c.heading, c.content,
			       h.score, h.keyword_hit
			FROM hits h
			JOIN chunks c ON c.id = h.id
			JOIN documents d ON d.id = c.document_id
		),
		deduped AS (
			SELECT *,
			       max(score) OVER (PARTITION BY product, title) AS group_score,
			       row_number() OVER (
			           PARTITION BY product, title
			           ORDER BY string_to_array(version, '.')::int[] DESC, score DESC
			       ) AS rn
			FROM scored
		)
		SELECT title, url, product, version, breadcrumb, heading,
		       CASE WHEN keyword_hit THEN
		           ts_headline('english', content, websearch_to_tsquery('english', $1),
		               'StartSel=**, StopSel=**, MaxFragments=3, MaxWords=50, MinWords=15, FragmentDelimiter=" … "')
		       ELSE left(content, 500) END AS snippet
		FROM deduped
		WHERE rn = 1
		ORDER BY group_score DESC
		LIMIT $5
	`, kwQuery, pgvector.NewVector(queryVec), args.Product, args.Version, args.MaxResults)
	if err != nil {
		log.Printf("search_docs: ERROR db query after %s: %v", time.Since(dbStart).Round(time.Millisecond), err)
		return nil, nil, errors.New("search failed, try again shortly")
	}
	defer rows.Close()

	var sb strings.Builder
	count := 0
	for rows.Next() {
		var title, pageURL, product, version, breadcrumb, heading, snippet string
		if err := rows.Scan(&title, &pageURL, &product, &version, &breadcrumb, &heading, &snippet); err != nil {
			log.Printf("search_docs: ERROR scanning row: %v", err)
			return nil, nil, errors.New("search failed, try again shortly")
		}
		count++
		fmt.Fprintf(&sb, "## %s (%s v%s)\n%s > %s\n%s\n\n%s\n\n---\n\n",
			title, product, version, breadcrumb, heading, pageURL, snippet)
	}
	if err := rows.Err(); err != nil {
		log.Printf("search_docs: ERROR reading rows after %s: %v", time.Since(dbStart).Round(time.Millisecond), err)
		return nil, nil, errors.New("search failed, try again shortly")
	}
	if count == 0 {
		sb.WriteString("No matching results.")
	}
	log.Printf("search_docs: db returned %d results in %s (total %s)",
		count, time.Since(dbStart).Round(time.Millisecond), time.Since(start).Round(time.Millisecond))

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: sb.String()}},
	}, nil, nil
}

// renderPage reassembles a page from its chunks in order. Returns the
// rendered markdown and the chunk count (0 means the URL is not indexed).
func renderPage(ctx context.Context, pageURL string) (string, int, error) {
	rows, err := pool.Query(ctx, `
		SELECT d.title, d.breadcrumb, c.heading, c.heading_level, c.content
		FROM chunks c
		JOIN documents d ON d.id = c.document_id
		WHERE d.url = $1
		ORDER BY c.chunk_index
	`, pageURL)
	if err != nil {
		return "", 0, err
	}
	defer rows.Close()

	var sb strings.Builder
	var title, breadcrumb string
	chunks := 0
	for rows.Next() {
		var heading, content string
		var level int
		if err := rows.Scan(&title, &breadcrumb, &heading, &level, &content); err != nil {
			return "", 0, err
		}
		chunks++
		if heading != "" {
			if level < 2 || level > 6 {
				level = 2
			}
			fmt.Fprintf(&sb, "%s %s\n\n", strings.Repeat("#", level), heading)
		}
		sb.WriteString(content)
		sb.WriteString("\n\n")
	}
	if err := rows.Err(); err != nil {
		return "", 0, err
	}
	if chunks == 0 {
		return "", 0, nil
	}
	return fmt.Sprintf("# %s\n%s\n\n%s", title, breadcrumb, sb.String()), chunks, nil
}

var slugCleanRe = regexp.MustCompile(`[^a-z0-9]+`)

// extractSlug pulls a normalized (lowercase, alphanumeric-only) page slug out
// of a URL: the topic id of a ?resourceId= redirect link, or the last path
// segment otherwise.
func extractSlug(raw string) string {
	slug := ""
	if u, err := url.Parse(raw); err == nil {
		if id := u.Query().Get("resourceId"); id != "" {
			parts := strings.Split(id, "_")
			slug = parts[len(parts)-1]
		} else {
			p := strings.Trim(u.Path, "/")
			if i := strings.LastIndex(p, "/"); i >= 0 {
				p = p[i+1:]
			}
			slug = p
		}
	}
	return slugCleanRe.ReplaceAllString(strings.ToLower(slug), "")
}

// resolveURL maps a URL that is not in the index to one that is. The docs
// cross-reference each other with help.splunk.com/?resourceId=Splunk_X_Topicid
// redirect links whose final hop is resolved client-side in JS, so they can
// never match an indexed URL directly. The topic id is the page's URL slug
// with the punctuation removed (Referencehardware -> reference-hardware), so
// matching it against hyphen-stripped indexed URLs finds the page. The same
// trick resolves direct URLs that differ from the crawled version only in
// path (e.g. a different manual edition), via their last path segment.
func resolveURL(ctx context.Context, raw string) (string, bool) {
	slug := extractSlug(raw)
	if slug == "" {
		return "", false
	}
	var resolved string
	err := pool.QueryRow(ctx, `
		SELECT url
		FROM documents
		WHERE replace(lower(url), '-', '') LIKE '%' || $1 || '%'
		ORDER BY string_to_array(version, '.')::int[] DESC, length(url)
		LIMIT 1
	`, slug).Scan(&resolved)
	if err != nil {
		return "", false
	}
	return resolved, true
}

// getPage returns the full, un-truncated content of a page, reassembled
// from its chunks in order -- for when search_docs has pointed at the
// right page but the agent needs the whole thing.
func getPage(ctx context.Context, req *mcp.CallToolRequest, args GetPageArgs) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()

	log.Printf("get_page: url=%q", args.URL)
	start := time.Now()

	pageURL := strings.TrimSpace(args.URL)
	if pageURL == "" {
		return nil, nil, errors.New("url must not be empty")
	}
	if len(pageURL) > maxURLLen {
		return nil, nil, fmt.Errorf("url too long (max %d bytes)", maxURLLen)
	}

	release, err := acquireToolSlot(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer release()

	text, chunks, err := renderPage(ctx, pageURL)
	if err != nil {
		log.Printf("get_page: ERROR after %s: %v", time.Since(start).Round(time.Millisecond), err)
		return nil, nil, errors.New("page fetch failed, try again shortly")
	}
	note := ""
	if chunks == 0 {
		if resolved, ok := resolveURL(ctx, pageURL); ok {
			text, chunks, err = renderPage(ctx, resolved)
			if err != nil {
				log.Printf("get_page: ERROR after %s: %v", time.Since(start).Round(time.Millisecond), err)
				return nil, nil, errors.New("page fetch failed, try again shortly")
			}
			note = fmt.Sprintf("(Resolved %s to %s)\n\n", pageURL, resolved)
		}
	}
	log.Printf("get_page: %d chunks in %s", chunks, time.Since(start).Round(time.Millisecond))
	if chunks == 0 {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "No page found for that URL. Try search_docs to locate the page."}},
		}, nil, nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: note + text}}}, nil, nil
}

// redactDSN strips the password from a connection string for logging.
func redactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return dsn
	}
	if _, has := u.User.Password(); has {
		u.User = url.UserPassword(u.User.Username(), "REDACTED")
	}
	return u.String()
}

// requireAuth rejects requests that don't carry the shared bearer token.
// Comparison is constant-time over fixed-length digests so neither token
// content nor length leaks through timing.
func requireAuth(token string, next http.Handler) http.Handler {
	want := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		auth := r.Header.Get("Authorization")
		if len(auth) < len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="splunk-docs-mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		got := sha256.Sum256([]byte(auth[len(prefix):]))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="splunk-docs-mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// logHTTP logs each inbound HTTP request so client connections and
// disconnections are visible on stdout. Path and headers are logged with %q
// so encoded control characters can't forge log lines. Behind the load
// balancer RemoteAddr is the LB, so X-Forwarded-For is logged too.
func logHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		session := r.Header.Get("Mcp-Session-Id")
		if session == "" {
			session = "-"
		}
		xff := r.Header.Get("X-Forwarded-For")
		if xff == "" {
			xff = "-"
		}
		log.Printf("http: %s %q from %s xff=%q session=%s ua=%q", r.Method, r.URL.Path, r.RemoteAddr, xff, session, r.UserAgent())
		next.ServeHTTP(w, r)
		log.Printf("http: %s %q from %s xff=%q session=%s closed after %s", r.Method, r.URL.Path, r.RemoteAddr, xff, session, time.Since(start).Round(time.Millisecond))
	})
}

// checkOllama verifies the embedding backend is reachable and the model is
// present before accepting traffic, so misconfiguration fails at startup
// instead of on the first query.
func checkOllama(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := embed(ctx, "startup probe"); err != nil {
		return err
	}
	return nil
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	ctx := context.Background()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is not set")
	}
	// The server refuses to start without a credible auth token so a
	// misconfigured deploy can never listen unauthenticated.
	authToken := os.Getenv("MCP_AUTH_TOKEN")
	if len(authToken) < 32 {
		log.Fatal("MCP_AUTH_TOKEN is not set or shorter than 32 characters; generate one with: openssl rand -hex 32")
	}
	log.Printf("startup: postgres = %s", redactDSN(dsn))
	log.Printf("startup: ollama   = %s (model %s)", ollamaURL, embedModel)

	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		log.Fatal(err)
	}
	// Without a connect timeout, an unroutable DB host (wrong IP, security
	// group, VPC) makes the first query hang for minutes instead of failing.
	config.ConnConfig.ConnectTimeout = 10 * time.Second
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if err := pgxvec.RegisterTypes(ctx, conn); err != nil {
			return err
		}
		// hnsw.ef_search defaults to 40, below the 50 candidates the
		// semantic leg asks for. 200 leaves headroom for product/version
		// filters, which discard scanned neighbors after the ANN walk: a
		// filter matching ~10% of the corpus still yields ~20 candidates.
		_, err := conn.Exec(ctx, "SET hnsw.ef_search = 200")
		return err
	}
	pool, err = pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	// The pool connects lazily; ping now so an unreachable DB kills the
	// server at startup with a clear error rather than timing out per-query.
	pingCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	if err := pool.Ping(pingCtx); err != nil {
		log.Fatalf("startup: cannot reach Postgres at %s: %v (is the host reachable from this machine? check DATABASE_URL, security groups, VPC routing)",
			config.ConnConfig.Host, err)
	}
	cancel()
	var chunkCount int64
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM chunks").Scan(&chunkCount); err != nil {
		log.Fatalf("startup: postgres reachable but chunks table unusable: %v", err)
	}
	log.Printf("startup: postgres OK, %d chunks indexed", chunkCount)

	if err := loadKnownProducts(ctx); err != nil {
		log.Fatalf("startup: loading product list: %v", err)
	}
	log.Printf("startup: products: %s", strings.Join(productList(), ", "))

	if err := checkOllama(ctx); err != nil {
		log.Fatalf("startup: ollama embedding probe failed: %v", err)
	}
	log.Println("startup: ollama OK")

	server := mcp.NewServer(&mcp.Implementation{Name: "splunk-docs", Version: "1.1.0"}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_docs",
		Description: "Search Splunk documentation (Splunk Enterprise, Splunk Cloud Platform, Enterprise Security, SOAR, and IT Service Intelligence) by keyword and meaning. Returns the best-matching sections with links, deduplicated to the newest version of each page per product. Pass product (e.g. \"splunk-soar\") and/or version (e.g. \"10.2\") to narrow the search. Use get_page for the full page a result came from.",
	}, searchDocs)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_page",
		Description: "Fetch the full content of a Splunk documentation page by URL, typically from a search_docs result. Also resolves the help.splunk.com/?resourceId=... cross-reference links that appear inside documentation pages.",
	}, getPage)

	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{},
	)

	// A dedicated mux (not http.DefaultServeMux) guarantees nothing else --
	// e.g. a future import of net/http/pprof -- can register public routes.
	mux := http.NewServeMux()
	mux.Handle("/mcp", logHTTP(requireAuth(authToken, handler)))
	// Unauthenticated liveness endpoint for the load balancer target group.
	// Deliberately touches no backends: it answers "is the process up",
	// and can't be used to hammer Postgres or Ollama.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	})

	addr := getEnvDefault("LISTEN_ADDR", ":8080")
	srv := &http.Server{
		Addr: addr,
		// MaxBytesHandler rejects oversized request bodies before the MCP
		// handler buffers them.
		Handler: http.MaxBytesHandler(mux, maxBodyBytes),
		// ReadHeaderTimeout defeats Slowloris-style connection holding.
		// No ReadTimeout/WriteTimeout: streamable HTTP responses are
		// long-lived SSE streams that a write deadline would sever.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("Splunk docs MCP server listening on %s/mcp (bearer auth enabled)", addr)
	log.Fatal(srv.ListenAndServe())
}
