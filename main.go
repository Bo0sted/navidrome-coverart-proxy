// navidrome-coverart-proxy
//
// A stateless reverse proxy that exposes only the Subsonic endpoints a music
// client needs to put cover art on a Discord profile, keeping the real server
// private. See README for architecture, security model, and configuration.

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ----------------------------------------------------------------------------
// Client spec model (implemented by <client>.go files as pure data)
// ----------------------------------------------------------------------------

type endpointKind int

const (
	// kindImage: GET cover art. Validates id, forwards to a fixed upstream
	// path, enforces image content-type, caches.
	kindImage endpointKind = iota

	// kindMetadata: forwards the request (method + body preserved), then
	// rewrites backend-referencing URLs in the response to the proxy's public
	// URL and strips backend identity fields. Not cached.
	kindMetadata

	// kindShareImage: a share-image whose token is in the path
	// (/share/img/<token>). Forwards the path as-is, enforces image
	// content-type, caches.
	kindShareImage
)

// endpoint describes one allowlisted request a client may make.
type endpoint struct {
	Paths        []string     // exact request paths (kindImage, kindMetadata)
	PathPrefix   string       // path-prefix match (kindShareImage)
	Method       string       // single accepted method ("GET" or "POST")
	Kind         endpointKind // execution mode
	UpstreamPath string       // fixed upstream path (ignored for kindShareImage)
}

type clientSpec struct {
	Name      string
	Endpoints []endpoint
}

// clientRegistry maps STREAMING_CLIENT values to specs. To add a client, write
// <client>.go declaring a clientSpec and register it here.
var clientRegistry = map[string]clientSpec{
	"feishin": feishinSpec,
}

type Config struct {
	ListenPort         int
	BackendInternalURL string
	PublicURL          string
	StreamingClient    string
	CacheMinutes       int
	LogLevel           string
}

func loadConfig() (Config, error) {
	cfg := Config{
		ListenPort:   8080,
		CacheMinutes: 60,
		LogLevel:     "info",
	}

	cfg.BackendInternalURL = os.Getenv("NAVIDROME_INTERNAL_URL")
	if cfg.BackendInternalURL == "" {
		return cfg, errors.New("NAVIDROME_INTERNAL_URL is required but not set")
	}
	u, err := url.Parse(cfg.BackendInternalURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return cfg, errors.New("NAVIDROME_INTERNAL_URL must be a valid http(s) URL with a host (e.g. http://navidrome:4533)")
	}

	cfg.PublicURL = strings.TrimRight(os.Getenv("PUBLIC_URL"), "/")
	if cfg.PublicURL == "" {
		return cfg, errors.New("PUBLIC_URL is required but not set (the public base URL this proxy is reached at, e.g. https://coverart.example.com)")
	}
	pu, err := url.Parse(cfg.PublicURL)
	if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" {
		return cfg, errors.New("PUBLIC_URL must be a valid http(s) URL with a host (e.g. https://coverart.example.com)")
	}

	cfg.StreamingClient = os.Getenv("STREAMING_CLIENT")
	if cfg.StreamingClient == "" {
		return cfg, errors.New("STREAMING_CLIENT is required but not set (supported: " + supportedClients() + ")")
	}
	if _, ok := clientRegistry[cfg.StreamingClient]; !ok {
		return cfg, errors.New("STREAMING_CLIENT \"" + cfg.StreamingClient + "\" is not supported (supported: " + supportedClients() + ")")
	}

	if v := os.Getenv("LISTEN_PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			return cfg, errors.New("LISTEN_PORT must be an integer between 1 and 65535")
		}
		cfg.ListenPort = n
	}
	if v := os.Getenv("CACHE_MINUTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return cfg, errors.New("CACHE_MINUTES must be a non-negative integer (0 disables caching)")
		}
		cfg.CacheMinutes = n
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		switch v {
		case "debug", "info":
			cfg.LogLevel = v
		default:
			return cfg, errors.New(`LOG_LEVEL must be one of: debug, info`)
		}
	}

	return cfg, nil
}

