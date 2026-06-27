package core

import (
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

var (
	htmlURLAttrPattern = regexp.MustCompile(`(?is)(\s)(href|src|action|poster|formaction|data-src|data-href|xlink:href)\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	htmlSetAttrPattern = regexp.MustCompile(`(?is)(\s)(srcset|imagesrcset)\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	htmlStyleAttrPat   = regexp.MustCompile(`(?is)(\s)(style)\s*=\s*(?:"([^"]*)"|'([^']*)')`)
)

func (s *Server) rewriteContent(req *http.Request, contentType string, body []byte) []byte {
	host := stripPort(req.Host)
	ct := strings.ToLower(strings.Split(contentType, ";")[0])
	rewritten := body
	switch ct {
	case "text/html", "application/xhtml+xml":
		rewritten = []byte(s.rewriteHTML(host, string(body)))
	case "text/css":
		rewritten = []byte(s.rewriteCSS(host, string(body)))
	case "application/javascript", "application/ecmascript", "text/javascript":
		rewritten = []byte(rewriteJS(string(body)))
	case "application/json":
		rewritten = []byte(s.rewriteQuotedURLStrings(host, string(body)))
	case "image/svg+xml", "application/xml", "text/xml":
		rewritten = []byte(s.rewriteXML(host, string(body)))
	default:
		if strings.HasPrefix(ct, "text/") {
			rewritten = []byte(s.rewriteQuotedURLStrings(host, string(body)))
		}
	}

	if s.rewriteHook == nil {
		return rewritten
	}
	return s.rewriteHook(s, RewriteEvent{
		Request:       req,
		LocalPort:     portFromHost(req.Host),
		ContentType:   contentType,
		OriginalBody:  append([]byte(nil), body...),
		RewrittenBody: append([]byte(nil), rewritten...),
	})
}

func (s *Server) rewriteHTML(host, html string) string {
	var out strings.Builder
	for i := 0; i < len(html); {
		next := strings.IndexByte(html[i:], '<')
		if next < 0 {
			out.WriteString(html[i:])
			break
		}
		next += i
		out.WriteString(html[i:next])

		end := strings.IndexByte(html[next:], '>')
		if end < 0 {
			out.WriteString(html[next:])
			break
		}
		end += next

		tag := html[next : end+1]
		lowerTag := strings.ToLower(tag)
		out.WriteString(s.rewriteHTMLTag(host, tag))

		switch {
		case strings.HasPrefix(lowerTag, "<style"):
			closeStart, closeEnd := findClosingTag(html, end+1, "style")
			if closeStart < 0 {
				i = end + 1
				continue
			}
			out.WriteString(s.rewriteCSS(host, html[end+1:closeStart]))
			out.WriteString(html[closeStart:closeEnd])
			i = closeEnd
		case strings.HasPrefix(lowerTag, "<script"):
			closeStart, closeEnd := findClosingTag(html, end+1, "script")
			if closeStart < 0 {
				i = end + 1
				continue
			}
			out.WriteString(rewriteJS(html[end+1 : closeStart]))
			out.WriteString(html[closeStart:closeEnd])
			i = closeEnd
		default:
			i = end + 1
		}
	}
	return out.String()
}

func rewriteJS(js string) string {
	return js
}

func (s *Server) rewriteHTMLTag(host, tag string) string {
	tag = htmlURLAttrPattern.ReplaceAllStringFunc(tag, func(match string) string {
		return rewriteHTMLAttrMatch(match, func(value string) string {
			if rewritten, ok := s.rewriteURLValue(host, value); ok {
				return rewritten
			}
			return value
		})
	})
	tag = htmlSetAttrPattern.ReplaceAllStringFunc(tag, func(match string) string {
		return rewriteHTMLAttrMatch(match, func(value string) string {
			return s.rewriteSrcset(host, value)
		})
	})
	tag = htmlStyleAttrPat.ReplaceAllStringFunc(tag, func(match string) string {
		return rewriteHTMLAttrMatch(match, func(value string) string {
			return s.rewriteCSS(host, value)
		})
	})
	return tag
}

func rewriteHTMLAttrMatch(match string, rewrite func(string) string) string {
	eq := strings.IndexByte(match, '=')
	if eq < 0 {
		return match
	}
	quote := byte(0)
	valueStart := -1
	for i := eq + 1; i < len(match); i++ {
		if match[i] == '"' || match[i] == '\'' {
			quote = match[i]
			valueStart = i + 1
			break
		}
	}
	if valueStart < 0 {
		return match
	}
	valueEnd := strings.LastIndexByte(match, quote)
	if valueEnd < valueStart {
		return match
	}
	return match[:valueStart] + rewrite(match[valueStart:valueEnd]) + match[valueEnd:]
}

func (s *Server) rewriteSrcset(host, value string) string {
	parts := strings.Split(value, ",")
	for i, part := range parts {
		leading := leadingWhitespace(part)
		trimmed := strings.TrimLeftFunc(part, unicode.IsSpace)
		if trimmed == "" {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) == 0 {
			continue
		}
		if rewritten, ok := s.rewriteURLValue(host, fields[0]); ok {
			fields[0] = rewritten
			parts[i] = leading + strings.Join(fields, " ")
		}
	}
	return strings.Join(parts, ",")
}

func (s *Server) rewriteXML(host, xmlText string) string {
	return htmlURLAttrPattern.ReplaceAllStringFunc(xmlText, func(match string) string {
		return rewriteHTMLAttrMatch(match, func(value string) string {
			if rewritten, ok := s.rewriteURLValue(host, value); ok {
				return rewritten
			}
			return value
		})
	})
}

func (s *Server) rewriteCSS(host, css string) string {
	var out strings.Builder
	for i := 0; i < len(css); {
		idx := indexFold(css[i:], "url(")
		if idx < 0 {
			out.WriteString(s.rewriteQuotedURLStrings(host, css[i:]))
			break
		}
		idx += i
		out.WriteString(s.rewriteQuotedURLStrings(host, css[i:idx]))
		out.WriteString(css[idx : idx+4])

		j := idx + 4
		for j < len(css) && isSpace(css[j]) {
			out.WriteByte(css[j])
			j++
		}

		quote := byte(0)
		if j < len(css) && (css[j] == '"' || css[j] == '\'') {
			quote = css[j]
			out.WriteByte(css[j])
			j++
		}

		valueStart := j
		if quote != 0 {
			for j < len(css) && css[j] != quote {
				j++
			}
		} else {
			for j < len(css) && css[j] != ')' {
				j++
			}
		}

		value := strings.TrimSpace(css[valueStart:j])
		if rewritten, ok := s.rewriteURLValue(host, value); ok {
			out.WriteString(rewritten)
		} else {
			out.WriteString(css[valueStart:j])
		}

		if quote != 0 && j < len(css) && css[j] == quote {
			out.WriteByte(css[j])
			j++
		}
		for j < len(css) && isSpace(css[j]) {
			out.WriteByte(css[j])
			j++
		}
		if j < len(css) && css[j] == ')' {
			out.WriteByte(css[j])
			j++
		}
		i = j
	}
	return out.String()
}

func (s *Server) rewriteQuotedURLStrings(host, text string) string {
	var out strings.Builder
	for i := 0; i < len(text); {
		ch := text[i]
		if ch != '"' && ch != '\'' && ch != '`' {
			out.WriteByte(ch)
			i++
			continue
		}

		quote := ch
		start := i
		i++
		for i < len(text) {
			if text[i] == '\\' {
				i += 2
				continue
			}
			if text[i] == quote {
				break
			}
			i++
		}
		if i >= len(text) {
			out.WriteString(text[start:])
			break
		}
		out.WriteByte(quote)
		out.WriteString(s.rewriteURLsInStringValue(host, text[start+1:i]))
		out.WriteByte(quote)
		i++
	}
	return out.String()
}

func (s *Server) rewriteURLsInStringValue(host, value string) string {
	var out strings.Builder
	for i := 0; i < len(value); {
		start, isProtocolRelative := findURLStart(value, i)
		if start < 0 {
			out.WriteString(value[i:])
			break
		}
		out.WriteString(value[i:start])

		end := start
		for end < len(value) && !isURLTerminator(value[end]) {
			end++
		}
		raw := value[start:end]
		if isProtocolRelative && !looksLikeNetworkHost(raw[2:]) {
			out.WriteString(raw)
		} else if rewritten, ok := s.rewriteURLValue(host, raw); ok {
			out.WriteString(rewritten)
		} else {
			out.WriteString(raw)
		}
		i = end
	}
	return out.String()
}

func (s *Server) rewriteURLValue(host, raw string) (string, bool) {
	if shouldSkipRawURL(raw) {
		return "", false
	}

	text := raw
	if strings.HasPrefix(text, "//") {
		if !looksLikeNetworkHost(text[2:]) {
			return "", false
		}
		text = "https:" + text
	}

	u, err := url.Parse(text)
	if err != nil || u.Host == "" {
		return "", false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	if shouldSkipRewriteURL(u) {
		return "", false
	}

	port, _, err := s.getOrCreateRoute(u)
	if err != nil {
		return "", false
	}
	return s.localURL(host, port, u), true
}

func findClosingTag(s string, from int, name string) (int, int) {
	lower := strings.ToLower(s[from:])
	needle := "</" + name
	idx := strings.Index(lower, needle)
	if idx < 0 {
		return -1, -1
	}
	start := from + idx
	end := strings.IndexByte(s[start:], '>')
	if end < 0 {
		return -1, -1
	}
	return start, start + end + 1
}

func findURLStart(s string, from int) (int, bool) {
	best := -1
	protoRelative := false
	for _, c := range [...]struct {
		prefix string
		proto  bool
	}{
		{"https://", false},
		{"http://", false},
		{"//", true},
	} {
		idx := strings.Index(s[from:], c.prefix)
		if idx < 0 {
			continue
		}
		idx += from
		if best < 0 || idx < best {
			best = idx
			protoRelative = c.proto
		}
	}
	return best, protoRelative
}

func shouldSkipRawURL(raw string) bool {
	if raw == "" || strings.HasPrefix(raw, "#") {
		return true
	}
	lower := strings.ToLower(raw)
	return strings.HasPrefix(lower, "data:") ||
		strings.HasPrefix(lower, "javascript:") ||
		strings.HasPrefix(lower, "mailto:")
}

func looksLikeNetworkHost(raw string) bool {
	host := raw
	if slash := strings.IndexByte(host, '/'); slash >= 0 {
		host = host[:slash]
	}
	if q := strings.IndexByte(host, '?'); q >= 0 {
		host = host[:q]
	}
	host = strings.Trim(host, "[]")
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if net.ParseIP(host) != nil {
		return true
	}
	return strings.Contains(host, ".")
}

func shouldSkipRewriteURL(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "schema.org" || strings.HasSuffix(host, ".schema.org"):
		return true
	case host == "w3.org" || host == "www.w3.org" || strings.HasSuffix(host, ".w3.org"):
		return true
	case host == "xmlns.com" || strings.HasSuffix(host, ".xmlns.com"):
		return true
	default:
		return false
	}
}

func indexFold(s, sub string) int {
	return strings.Index(strings.ToLower(s), strings.ToLower(sub))
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\n' || b == '\t' || b == '\r' || b == '\f'
}

func isURLTerminator(b byte) bool {
	switch b {
	case ' ', '\n', '\t', '\r', '\f', '"', '\'', '`', '<', '>', '\\', ')', ']', '}':
		return true
	default:
		return false
	}
}

func leadingWhitespace(s string) string {
	for i, r := range s {
		if !unicode.IsSpace(r) {
			return s[:i]
		}
	}
	return s
}

// stripPort removes the port number from a host:port string.
func stripPort(hostPort string) string {
	host, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		return hostPort
	}
	return host
}
