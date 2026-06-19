package fakeproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultBindAddr       = "127.0.0.1"
	defaultMaxRewriteBody = 8 << 20
)

// Config configures global proxy behavior.
type Config struct {
	BindAddr string

	ProxyURL  *url.URL //Set if httpProxy is needed;
	Transport http.RoundTripper

	ResponseHook ResponseHook
	RewriteHook  RewriteHook

	MaxRewriteBody int64
}

// ResponseHook runs after an upstream response is received and before it is
// rewritten for the local client. Returning false rejects the current response.
type ResponseHook func(*Server, ResponseEvent) bool

// RewriteHook runs after the built-in rewriter and returns the final body sent
// to the local client.
type RewriteHook func(*Server, RewriteEvent) []byte

// ResponseEvent is the response data passed to ResponseHook.
type ResponseEvent struct {
	Request        *http.Request
	LocalPort      int
	UpstreamURL    *url.URL
	ResponseHeader http.Header
	StatusCode     int
	Body           []byte
}

// RewriteEvent contains the original and built-in rewritten body passed to
// RewriteHook.
type RewriteEvent struct {
	Request       *http.Request
	LocalPort     int
	ContentType   string
	OriginalBody  []byte
	RewrittenBody []byte
}

// Server owns the HTTP client and all local origin routes.
type Server struct {
	cfg Config

	client *http.Client

	mu      sync.Mutex
	routes  map[int]*originRoute // local port -> upstream origin route
	origins map[string]int       // upstream origin -> local port
	closed  bool
}

// originRoute binds one local port to one upstream origin.
type originRoute struct {
	upstream *url.URL
	httpSrv  *http.Server
}

// Construction and default hooks.

func New(cfg Config) (*Server, error) {
	if cfg.BindAddr == "" {
		cfg.BindAddr = defaultBindAddr
	}
	if cfg.MaxRewriteBody == 0 {
		cfg.MaxRewriteBody = defaultMaxRewriteBody
	}
	if cfg.ResponseHook == nil {
		cfg.ResponseHook = AllowAllResponseHook
	}

	transport := cfg.Transport
	if transport == nil {
		base := http.DefaultTransport.(*http.Transport).Clone()
		base.Proxy = http.ProxyURL(cfg.ProxyURL)
		transport = base
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}

	return &Server{
		cfg:     cfg,
		client:  &http.Client{Transport: transport, Jar: jar, CheckRedirect: noRedirect},
		routes:  make(map[int]*originRoute),
		origins: make(map[string]int),
	}, nil
}

func MustNew(cfg Config) *Server {
	srv, err := New(cfg)
	if err != nil {
		panic(err)
	}
	return srv
}

// AllowAllResponseHook accepts every upstream response.
func AllowAllResponseHook(*Server, ResponseEvent) bool {
	return true
}

// NoopRewriteHook keeps the built-in rewritten body unchanged.
func NoopRewriteHook(_ *Server, ev RewriteEvent) []byte {
	return ev.RewrittenBody
}

// Server lifecycle.

func (s *Server) Start(ctx context.Context, rawURL string) (string, error) {
	upstream, err := normalizeUpstream(rawURL)
	if err != nil {
		return "", err
	}

	port, _, err := s.getOrCreateRoute(upstream)
	if err != nil {
		return "", err
	}

	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()

	return s.localURLFor(port, upstream), nil
}

func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	routes := make([]*originRoute, 0, len(s.routes))
	for _, rt := range s.routes {
		routes = append(routes, rt)
	}
	s.mu.Unlock()

	var errs []error
	for _, rt := range routes {
		errs = append(errs, rt.httpSrv.Close())
	}
	return errors.Join(errs...)
}

// Route table management.