func supportedClients() string {
	names := make([]string, 0, len(clientRegistry))
	for name := range clientRegistry {
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}

// Validation rules
// allowedQueryKeys is the whitelist of Subsonic query params forwarded on GET.
var allowedQueryKeys = []string{"id", "u", "s", "t", "v", "c", "size", "f"}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var sizePattern = regexp.MustCompile(`^[0-9]{1,5}$`)

// forwardedAuthHeaders are stripped from every upstream request so the backend
// never sees a client-supplied (spoofable) source IP, host, or proto.
var forwardedAuthHeaders = []string{
	"X-Forwarded-For",
	"X-Forwarded-Host",
	"X-Forwarded-Proto",
	"X-Forwarded-Port",
	"X-Real-Ip",
	"Forwarded",
}

const maxMetadataRequestBody = 8 << 10 // 8 KiB


type cacheEntry struct {
	contentType string
	body        []byte
	expires     time.Time
}

type cache struct {
	mu  sync.RWMutex
	ttl time.Duration
	m   map[string]cacheEntry
}

func newCache(ttl time.Duration) *cache {
	return &cache{ttl: ttl, m: make(map[string]cacheEntry)}
}

func (c *cache) get(key string) (cacheEntry, bool) {
	if c.ttl <= 0 {
		return cacheEntry{}, false
	}
	c.mu.RLock()
	e, ok := c.m[key]
	c.mu.RUnlock()
	if !ok || time.Now().After(e.expires) {
		return cacheEntry{}, false
	}
	return e, true
}

func (c *cache) set(key, contentType string, body []byte) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	c.m[key] = cacheEntry{contentType: contentType, body: body, expires: time.Now().Add(c.ttl)}
	c.mu.Unlock()
}

type server struct {
	cfg          Config
	upstream     *url.URL
	client       *http.Client
	cache        *cache
	log          *slog.Logger
	maxBodySize  int64
	routes       map[string]endpoint // exact-path routes
	prefixRoutes []endpoint          // path-prefix routes
	backendHost  string              // scheme://host of the backend
}

func newServer(cfg Config, log *slog.Logger) (*server, error) {
	up, err := url.Parse(cfg.BackendInternalURL)
	if err != nil {
		return nil, err
	}

	spec := clientRegistry[cfg.StreamingClient]

	routes := make(map[string]endpoint)
	var prefixRoutes []endpoint
	for _, ep := range spec.Endpoints {
		if ep.PathPrefix != "" {
			prefixRoutes = append(prefixRoutes, ep)
		}
		for _, p := range ep.Paths {
			routes[p] = ep
		}
	}

	return &server{
		cfg:          cfg,
		upstream:     up,
		client:       &http.Client{Timeout: 15 * time.Second},
		cache:        newCache(time.Duration(cfg.CacheMinutes) * time.Minute),
		log:          log,
		maxBodySize:  25 << 20,
		routes:       routes,
		prefixRoutes: prefixRoutes,
		backendHost:  strings.TrimRight(up.Scheme+"://"+up.Host, "/"),
	}, nil
}

