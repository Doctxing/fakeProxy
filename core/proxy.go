package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultBindAddr       = "0.0.0.0"
	defaultMaxRewriteBody = 8 << 20
)

// --- Public types -----------------------------------------------------------

// Config is passed to New to configure a Server.
type Config struct {
	BindAddr       string
	ProxyURL       *url.URL
	Transport      http.RoundTripper
	ResponseHook   ResponseHook
	RewriteHook    RewriteHook
	RedirectRules  []RedirectRule
	MaxRewriteBody int64
}

// RedirectRule rewrites an upstream 3xx Location header that matches Pattern
// into Replacement. $1, $2, ... reference Pattern capture groups.
type RedirectRule struct {
	Pattern, Replacement string
}

// StartResult is returned by Server.Start.
type StartResult struct {
	ProxyURL string
	EntryURL string
}

// ResponseHook runs after an upstream response is received. Return false to
// reject the response (client gets 403).
type ResponseHook func(*Server, ResponseEvent) bool

// RewriteHook runs after the built-in content rewriter. Return the final body
// that will be sent to the client.
type RewriteHook func(*Server, RewriteEvent) []byte

// ResponseEvent is passed to ResponseHook.
type ResponseEvent struct {
	Request        *http.Request
	LocalPort      int
	UpstreamURL    *url.URL
	ResponseHeader http.Header
	StatusCode     int
	Body           []byte
}

// RewriteEvent is passed to RewriteHook.
type RewriteEvent struct {
	Request       *http.Request
	LocalPort     int
	ContentType   string
	OriginalBody  []byte
	RewrittenBody []byte
}

// --- Internal types ---------------------------------------------------------

// route binds one local port to one upstream origin.
type route struct {
	upstream *url.URL
	srv      *http.Server
}

// --- Server -----------------------------------------------------------------

// Server is a reverse proxy that rewrites response content so all browser
// requests stay within local proxy ports. Create with New, start with Start.
type Server struct {
	bindAddr       string
	upstreamProxy  *url.URL
	transport      http.RoundTripper
	responseHook   ResponseHook
	rewriteHook    RewriteHook
	maxRewriteBody int64

	redirectRE []*regexp.Regexp
	redirectTo []string

	client *http.Client

	mu      sync.Mutex
	routes  map[int]*route
	origins map[string]int
	closed  bool

	entryLn   net.Listener
	entrySrv  *http.Server
	entryPort int
	proxyURL  string
}

// --- Constructor & helpers --------------------------------------------------

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

	res := make([]*regexp.Regexp, 0, len(cfg.RedirectRules))
	tos := make([]string, 0, len(cfg.RedirectRules))
	for i, r := range cfg.RedirectRules {
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return nil, fmt.Errorf("redirect rule %d: %w", i, err)
		}
		res = append(res, re)
		tos = append(tos, r.Replacement)
	}

	return &Server{
		bindAddr:       cfg.BindAddr,
		upstreamProxy:  cfg.ProxyURL,
		transport:      transport,
		responseHook:   cfg.ResponseHook,
		rewriteHook:    cfg.RewriteHook,
		maxRewriteBody: cfg.MaxRewriteBody,
		redirectRE:     res,
		redirectTo:     tos,
		client:         &http.Client{Transport: transport, Jar: jar, CheckRedirect: noRedirect},
		routes:         make(map[int]*route),
		origins:        make(map[string]int),
	}, nil
}

func MustNew(cfg Config) *Server {
	srv, err := New(cfg)
	if err != nil {
		panic(err)
	}
	return srv
}

func AllowAllResponseHook(*Server, ResponseEvent) bool { return true }
func NoopRewriteHook(_ *Server, ev RewriteEvent) []byte { return ev.RewrittenBody }

func hostOnly(hostPort string) string {
	h, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		return hostPort
	}
	return strings.Trim(h, "[]")
}

