package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"

	"github.com/qoryai/runner/internal/policy"
)

// AmbiguousPath is the rule a denial names when a request's path could be read two
// ways: not a path entry, the proxy's own refusal, in either mode.
const AmbiguousPath = "wall:ambiguous-path"

// Credential is one use of a token the runner holds for the session: the hosts it goes
// to, how it is set, and the paths of those hosts the run may ask for.
type Credential struct {
	// Name is the credential's name in the machine's configuration, for the record.
	Name  string
	Hosts []string
	// Scheme is bearer, basic or header; Username goes with basic, Header with header.
	Scheme, Username, Header string
	// Paths are the paths of the hosts the run may ask for; nil means every path.
	Paths []string
	// Token returns the token as it is now; it changes when it is renewed. Rejected is
	// told when the host answered 401, which is a reason to ask for another.
	Token    func() string
	Rejected func()
}

// Terminate makes the proxy end the session's TLS itself for the hosts the credentials
// are for and the hosts with path rules, answering as each with a certificate of the
// run's authority, so it can read a request's path and set a credential on it. Every
// other host stays a tunnel it does not read. The session runner calls it once, before
// anything connects.
func (p *Proxy) Terminate(ca *CA, creds []Credential, paths map[string][]string) {
	p.term = &terminator{p: p, ca: ca, creds: creds, paths: paths}
}

// Terminated reports the hosts, as configured, the proxy terminates TLS for.
func (p *Proxy) Terminated() []string {
	if p.term == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, c := range p.term.creds {
		for _, h := range c.Hosts {
			if !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	for h := range p.term.paths {
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

type terminator struct {
	p     *Proxy
	ca    *CA
	creds []Credential
	paths map[string][]string
}

// credential is the credential that is for the host, if one is.
func (t *terminator) credential(host string) *Credential {
	for i := range t.creds {
		if _, ok := policy.Match(t.creds[i].Hosts, host); ok {
			return &t.creds[i]
		}
	}
	return nil
}

// rules are the policy's path rules for the host, and whether it has any.
func (t *terminator) rules(host string) ([]string, bool) {
	for pattern, rules := range t.paths {
		if _, ok := policy.Match([]string{pattern}, host); ok {
			return rules, true
		}
	}
	return nil, false
}

// covers reports whether the host is one the proxy terminates.
func (t *terminator) covers(host string) bool {
	if t == nil {
		return false
	}
	_, ruled := t.rules(host)
	return ruled || t.credential(host) != nil
}

// path decides one request's path. A path that could be read two ways is denied in
// either mode; a path no rule matches is denied under enforce. The credential is set
// only on a path its own rules match, whatever the mode: observing is no reason to hand
// a token to a path nobody configured.
func (t *terminator) path(host, escaped string) (clean, rule string, allowed, credentialed bool) {
	clean, ok := policy.CleanPath(escaped)
	if !ok {
		return escaped, AmbiguousPath, false, false
	}
	allowed, credentialed = true, true
	if rules, ruled := t.rules(host); ruled {
		r, ok := policy.MatchPath(rules, clean)
		rule = r
		allowed = ok
	}
	if c := t.credential(host); c != nil && c.Paths != nil {
		r, ok := policy.MatchPath(c.Paths, clean)
		credentialed = ok
		if rule == "" || !ok {
			rule = r
		}
		allowed = allowed && ok
	}
	if !allowed && t.p.mode == policy.Observe {
		allowed = true
	}
	return clean, rule, allowed, credentialed
}

// serve ends the session's TLS on client as host and serves the requests inside it.
func (t *terminator) serve(ctx context.Context, client net.Conn, d Decision, authority string) {
	leaf, err := t.ca.leaf(d.Host)
	if err != nil {
		client.Close()
		return
	}
	// HTTP/1.1 only on this side: one request at a time is one decision at a time, and
	// every client that speaks to a proxy speaks it.
	conn := tls.Server(client, &tls.Config{Certificates: []tls.Certificate{*leaf}, NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12})
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			// The host asked of upstream is the one the connection was opened to and
			// decided on, whatever Host the request names: a credential is for that host,
			// and another name behind the same address is another site.
			r.Out.URL.Scheme = "https"
			r.Out.URL.Host = strings.TrimSuffix(authority, ":443")
			r.Out.Host = ""
		},
		Transport:     t.p.upstream(),
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		},
		ModifyResponse: func(resp *http.Response) error {
			if c := t.credential(d.Host); c != nil && resp.StatusCode == http.StatusUnauthorized && c.Rejected != nil && resp.Request.Header.Get(setBy) == c.Name {
				c.Rejected()
			}
			return nil
		},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean, rule, allowed, credentialed := t.path(d.Host, r.URL.EscapedPath())
		req := d
		req.Method, req.RequestMethod, req.Path, req.PathRule = "HTTPS", r.Method, clean, rule
		req.Allowed = allowed
		c := t.credential(d.Host)
		r.Header.Del(setBy)
		if allowed && credentialed && c != nil {
			if name, value, ok := c.header(); ok {
				r.Header.Set(name, value)
				r.Header.Set(setBy, c.Name)
				req.Credential = c.Name
			}
		}
		t.p.observe(req)
		if !allowed {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			if rule == AmbiguousPath {
				fmt.Fprintf(w, "qory: %s %s%s denied by the wall: the path could be read two ways\n", r.Method, d.Host, clean)
				return
			}
			fmt.Fprintf(w, "qory: %s %s%s denied by policy (mode %s): no path rule of the run's covers it\n", r.Method, d.Host, clean, t.p.mode)
			return
		}
		rp.ServeHTTP(w, r.WithContext(t.p.dialContext(r.Context(), d)))
	})
	ln := &once{conn: conn, done: make(chan struct{})}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: t.p.srv.ReadHeaderTimeout, ConnState: func(_ net.Conn, s http.ConnState) {
		if s == http.StateClosed || s == http.StateHijacked {
			ln.Close()
		}
	}}
	stop := context.AfterFunc(ctx, func() { srv.Close() })
	defer stop()
	srv.Serve(ln)
}