func (s *Server) getOrCreateRoute(upstream *url.URL) (int, *originRoute, error) {
	key := originKey(upstream)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return 0, nil, http.ErrServerClosed
	}
	if port, rt := s.routeForOriginLocked(key); rt != nil {
		s.mu.Unlock()
		return port, rt, nil
	}
	s.mu.Unlock()

	port, rt, err := s.createRoute(upstream)
	if err != nil {
		return 0, nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		_ = rt.httpSrv.Close()
		return 0, nil, http.ErrServerClosed
	}
	if existingPort, existing := s.routeForOriginLocked(key); existing != nil {
		_ = rt.httpSrv.Close()
		return existingPort, existing, nil
	}
	s.routes[port] = rt
	s.origins[key] = port
	return port, rt, nil
}

func (s *Server) routeForOriginLocked(key string) (int, *originRoute) {
	port, ok := s.origins[key]
	if !ok {
		return 0, nil
	}
	return port, s.routes[port]
}

func (s *Server) createRoute(upstream *url.URL) (int, *originRoute, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(s.cfg.BindAddr, "0"))
	if err != nil {
		return 0, nil, err
	}
	port := listenerPort(ln)

	rt := &originRoute{
		upstream: originURL(upstream),
	}
	rt.httpSrv = &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.serveLocalRoute(port, rt, w, r)
		}),
		ReadHeaderTimeout: 15 * time.Second,
	}

	go func() {
		err := rt.httpSrv.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Serve errors are intentionally not returned here because this route
			// can be created while processing a response rewrite.
		}
	}()

	return port, rt, nil
}

// Request forwarding.

func (s *Server) serveLocalRoute(port int, rt *originRoute, w http.ResponseWriter, r *http.Request) {
	upstreamURL := upstreamURLForRequest(rt, r)
	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL.String(), r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	upstreamReq.Header = cloneHeader(r.Header)
	s.prepareUpstreamHeaders(upstreamReq)
	upstreamReq.Host = upstreamURL.Host

	resp, err := s.client.Do(upstreamReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	s.writeUpstreamResponse(port, rt, w, r, upstreamURL, resp)
}

func (s *Server) prepareUpstreamHeaders(upstreamReq *http.Request) {
	stripHopHeaders(upstreamReq.Header)
	upstreamReq.Header.Del("Accept-Encoding")

	if referer := upstreamReq.Header.Get("Referer"); referer != "" {
		if rewritten, ok := s.upstreamURLFromLocal(referer); ok {
			upstreamReq.Header.Set("Referer", rewritten)
		}
	}
	if origin := upstreamReq.Header.Get("Origin"); origin != "" {
		if rewritten, ok := s.upstreamOriginFromLocal(origin); ok {
			upstreamReq.Header.Set("Origin", rewritten)
		}
	}
}

func upstreamURLForRequest(rt *originRoute, r *http.Request) *url.URL {
	target := cloneURL(rt.upstream)
	target.Path = joinURLPath(rt.upstream.Path, r.URL.Path)
	target.RawPath = ""
	target.RawQuery = r.URL.RawQuery
	target.Fragment = ""
	return target
}

// Response forwarding.

func (s *Server) writeUpstreamResponse(port int, rt *originRoute, w http.ResponseWriter, req *http.Request, upstreamURL *url.URL, resp *http.Response) {
	bufferBody := shouldBufferBody(req, resp, s.cfg.MaxRewriteBody)
	body, err := s.readResponseBody(resp, bufferBody)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	ev := responseEvent(port, req, upstreamURL, resp, body)
	if !s.cfg.ResponseHook(s, ev) {
		http.Error(w, "blocked by hook", http.StatusForbidden)
		return
	}

	rewroteLocation := s.rewriteRedirectLocation(rt, resp)

	body = s.rewriteBufferedBody(req, resp, body, bufferBody)
	writeResponseHeader(w, resp, rewroteLocation)
	writeResponseBody(w, req, resp, body, bufferBody)
}

func (s *Server) readResponseBody(resp *http.Response, shouldRead bool) ([]byte, error) {
	if !shouldRead {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, s.cfg.MaxRewriteBody+1))
	if err == nil && int64(len(body)) > s.cfg.MaxRewriteBody {
		err = fmt.Errorf("response body exceeds rewrite limit %d", s.cfg.MaxRewriteBody)
	}
	return body, err
}

