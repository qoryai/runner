// Package proxy is the loopback HTTP proxy the session is started behind.
//
// The proxy listens on a loopback port and the session's environment names it in the
// proxy variables, upper and lower case, with loopback in NO_PROXY so a local server
// still answers. Every connection through it is one [Decision] handed to an observer:
// a CONNECT tunnel, decided on its authority, or a plain request, decided on the
// authority of its absolute-form target. In observe mode every decision allows; in
// enforce mode a host no allow entry matches is denied with 403 and nothing is opened
// for it. The session continues either way.
//
// The proxy sees host names and ports and never the content of a TLS connection: a
// tunnel is a blind relay once established. Only a proxy-aware program is seen.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qoryai/runner/internal/policy"
)

// Decision is one connection the session asked for and what the proxy did with it.
type Decision struct {
	Host string
	Port int
	// Method is "CONNECT" for a tunnel, "HTTP" for a plain request.
	Method  string
	Allowed bool
	// Rule is the allow entry that matched, or empty when none did.
	Rule string
}

// Proxy is a listening proxy.
type Proxy struct {
	mode    policy.Mode
	allow   []string
	observe func(Decision)
	ln      net.Listener
	srv     *http.Server
	dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	wg      sync.WaitGroup
}

// Listen starts a proxy on a loopback port in the given mode with the given allow list,
// handing every decision to observe. Close stops it.
func Listen(mode policy.Mode, allow []string, observe func(Decision)) (*Proxy, error) {
	if err := mode.Validate(); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	d := &net.Dialer{Timeout: 30 * time.Second}
	p := &Proxy{mode: mode, allow: allow, observe: observe, ln: ln, dial: d.DialContext}
	p.srv = &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.srv.Serve(ln)
	}()
	return p, nil
}

// Addr is the proxy's address, host:port on loopback.
func (p *Proxy) Addr() string { return p.ln.Addr().String() }

// URL is the proxy's URL for the proxy variables.
func (p *Proxy) URL() string { return "http://" + p.Addr() }

// NoProxy is the value of NO_PROXY the session gets: loopback by every name, so a
// local server, a local model endpoint or the runner's own socket-fronting programs are
// reached directly.
const NoProxy = "localhost,127.0.0.1,::1"

// Env returns the variables that point a program at the proxy, in both cases, because
// curl reads http_proxy in lower case only and other programs document the upper case.
func (p *Proxy) Env() []string {
	u := p.URL()
	return []string{
		"HTTP_PROXY=" + u, "http_proxy=" + u,
		"HTTPS_PROXY=" + u, "https_proxy=" + u,
		"NO_PROXY=" + NoProxy, "no_proxy=" + NoProxy,
	}
}

// Close stops the listener and every tunnel.
func (p *Proxy) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := p.srv.Shutdown(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		err = p.srv.Close()
	}
	p.wg.Wait()
	return err
}

// decide applies the mode and the allow list to a host.
func (p *Proxy) decide(method, host string, port int) Decision {
	rule, ok := policy.Match(p.allow, host)
	return Decision{Host: strings.ToLower(host), Port: port, Method: method, Allowed: ok || p.mode == policy.Observe, Rule: rule}
}

// ServeHTTP handles one proxy request: a CONNECT, or a plain request with an
// absolute-form target. Anything else is a 400.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.connect(w, r)
		return
	}
	if r.URL.Host == "" || r.URL.Scheme == "" {
		http.Error(w, "the proxy takes an absolute-form request target", http.StatusBadRequest)
		return
	}
	host := r.URL.Hostname()
	port, err := portOf(r.URL.Port(), r.URL.Scheme)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d := p.decide("HTTP", host, port)
	p.observe(d)
	if !d.Allowed {
		deny(w, d, p.mode)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Host = r.URL.Host
	for _, h := range hopByHop {
		out.Header.Del(h)
	}
	resp, err := p.transport().RoundTrip(out)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// connect opens a tunnel, or refuses it with a 403 before anything is dialled.
func (p *Proxy) connect(w http.ResponseWriter, r *http.Request) {
	host, portText, err := net.SplitHostPort(r.Host)
	if err != nil {
		http.Error(w, "CONNECT takes host:port", http.StatusBadRequest)
		return
	}
	port, err := portOf(portText, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d := p.decide(http.MethodConnect, host, port)
	p.observe(d)
	if !d.Allowed {
		deny(w, d, p.mode)
		return
	}
	upstream, err := p.dial(r.Context(), "tcp", net.JoinHostPort(host, portText))
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	if _, err := buf.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err == nil {
		err = buf.Flush()
	}
	if err != nil {
		client.Close()
		upstream.Close()
		return
	}
	relay(client, upstream, buf.Reader.Buffered(), buf.Reader)
}

// relay copies both ways and closes both ends when either side closes.
func relay(client, upstream net.Conn, buffered int, r io.Reader) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if buffered > 0 {
			io.CopyN(upstream, r, int64(buffered))
		}
		io.Copy(upstream, client)
		closeWrite(upstream)
	}()
	go func() {
		defer wg.Done()
		io.Copy(client, upstream)
		closeWrite(client)
	}()
	wg.Wait()
	client.Close()
	upstream.Close()
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
}

// deny answers a refused connection: 403 with one line naming the host and the mode.
func deny(w http.ResponseWriter, d Decision, mode policy.Mode) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	fmt.Fprintf(w, "qory: egress to %s:%d denied by policy (mode %s)\n", d.Host, d.Port, mode)
}

func portOf(text, scheme string) (int, error) {
	if text == "" {
		switch scheme {
		case "https":
			return 443, nil
		case "http":
			return 80, nil
		}
		return 0, errors.New("no port")
	}
	n, err := strconv.Atoi(text)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("invalid port %q", text)
	}
	return n, nil
}

var hopByHop = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

// transport forwards plain requests upstream, directly and never through another proxy.
func (p *Proxy) transport() *http.Transport {
	return &http.Transport{Proxy: nil, DialContext: p.dial, DisableCompression: true}
}