// setBy marks, on the way upstream only, a request the proxy set a credential on; the
// transport takes it off again.
const setBy = "X-Qory-Credential-Set"

// header is the header that carries the token.
func (c *Credential) header() (string, string, bool) {
	token := c.Token()
	if token == "" {
		return "", "", false
	}
	switch c.Scheme {
	case "bearer":
		return "Authorization", "Bearer " + token, true
	case "basic":
		return "Authorization", "Basic " + base64.StdEncoding.EncodeToString([]byte(c.Username+":"+token)), true
	case "header":
		return c.Header, token, true
	}
	return "", "", false
}

// upstream is the transport to the real hosts of terminated connections: verified
// against this machine's roots, dialled through the proxy's guarded dialler.
func (p *Proxy) upstream() http.RoundTripper {
	p.upOnce.Do(func() {
		p.up = &unmark{&http.Transport{Proxy: nil, DialContext: p.dial, ForceAttemptHTTP2: true, DisableCompression: true, TLSClientConfig: p.upstreamTLS}}
	})
	return p.up
}

// unmark takes the proxy's own mark off a request before it leaves.
type unmark struct{ rt http.RoundTripper }

// RoundTrip sends the request on without the mark.
func (u *unmark) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Header.Get(setBy) == "" {
		return u.rt.RoundTrip(r)
	}
	out := r.Clone(r.Context())
	out.Header.Del(setBy)
	resp, err := u.rt.RoundTrip(out)
	if resp != nil {
		resp.Request = r
	}
	return resp, err
}

// once is a listener of the one connection it was made with.
type once struct {
	conn net.Conn
	mu   sync.Mutex
	used bool
	done chan struct{}
	shut sync.Once
}

// Accept hands the connection on, then waits for Close.
func (l *once) Accept() (net.Conn, error) {
	l.mu.Lock()
	if !l.used {
		l.used = true
		l.mu.Unlock()
		return l.conn, nil
	}
	l.mu.Unlock()
	<-l.done
	return nil, net.ErrClosed
}

// Close ends Accept.
func (l *once) Close() error {
	l.shut.Do(func() { close(l.done) })
	return nil
}

// Addr is the connection's.
func (l *once) Addr() net.Addr { return l.conn.LocalAddr() }

// plainPath decides a plain request's path by the policy's rules. No credential is ever
// set on a connection that is not encrypted.
func (t *terminator) plainPath(host, escaped string) (string, string, bool) {
	if t == nil {
		return "", "", true
	}
	if _, ruled := t.rules(host); !ruled && t.credential(host) == nil {
		return "", "", true
	}
	clean, rule, allowed, _ := t.path(host, escaped)
	return clean, rule, allowed
}

// TrustUpstream sets the roots the proxy verifies a terminated host's own certificate
// against; nil means this machine's.
func (p *Proxy) TrustUpstream(roots *x509.CertPool) {
	p.upstreamTLS = &tls.Config{RootCAs: roots}
}