// dispatch routes a request to the matching endpoint and executes it by Kind.
func (s *server) dispatch(w http.ResponseWriter, r *http.Request) {
	remote := clientIP(r)

	s.log.Debug("request received",
		"method", r.Method,
		"path", r.URL.Path,
		"query", r.URL.RawQuery,
		"remote", remote,
		"user_agent", r.UserAgent(),
	)

	ep, ok := s.routes[r.URL.Path]
	if !ok {
		for _, pe := range s.prefixRoutes {
			if strings.HasPrefix(r.URL.Path, pe.PathPrefix) {
				ep, ok = pe, true
				break
			}
		}
	}
	if !ok {
		s.log.Info("request failed: path not in allowlist", "path", r.URL.Path, "remote", remote)
		http.NotFound(w, r)
		return
	}

	if r.Method != ep.Method && !(ep.Method == http.MethodGet && r.Method == http.MethodHead) {
		s.log.Info("request failed: method not allowed",
			"method", r.Method, "path", r.URL.Path, "remote", remote)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	switch ep.Kind {
	case kindImage:
		s.execImage(w, r, ep, remote)
	case kindMetadata:
		s.execMetadata(w, r, ep, remote)
	case kindShareImage:
		s.execShareImage(w, r, ep, remote)
	default:
		s.log.Info("request failed: unknown endpoint kind", "path", r.URL.Path, "remote", remote)
		http.NotFound(w, r)
	}
}

func (s *server) execImage(w http.ResponseWriter, r *http.Request, ep endpoint, remote string) {
	in := r.URL.Query()
	out := url.Values{}
	for _, key := range allowedQueryKeys {
		if v := in.Get(key); v != "" {
			out.Set(key, v)
		}
	}

	id := out.Get("id")
	if !idPattern.MatchString(id) {
		s.log.Info("request failed: invalid id", "id", id, "remote", remote)
		http.NotFound(w, r)
		return
	}
	if size := out.Get("size"); size != "" && !sizePattern.MatchString(size) {
		out.Del("size")
	}

	cacheKey := ep.UpstreamPath + "|" + id + "|" + out.Get("size")
	if e, ok := s.cache.get(cacheKey); ok {
		s.log.Debug("request served from cache", "id", id, "remote", remote)
		s.writeImage(w, r, e.contentType, e.body, "HIT")
		return
	}

	target := *s.upstream
	target.Path = ep.UpstreamPath
	target.RawQuery = out.Encode()

	resp, err := s.forward(r.Context(), http.MethodGet, target.String(), nil, "")
	if err != nil {
		s.log.Info("request failed: upstream fetch error", "err", err, "id", id, "remote", remote)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		s.log.Info("request failed: upstream returned non-200",
			"status", resp.StatusCode, "id", id, "remote", remote)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	ct := resp.Header.Get("Content-Type")
	if !isImageContentType(ct) {
		s.log.Info("request failed: upstream returned non-image",
			"content_type", ct, "status", resp.StatusCode, "id", id, "remote", remote)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, s.maxBodySize))
	if err != nil {
		s.log.Info("request failed: error reading upstream body", "err", err, "id", id, "remote", remote)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}

	s.cache.set(cacheKey, ct, body)
	s.log.Debug("request served from upstream",
		"id", id, "content_type", ct, "bytes", len(body), "remote", remote)
	s.writeImage(w, r, ct, body, "MISS")
}

func (s *server) execMetadata(w http.ResponseWriter, r *http.Request, ep endpoint, remote string) {
	var bodyData []byte
	if r.Body != nil {
		b, err := io.ReadAll(io.LimitReader(r.Body, maxMetadataRequestBody))
		if err != nil {
			s.log.Info("metadata failed: error reading request body", "err", err, "remote", remote)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		bodyData = b
	}

	target := *s.upstream
	target.Path = ep.UpstreamPath
	in := r.URL.Query()
	out := url.Values{}
	for _, key := range allowedQueryKeys {
		if v := in.Get(key); v != "" {
			out.Set(key, v)
		}
	}
	target.RawQuery = out.Encode()

	var reqBody io.Reader
	if len(bodyData) > 0 {
		reqBody = bytes.NewReader(bodyData)
	}
	resp, err := s.forward(r.Context(), r.Method, target.String(), reqBody, r.Header.Get("Content-Type"))
	if err != nil {
		s.log.Info("metadata failed: upstream fetch error", "err", err, "remote", remote)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, s.maxBodySize))
	if err != nil {
		s.log.Info("metadata failed: error reading upstream body", "err", err, "remote", remote)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}

	ct := resp.Header.Get("Content-Type")
	rewritten := s.rewriteMetadata(respBody)

	s.log.Debug("metadata served (rewritten)",
		"status", resp.StatusCode, "content_type", ct,
		"in_bytes", len(respBody), "out_bytes", len(rewritten), "remote", remote)

	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(rewritten)))
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		_, _ = w.Write(rewritten)
	}
}

// forward executes one upstream request with a fixed minimal header set and all
// forwarded-auth headers stripped.
func (s *server) forward(ctx context.Context, method, urlStr string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, urlStr, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "navidrome-coverart-proxy")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for _, h := range forwardedAuthHeaders {
		req.Header.Del(h)
	}
	return s.client.Do(req)
}