func responseEvent(port int, req *http.Request, upstreamURL *url.URL, resp *http.Response, body []byte) ResponseEvent {
	return ResponseEvent{
		Request:        req,
		LocalPort:      port,
		UpstreamURL:    cloneURL(upstreamURL),
		ResponseHeader: cloneHeader(resp.Header),
		StatusCode:     resp.StatusCode,
		Body:           append([]byte(nil), body...),
	}
}

func (s *Server) rewriteBufferedBody(req *http.Request, resp *http.Response, body []byte, shouldRewrite bool) []byte {
	if !shouldRewrite {
		return body
	}
	if isRewritableContent(resp.Header.Get("Content-Type")) {
		body = s.rewriteBody(req, resp.Header.Get("Content-Type"), body)
		resp.Header.Del("Content-Encoding")
		resp.Header.Del("Content-Length")
		resp.ContentLength = int64(len(body))
	}
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return body
}

func writeResponseHeader(w http.ResponseWriter, resp *http.Response, rewroteLocation bool) {
	copyResponseHeader(w.Header(), resp.Header)
	if rewroteLocation {
		w.Header().Set("Location", resp.Header.Get("Location"))
	}
	w.WriteHeader(resp.StatusCode)
}

func writeResponseBody(w http.ResponseWriter, req *http.Request, resp *http.Response, body []byte, hasBufferedBody bool) {
	if req.Method == http.MethodHead {
		return
	}
	if hasBufferedBody {
		_, _ = w.Write(body)
		return
	}
	_, _ = io.Copy(w, resp.Body)
}

// Redirect and local/upstream URL mapping.

func (s *Server) rewriteRedirectLocation(rt *originRoute, resp *http.Response) bool {
	rawLocation := resp.Header.Get("Location")
	if rawLocation == "" {
		return false
	}

	loc, err := url.Parse(rawLocation)
	if err != nil {
		return false
	}
	if loc.Scheme == "" && loc.Host == "" {
		return false
	}
	if loc.Scheme == "" {
		loc.Scheme = rt.upstream.Scheme
	}

	target, err := normalizeUpstream(loc.String())
	if err != nil {
		return false
	}
	target.Path = loc.Path
	target.RawQuery = loc.RawQuery
	target.Fragment = loc.Fragment

	targetPort, _, err := s.getOrCreateRoute(target)
	if err != nil {
		return false
	}
	resp.Header.Set("Location", s.localURLFor(targetPort, target))
	return true
}

func (s *Server) localURLFor(port int, target *url.URL) string {
	u := cloneURL(target)
	u.Scheme = "http"
	u.Host = net.JoinHostPort(s.localHost(), strconv.Itoa(port))
	return u.String()
}

func (s *Server) localHost() string {
	host := s.cfg.BindAddr
	if hostOnly, _, err := net.SplitHostPort(host); err == nil {
		host = hostOnly
	}
	return strings.Trim(host, "[]")
}

func (s *Server) upstreamURLFromLocal(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false
	}
	rt := s.routeForLocalHost(u.Host)
	if rt == nil {
		return "", false
	}
	u.Scheme = rt.upstream.Scheme
	u.Host = rt.upstream.Host
	return u.String(), true
}

func (s *Server) upstreamOriginFromLocal(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false
	}
	rt := s.routeForLocalHost(u.Host)
	if rt == nil {
		return "", false
	}
	return rt.upstream.Scheme + "://" + rt.upstream.Host, true
}

func (s *Server) routeForLocalHost(host string) *originRoute {
	port := localPortFromHost(host)
	if port == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.routes[port]
}

func localPortFromHost(host string) int {
	_, portText, err := net.SplitHostPort(host)
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return 0
	}
	return port
}

// Upstream URL normalization and origin helpers.