func portFromHost(hostPort string) int {
	_, pt, err := net.SplitHostPort(hostPort)
	if err != nil {
		return 0
	}
	port, _ := strconv.Atoi(pt)
	return port
}

// --- Lifecycle --------------------------------------------------------------

func (s *Server) Start(ctx context.Context, rawURL string) (*StartResult, error) {
	upstream, err := normalizeUpstream(rawURL)
	if err != nil {
		return nil, err
	}

	port, _, err := s.getOrCreateRoute(upstream)
	if err != nil {
		return nil, err
	}

	host := strings.Trim(s.bindAddr, "[]")
	if h, _, e := net.SplitHostPort(host); e == nil {
		host = h
	}
	s.proxyURL = s.localURL(host, port, upstream)
	entryURL := s.startEntryPort(host)

	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()

	return &StartResult{ProxyURL: s.proxyURL, EntryURL: entryURL}, nil
}

func (s *Server) startEntryPort(host string) string {
	ln, err := net.Listen("tcp", net.JoinHostPort(s.bindAddr, "0"))
	if err != nil {
		return ""
	}
	s.entryLn = ln
	s.entryPort = listenerPort(ln)
	s.entrySrv = &http.Server{
		Handler:           http.HandlerFunc(s.serveEntryRedirect),
		ReadHeaderTimeout: 15 * time.Second,
	}
	go func() { _ = s.entrySrv.Serve(ln) }()
	return fmt.Sprintf("http://%s:%d", host, s.entryPort)
}

func (s *Server) serveEntryRedirect(w http.ResponseWriter, r *http.Request) {
	u, _ := url.Parse(s.proxyURL)
	u.Host = net.JoinHostPort(hostOnly(r.Host), u.Port())
	w.Header().Set("Location", u.String())
	w.WriteHeader(http.StatusFound)
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	all := make([]*route, 0, len(s.routes))
	for _, rt := range s.routes {
		all = append(all, rt)
	}
	s.mu.Unlock()

	var errs []error
	if s.entrySrv != nil {
		errs = append(errs, s.entrySrv.Shutdown(ctx))
	}
	for _, rt := range all {
		errs = append(errs, rt.srv.Shutdown(ctx))
	}
	return errors.Join(errs...)
}

func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.Shutdown(ctx)
}

// --- Route table ------------------------------------------------------------

func (s *Server) getOrCreateRoute(upstream *url.URL) (int, *route, error) {
	key := originKey(upstream)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return 0, nil, http.ErrServerClosed
	}
	if port, ok := s.origins[key]; ok {
		s.mu.Unlock()
		return port, s.routes[port], nil
	}
	s.mu.Unlock()

	port, rt, err := s.createRoute(upstream)
	if err != nil {
		return 0, nil, err
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = rt.srv.Close()
		return 0, nil, http.ErrServerClosed
	}
	if exist, ok := s.origins[key]; ok {
		s.mu.Unlock()
		_ = rt.srv.Close()
		return exist, s.routes[exist], nil
	}
	s.routes[port] = rt
	s.origins[key] = port
	s.mu.Unlock()
	return port, rt, nil
}

func (s *Server) createRoute(upstream *url.URL) (int, *route, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(s.bindAddr, "0"))
	if err != nil {
		return 0, nil, err
	}
	port := listenerPort(ln)

	rt := &route{upstream: originURL(upstream)}
	rt.srv = &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.serveRoute(port, rt, w, r) }),
		ReadHeaderTimeout: 15 * time.Second,
	}

	go func() { _ = rt.srv.Serve(ln) }()
	return port, rt, nil
}

// --- Request handling -------------------------------------------------------