func (s *server) writeImage(w http.ResponseWriter, r *http.Request, contentType string, body []byte, cacheState string) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("X-Cache", cacheState)
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// sharePathPattern validates the token segment of a /share/img/<token> path.
var sharePathPattern = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]{1,1000}$`)

func (s *server) execShareImage(w http.ResponseWriter, r *http.Request, ep endpoint, remote string) {
	token := strings.TrimPrefix(r.URL.Path, ep.PathPrefix)
	if !sharePathPattern.MatchString(token) {
		s.log.Info("request failed: invalid share token", "remote", remote)
		http.NotFound(w, r)
		return
	}

	in := r.URL.Query()
	out := url.Values{}
	for _, key := range allowedQueryKeys {
		if v := in.Get(key); v != "" {
			out.Set(key, v)
		}
	}

	cacheKey := r.URL.Path + "|" + out.Get("size")
	if e, ok := s.cache.get(cacheKey); ok {
		s.log.Debug("share image served from cache", "remote", remote)
		s.writeImage(w, r, e.contentType, e.body, "HIT")
		return
	}

	target := *s.upstream
	target.Path = r.URL.Path
	target.RawQuery = out.Encode()

	resp, err := s.forward(r.Context(), http.MethodGet, target.String(), nil, "")
	if err != nil {
		s.log.Info("share image failed: upstream fetch error", "err", err, "remote", remote)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		s.log.Info("share image failed: upstream non-200", "status", resp.StatusCode, "remote", remote)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	ct := resp.Header.Get("Content-Type")
	if !isImageContentType(ct) {
		s.log.Info("share image failed: upstream non-image", "content_type", ct, "remote", remote)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, s.maxBodySize))
	if err != nil {
		s.log.Info("share image failed: error reading body", "err", err, "remote", remote)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}

	s.cache.set(cacheKey, ct, body)
	s.log.Debug("share image served from upstream", "content_type", ct, "bytes", len(body), "remote", remote)
	s.writeImage(w, r, ct, body, "MISS")
}

// handleHealth always returns 200 (proxy liveness) and reports backend
// reachability without disclosing the backend's identity or address.
func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	backend := "Not Reachable"
	if s.backendReachable(r.Context()) {
		backend = "Reachable"
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "Proxy: OK\nBackend: "+backend+"\n")
}

func (s *server) backendReachable(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	target := *s.upstream
	target.Path = "/rest/ping"
	resp, err := s.forward(ctx, http.MethodGet, target.String(), nil, "")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}

func (s *server) router() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthcheck", s.handleHealth)
	for path := range s.routes {
		mux.HandleFunc(path, s.dispatch)
	}
	for _, pe := range s.prefixRoutes {
		mux.HandleFunc(pe.PathPrefix, s.dispatch)
	}
	return mux
}


// Metadata rewriting / identity stripping
// serverIdentityFields are response fields that disclose the backend software
// name and version; stripped from metadata responses.
var serverIdentityFields = []string{"type", "serverVersion"}

var (
	jsonIdentityPatterns []*regexp.Regexp
	xmlIdentityPatterns  []*regexp.Regexp
)

func init() {
	for _, f := range serverIdentityFields {
		jsonIdentityPatterns = append(jsonIdentityPatterns,
			regexp.MustCompile(`,?\s*"`+regexp.QuoteMeta(f)+`"\s*:\s*"[^"]*"`))
		xmlIdentityPatterns = append(xmlIdentityPatterns,
			regexp.MustCompile(`\s*`+regexp.QuoteMeta(f)+`\s*=\s*"[^"]*"`))
	}
}

// stripServerIdentity removes backend software-name/version fields from a
// metadata response (JSON and XML forms).
func stripServerIdentity(body string) string {
	out := body
	for _, re := range jsonIdentityPatterns {
		out = re.ReplaceAllString(out, "")
	}
	for _, re := range xmlIdentityPatterns {
		out = re.ReplaceAllString(out, "")
	}
	out = strings.ReplaceAll(out, "{,", "{")
	return out
}

// rewriteMetadata replaces the backend host with the proxy's public URL in a
// metadata response and strips backend identity fields. If a backend-host
// reference still remains afterward, it returns a safe empty response.
func (s *server) rewriteMetadata(body []byte) []byte {
	rewritten := strings.ReplaceAll(string(body), s.backendHost, s.cfg.PublicURL)

	if u := s.upstream; u.Host != "" {
		rewritten = strings.ReplaceAll(rewritten, u.Host, hostOnly(s.cfg.PublicURL))
	}

	rewritten = stripServerIdentity(rewritten)

	if strings.Contains(rewritten, s.upstream.Host) {
		s.log.Info("metadata still referenced backend after rewrite; returning empty")
		return []byte(`{"subsonic-response":{"status":"ok"}}`)
	}
	return []byte(rewritten)
}

func hostOnly(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		return u.Host
	}
	return rawURL
}

func isImageContentType(ct string) bool {
	return strings.HasPrefix(ct, "image/")
}

// clientIP returns a best-effort remote IP for logging only (never trusted for
// auth).
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return xff[:i]
		}
		return xff
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	default:
		return slog.LevelInfo
	}
}

func main() {
	boot := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := loadConfig()
	if err != nil {
		boot.Error("configuration error, shutting down", "err", err)
		os.Exit(1)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: parseLevel(cfg.LogLevel),
	}))

	srv, err := newServer(cfg, log)
	if err != nil {
		log.Error("failed to init server", "err", err)
		os.Exit(1)
	}

	addr := ":" + strconv.Itoa(cfg.ListenPort)
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv.router(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	serverErr := make(chan error, 1)
	go func() {
		log.Info("navidrome-coverart-proxy starting",
			"listen", addr,
			"client", cfg.StreamingClient,
			"cache_minutes", cfg.CacheMinutes,
		)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		log.Error("server stopped unexpectedly", "err", err)
		os.Exit(1)
	case <-ctx.Done():
		log.Info("shutdown signal received, draining connections")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed, forcing close", "err", err)
		_ = httpServer.Close()
		os.Exit(1)
	}
	log.Info("shutdown complete")
}
