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

// searchDocs and its helpers live in search.go.

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

// versionSegRe matches the version segment of a documentation path: a dotted
// release number ("10.4", "9.4.13", "10.5.2605") or the "latest" alias.
var versionSegRe = regexp.MustCompile(`^(latest|\d+(\.\d+)*)$`)

// normalizeAlias lowercases and strips every non-alphanumeric character, the
// same normalization the indexer applies when it writes document_aliases.
func normalizeAlias(s string) string {
	return slugCleanRe.ReplaceAllString(strings.ToLower(s), "")
}

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
	return normalizeAlias(slug)
}

// urlProduct returns the product slug for known Splunk docs hosts, mirroring
// the indexer so a URL maps to the same canonical identity on both sides.
func urlProduct(raw string) string {
	u, err := url.Parse(raw)
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
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) < 2 || parts[0] != "en" {
			return ""
		}
		return parts[1]
	default:
		return ""
	}
}

// urlAliases builds the document_aliases keys a URL could be indexed under.
// The docs cross-reference each other with help.splunk.com/?resourceId=
// redirect links whose final hop is resolved client-side in JS, so they never
// match an indexed URL directly; the topic id is the page's slug with the
// punctuation removed. Whole-path aliases additionally resolve URLs that
// differ from the crawled one only by version or manual edition, and are
// tried first because a slug alone ("troubleshooting") is ambiguous.
//
// The path alias must reproduce the indexer's canonical_id exactly — product
// slug included, and with only the first version-like segment dropped — or the
// exact lookup silently never matches.
func urlAliases(raw string) []string {
	var aliases []string
	if u, err := url.Parse(raw); err == nil && u.Query().Get("resourceId") == "" {
		segs := strings.Split(strings.Trim(u.Path, "/"), "/")
		product := urlProduct(raw)
		if strings.EqualFold(u.Hostname(), "help.splunk.com") && len(segs) >= 2 && segs[0] == "en" {
			segs = segs[2:]
		}
		canonical := make([]string, 0, len(segs))
		dropped := false
		for _, seg := range segs {
			if seg == "" {
				continue
			}
			if !dropped && versionSegRe.MatchString(seg) {
				dropped = true
				continue
			}
			canonical = append(canonical, seg)
		}
		if len(canonical) > 0 && product != "" {
			aliases = append(aliases, "path:"+normalizeAlias(product+"/"+strings.Join(canonical, "/")))
		}
	}
	if slug := extractSlug(raw); slug != "" {
		aliases = append(aliases, "slug:"+slug)
	}
	return aliases
}

// resolveURL maps a URL that is not in the index to one that is, via the
// alias table the indexer populates. Aliases are matched exactly and in
// priority order (whole path before bare slug), so a generic slug can no
// longer silently resolve to an unrelated page the way a substring match
// over the url column could.
func resolveURL(ctx context.Context, raw string) (string, bool) {
	for _, alias := range urlAliases(raw) {
		var resolved string
		err := pool.QueryRow(ctx, `
			SELECT d.url
			FROM document_aliases a
			JOIN documents d ON d.id = a.document_id
			WHERE a.alias = $1
			ORDER BY
				CASE
					WHEN d.version ~ '^[0-9]+(\.[0-9]+)+$' THEN string_to_array(d.version, '.')::int[]
					ELSE NULL
				END DESC NULLS LAST,
				length(d.url)
			LIMIT 1
		`, alias).Scan(&resolved)
		if err == nil {
			return resolved, true
		}
	}
	return "", false
}

// PageOutput is the structured form of a get_page result, so a client can
// read the page metadata without parsing the rendered markdown.
type PageOutput struct {
	URL             string   `json:"url" jsonschema:"the indexed URL the content came from, which may differ from the requested one"`
	RequestedURL    string   `json:"requested_url"`
	Resolved        bool     `json:"resolved" jsonschema:"true when the requested URL was mapped to a different indexed URL"`
	Found           bool     `json:"found"`
	DocumentID      string   `json:"document_id,omitempty" jsonschema:"version-independent identifier of the page"`
	Title           string   `json:"title,omitempty"`
	Product         string   `json:"product,omitempty"`
	Version         string   `json:"version,omitempty"`
	Manual          string   `json:"manual,omitempty"`
	Breadcrumb      []string `json:"breadcrumb,omitempty"`
	Sections        int      `json:"sections" jsonschema:"number of sections the page was reassembled from"`
	Content         string   `json:"content,omitempty" jsonschema:"the complete page as markdown"`
	ContentHash     string   `json:"content_hash,omitempty"`
	SourceUpdatedAt string   `json:"source_updated_at,omitempty"`
}

// getPage returns the full, un-truncated content of a page, reassembled
// from its chunks in order -- for when search_docs has pointed at the
// right page but the agent needs the whole thing.
func getPage(ctx context.Context, req *mcp.CallToolRequest, args GetPageArgs) (*mcp.CallToolResult, *PageOutput, error) {
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
	out := &PageOutput{RequestedURL: pageURL, URL: pageURL}
	note := ""
	if chunks == 0 {
		if resolved, ok := resolveURL(ctx, pageURL); ok {
			text, chunks, err = renderPage(ctx, resolved)
			if err != nil {
				log.Printf("get_page: ERROR after %s: %v", time.Since(start).Round(time.Millisecond), err)
				return nil, nil, errors.New("page fetch failed, try again shortly")
			}
			note = fmt.Sprintf("(Resolved %s to %s)\n\n", pageURL, resolved)
			out.URL = resolved
			out.Resolved = true
		}
	}
	log.Printf("get_page: %d chunks in %s", chunks, time.Since(start).Round(time.Millisecond))
	if chunks == 0 {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "No page found for that URL. Try search_docs to locate the page."}},
		}, out, nil
	}
	out.Found = true
	out.Sections = chunks
	out.Content = text
	if err := loadPageMetadata(ctx, out); err != nil {
		log.Printf("get_page: WARN loading metadata for %s: %v", out.URL, err)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: note + text}}}, out, nil
}

// loadPageMetadata fills in the document-level fields of a page result. A
// failure here is not fatal: the page content is already rendered, so the
// caller still gets the answer, just without the citation metadata.
func loadPageMetadata(ctx context.Context, out *PageOutput) error {
	var breadcrumb *string
	var sourceUpdated *time.Time
	err := pool.QueryRow(ctx, `
		SELECT canonical_id, title, product, version, manual, breadcrumb, content_hash, source_updated_at
		FROM documents
		WHERE url = $1
	`, out.URL).Scan(&out.DocumentID, &out.Title, &out.Product, &out.Version, &out.Manual,
		&breadcrumb, &out.ContentHash, &sourceUpdated)
	if err != nil {
		return err
	}
	if breadcrumb != nil && *breadcrumb != "" {
		out.Breadcrumb = strings.Split(*breadcrumb, " > ")
	}
	if sourceUpdated != nil {
		out.SourceUpdatedAt = sourceUpdated.UTC().Format(time.RFC3339)
	}
	return nil
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

	handler := newDualMCPHandler(
		newModernMCPHandler(os.Getenv("MCP_ALLOWED_ORIGINS")),
		newLegacyMCPHandler(),
		os.Getenv("MCP_ALLOWED_ORIGINS"),
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