func (s *Server) serveRoute(port int, rt *route, w http.ResponseWriter, r *http.Request) {
	upstreamURL := urlForRequest(rt, r)
	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL.String(), r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	upstreamReq.Header = cloneHeader(r.Header)
	stripHopHeaders(upstreamReq.Header)
	upstreamReq.Header.Del("Accept-Encoding")
	s.rewriteLocalHeaders(upstreamReq)
	upstreamReq.Host = upstreamURL.Host

	resp, err := s.client.Do(upstreamReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	s.writeResponse(port, rt, w, r, upstreamURL, resp)
}

func (s *Server) rewriteLocalHeaders(req *http.Request) {
	if ref := req.Header.Get("Referer"); ref != "" {
		if u, ok := s.localToUpstream(ref); ok {
			req.Header.Set("Referer", u)
		}
	}
	if org := req.Header.Get("Origin"); org != "" {
		if u, ok := s.upstreamOriginFromLocal(org); ok {
			req.Header.Set("Origin", u)
		}
	}
}

func urlForRequest(rt *route, r *http.Request) *url.URL {
	target := cloneURL(rt.upstream)
	target.Path = joinURLPath(rt.upstream.Path, r.URL.Path)
	target.RawPath = ""
	target.RawQuery = r.URL.RawQuery
	target.Fragment = ""
	return target
}

// --- Response handling ------------------------------------------------------

func (s *Server) writeResponse(port int, rt *route, w http.ResponseWriter, req *http.Request, upstreamURL *url.URL, resp *http.Response) {
	buffer := shouldBuffer(req, resp, s.maxRewriteBody)
	body, err := readBody(resp, buffer, s.maxRewriteBody)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	ev := ResponseEvent{
		Request:        req,
		LocalPort:      port,
		UpstreamURL:    cloneURL(upstreamURL),
		ResponseHeader: cloneHeader(resp.Header),
		StatusCode:     resp.StatusCode,
		Body:           append([]byte(nil), body...),
	}
	if !s.responseHook(s, ev) {
		http.Error(w, "blocked by hook", http.StatusForbidden)
		return
	}

	rewrote := s.rewriteRedirect(hostOnly(req.Host), rt, resp)
	body = s.rewriteBody(req, resp, body, buffer)
	writeHeader(w, resp, rewrote)
	writeBody(w, req, resp, body, buffer)
}

func readBody(resp *http.Response, should bool, max int64) ([]byte, error) {
	if !should {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err == nil && int64(len(body)) > max {
		err = fmt.Errorf("response body exceeds rewrite limit %d", max)
	}
	return body, err
}

func writeHeader(w http.ResponseWriter, resp *http.Response, rewroteLocation bool) {
	copyResponseHeader(w.Header(), resp.Header)
	if rewroteLocation {
		w.Header().Set("Location", resp.Header.Get("Location"))
	}
	w.WriteHeader(resp.StatusCode)
}

func writeBody(w http.ResponseWriter, req *http.Request, resp *http.Response, body []byte, hasBody bool) {
	if req.Method == http.MethodHead {
		return
	}
	if hasBody {
		_, _ = w.Write(body)
	} else {
		_, _ = io.Copy(w, resp.Body)
	}
}

// --- URL rewriting ----------------------------------------------------------

func (s *Server) rewriteRedirect(host string, rt *route, resp *http.Response) bool {
	raw := resp.Header.Get("Location")
	if raw == "" {
		return false
	}

	for i, re := range s.redirectRE {
		if re.MatchString(raw) {
			if rep := re.ReplaceAllString(raw, s.redirectTo[i]); rep != "" && rep != raw {
				resp.Header.Set("Location", rep)
				return true
			}
		}
	}

	loc, err := url.Parse(raw)
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

	port, _, err := s.getOrCreateRoute(target)
	if err != nil {
		return false
	}
	resp.Header.Set("Location", s.localURL(host, port, target))
	return true
}

func (s *Server) localURL(host string, port int, target *url.URL) string {
	u := cloneURL(target)
	u.Scheme = "http"
	u.Host = net.JoinHostPort(host, strconv.Itoa(port))
	return u.String()
}

func (s *Server) localToUpstream(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false
	}
	rt := s.routeForHost(u.Host)
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
	rt := s.routeForHost(u.Host)
	if rt == nil {
		return "", false
	}
	return rt.upstream.Scheme + "://" + rt.upstream.Host, true
}

func (s *Server) routeForHost(hostPort string) *route {
	_, pt, err := net.SplitHostPort(hostPort)
	if err != nil {
		return nil
	}
	port, _ := strconv.Atoi(pt)
	if port == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.routes[port]
}

// --- Content rewriting bridge -----------------------------------------------

func (s *Server) rewriteBody(req *http.Request, resp *http.Response, body []byte, shouldRewrite bool) []byte {
	if !shouldRewrite {
		return body
	}
	if !isRewritable(resp.Header.Get("Content-Type")) {
		resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
		return body
	}
	body = s.rewriteContent(req, resp.Header.Get("Content-Type"), body)
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return body
}

// --- Decision helpers -------------------------------------------------------

func shouldBuffer(req *http.Request, resp *http.Response, max int64) bool {
	if req.Method == http.MethodHead || max <= 0 || resp.ContentLength > max {
		return false
	}
	if isStreaming(resp.Header.Get("Content-Type")) {
		return false
	}
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified {
		return false
	}
	return isRewritable(resp.Header.Get("Content-Type")) || resp.Header.Get("Location") != ""
}

func isRewritable(ct string) bool {
	ct = strings.ToLower(strings.SplitN(ct, ";", 2)[0])
	if strings.HasPrefix(ct, "text/") {
		return true
	}
	switch ct {
	case "application/json", "application/javascript", "application/ecmascript",
		"application/xml", "application/xhtml+xml", "image/svg+xml":
		return true
	}
	return false
}

func isStreaming(ct string) bool {
	return strings.EqualFold(strings.SplitN(ct, ";", 2)[0], "text/event-stream")
}

// --- Hop-by-hop headers -----------------------------------------------------

func stripHopHeaders(h http.Header) {
	for name := range hopHeaderNames(h) {
		h.Del(name)
	}
}

func hopHeaderNames(h http.Header) map[string]bool {
	names := map[string]bool{
		"connection": true, "keep-alive": true, "proxy-authenticate": true,
		"proxy-authorization": true, "proxy-connection": true, "te": true,
		"trailer": true, "transfer-encoding": true, "upgrade": true,
	}
	for _, v := range h.Values("Connection") {
		for _, t := range strings.Split(v, ",") {
			if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
				names[t] = true
			}
		}
	}
	return names
}