func normalizeUpstream(raw string) (*url.URL, error) {
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported upstream scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("missing upstream host")
	}
	return u, nil
}

func originKey(u *url.URL) string {
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

func originURL(u *url.URL) *url.URL {
	return &url.URL{
		Scheme: u.Scheme,
		Host:   u.Host,
		Path:   "/",
	}
}

func cloneURL(u *url.URL) *url.URL {
	if u == nil {
		return nil
	}
	cp := *u
	return &cp
}

func cloneHeader(h http.Header) http.Header {
	cp := make(http.Header, len(h))
	for k, values := range h {
		cp[k] = append([]string(nil), values...)
	}
	return cp
}

// Response header adaptation.

func copyResponseHeader(dst, src http.Header) {
	connectionHeaders := hopHeaderNames(src)
	for k, values := range src {
		if connectionHeaders[strings.ToLower(k)] || shouldDropResponseHeader(k) {
			continue
		}
		if strings.EqualFold(k, "Set-Cookie") {
			for _, value := range values {
				dst.Add(k, rewriteSetCookie(value))
			}
			continue
		}
		for _, value := range values {
			dst.Add(k, value)
		}
	}
}

func rewriteSetCookie(value string) string {
	parts := strings.Split(value, ";")
	out := parts[:0]
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "domain=") || lower == "secure" {
			continue
		}
		out = append(out, trimmed)
	}
	return strings.Join(out, "; ")
}

// Response buffering and content rewrite decisions.

func shouldBufferBody(req *http.Request, resp *http.Response, max int64) bool {
	if req.Method == http.MethodHead {
		return false
	}
	if max <= 0 {
		return false
	}
	if resp.ContentLength > max {
		return false
	}
	ct := resp.Header.Get("Content-Type")
	if isStreamingContent(ct) {
		return false
	}
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified {
		return false
	}
	return isRewritableContent(ct) || resp.Header.Get("Location") != ""
}

func isRewritableContent(contentType string) bool {
	ct := strings.ToLower(strings.Split(contentType, ";")[0])
	if strings.HasPrefix(ct, "text/") {
		return true
	}
	switch ct {
	case "application/json",
		"application/javascript",
		"application/ecmascript",
		"application/xml",
		"application/xhtml+xml",
		"image/svg+xml":
		return true
	default:
		return false
	}
}

func isStreamingContent(contentType string) bool {
	ct := strings.ToLower(strings.Split(contentType, ";")[0])
	return ct == "text/event-stream"
}

// Hop-by-hop and unsafe response header helpers.

func stripHopHeaders(h http.Header) {
	for name := range hopHeaderNames(h) {
		h.Del(name)
	}
}

func hopHeaderNames(h http.Header) map[string]bool {
	names := map[string]bool{
		"connection":          true,
		"keep-alive":          true,
		"proxy-authenticate":  true,
		"proxy-authorization": true,
		"proxy-connection":    true,
		"te":                  true,
		"trailer":             true,
		"transfer-encoding":   true,
		"upgrade":             true,
	}
	for _, value := range h.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			token = strings.ToLower(strings.TrimSpace(token))
			if token != "" {
				names[token] = true
			}
		}
	}
	return names
}

func shouldDropResponseHeader(k string) bool {
	switch strings.ToLower(k) {
	case "content-security-policy",
		"content-security-policy-report-only",
		"strict-transport-security",
		"public-key-pins",
		"public-key-pins-report-only",
		"expect-ct":
		return true
	default:
		return false
	}
}

func noRedirect(req *http.Request, via []*http.Request) error {
	return http.ErrUseLastResponse
}

// Miscellaneous URL and listener helpers.

func joinURLPath(a, b string) string {
	if a == "" || a == "/" {
		if b == "" {
			return "/"
		}
		return b
	}
	if b == "" || b == "/" {
		return a
	}
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	default:
		return a + b
	}
}

func listenerPort(ln net.Listener) int {
	_, portText, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		return 0
	}
	port, _ := strconv.Atoi(portText)
	return port
}