func shouldDropResponseHeader(k string) bool {
	switch strings.ToLower(k) {
	case "content-security-policy", "content-security-policy-report-only",
		"strict-transport-security", "public-key-pins",
		"public-key-pins-report-only", "expect-ct":
		return true
	}
	return false
}

func copyResponseHeader(dst, src http.Header) {
	conn := hopHeaderNames(src)
	for k, vals := range src {
		if conn[strings.ToLower(k)] || shouldDropResponseHeader(k) {
			continue
		}
		if strings.EqualFold(k, "Set-Cookie") {
			for _, v := range vals {
				dst.Add(k, stripCookie(v))
			}
			continue
		}
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

func stripCookie(v string) string {
	parts := strings.Split(v, ";")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		lower := strings.ToLower(p)
		if strings.HasPrefix(lower, "domain=") || lower == "secure" {
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, "; ")
}

// --- URL helpers ------------------------------------------------------------

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

func originKey(u *url.URL) string { return strings.ToLower(u.Scheme + "://" + u.Host) }

func originURL(u *url.URL) *url.URL {
	return &url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/"}
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
	for k, vals := range h {
		cp[k] = append([]string(nil), vals...)
	}
	return cp
}

func noRedirect(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }

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
	aslash, bslash := strings.HasSuffix(a, "/"), strings.HasPrefix(b, "/")
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
	_, pt, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		return 0
	}
	port, _ := strconv.Atoi(pt)
	return port
}
