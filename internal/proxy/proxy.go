// Package proxy is the HTTP proxy the session is started behind.
//
// The proxy listens on a loopback port, or on the address a wall asks for, and the
// session's environment names it in the proxy variables, upper and lower case, with
// loopback in NO_PROXY so a local server still answers. Every connection through it is one [Decision] handed to an observer:
// a CONNECT tunnel, decided on its authority, or a plain request, decided on the
// authority of its absolute-form target. In observe mode every decision allows; in
// enforce mode a host no allow entry matches is denied with 403 and nothing is opened
// for it. The session continues either way.
//
// A proxy that serves an enclosure is guarded ([Proxy.Guard]): it dials from this
// machine on behalf of something that is not on it, so what is on this machine is not
// reached by default. The link-local range, which holds a cloud's metadata service, is
// refused whatever the policy says. This machine's own addresses, loopback and every
// address an interface holds, are refused unless an allow entry names the host itself,
// not a *. suffix over it: a local MCP server or model endpoint is reached through the
// proxy like everything else, decided and recorded, when the policy names it, in either
// mode. Without the guard the way around a wall is through the proxy.
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
	"sync/atomic"
	"syscall"
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
	guarded atomic.Bool
	// token, when set, is what every connection must open with; refused is told of one
	// that did not.
	token   atomic.Pointer[string]
	refused func()
	// opened are the hosts a guarded proxy reaches on this machine.
	opened []string
}

// GuardRule is the rule a guarded proxy's denial names in its decision: not an allow
// entry, the wall's own refusal.
const GuardRule = "wall:own-address"

// Loopback is the address the proxy binds when it is given none: a port of the
// system's choosing on loopback.
const Loopback = "127.0.0.1:0"

// Listen starts a proxy on addr, host:port, in the given mode with the given allow
// list, handing every decision to observe. An empty addr is [Loopback]; port 0 is a
// port of the system's choosing. Close stops it.
func Listen(addr string, mode policy.Mode, allow []string, observe func(Decision)) (*Proxy, error) {
	if err := mode.Validate(); err != nil {
		return nil, err
	}
	if addr == "" {
		addr = Loopback
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	p := &Proxy{mode: mode, allow: allow, observe: observe, ln: ln}
	gate := &gate{Listener: ln, p: p}
	// The guard checks the address a name resolved to, at the moment of the connection,
	// so a name that resolves to this machine is refused like the address itself.
	d := &net.Dialer{Timeout: 30 * time.Second, ControlContext: func(ctx context.Context, _, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil || !p.guarded.Load() {
			return nil
		}
		switch ip := net.ParseIP(host); {
		case linkLocal(ip):
			return fmt.Errorf("%s is a link-local address; the wall refuses it", host)
		case own(ip) && ctx.Value(namedKey{}) == nil:
			return fmt.Errorf("%s is this machine's own address and no allow entry names the host; the wall refuses it", host)
		}
		return nil
	}}
	p.dial = d.DialContext
	p.srv = &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.srv.Serve(gate)
	}()
	return p, nil
}

// Addr is the address the proxy listens on, host:port.
func (p *Proxy) Addr() string { return p.ln.Addr().String() }

// URL is the proxy's URL for the proxy variables.
func (p *Proxy) URL() string { return "http://" + p.Addr() }

// NoProxy is the value of NO_PROXY the session gets: loopback by every name, so a
// local server, a local model endpoint or the runner's own socket-fronting programs are
// reached directly.
const NoProxy = "localhost,127.0.0.1,::1"

// Env returns the variables that point a program at the proxy, by the address it
// listens on.
func (p *Proxy) Env() []string { return EnvFor(p.URL()) }

// EnvFor returns the variables that point a program at the proxy at u, in both cases,
// because curl reads http_proxy in lower case only and other programs document the
// upper case. A wall names the proxy by the address the enclosure reaches it on.
func EnvFor(u string) []string {
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

// Guard makes the proxy refuse the link-local range, and this machine's own addresses
// unless the host is one of names: the entries of the policy's own allow list, as its
// owner wrote them. The effective allow list does not serve, because a harness's
// declaration narrows a *. entry to the names under it, and a name a repository
// declared is not a name the machine's owner wrote. Entries that are *. suffixes are
// ignored. The session runner calls Guard once, before it starts anything behind a wall.
func (p *Proxy) Guard(names []string) {
	for _, n := range names {
		if !strings.HasPrefix(n, "*.") {
			p.opened = append(p.opened, n)
		}
	}
	p.guarded.Store(true)
}

// namedKey marks the context of a connection whose host an allow entry names itself.
type namedKey struct{}

// named reports whether the policy's owner named the host, which alone opens this
// machine to an enclosure, and the connection is one the allow list lets through.
func (p *Proxy) named(host string, allowed bool) bool {
	_, ok := policy.Match(p.opened, host)
	return ok && allowed
}

// linkLocal reports whether ip is in the range a guarded proxy never dials.
func linkLocal(ip net.IP) bool {
	return ip != nil && (ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast())
}

// own reports whether ip is this machine's: loopback, unspecified, or an address one of
// its interfaces holds.
func own(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsUnspecified() {
		return true
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// decide applies the guard, the mode and the allow list to a host. The guard decides
// here what it can without resolving, a literal address and localhost, so the denial is
// in the record; a name that resolves to such an address is refused when dialled.
func (p *Proxy) decide(method, host string, port int) Decision {
	rule, ok := policy.Match(p.allow, host)
	if p.guarded.Load() {
		ip := net.ParseIP(host)
		local := strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") || own(ip)
		if linkLocal(ip) || (local && !p.named(host, ok)) {
			return Decision{Host: strings.ToLower(host), Port: port, Method: method, Rule: GuardRule}
		}
	}
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
	out := r.Clone(p.dialContext(r.Context(), d))
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
	upstream, err := p.dial(p.dialContext(r.Context(), d), "tcp", net.JoinHostPort(host, portText))
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

// dialContext carries into the dial whether the policy names the host, which is what
// lets a guarded proxy reach this machine for it.
func (p *Proxy) dialContext(ctx context.Context, d Decision) context.Context {
	if p.named(d.Host, d.Rule != "") {
		return context.WithValue(ctx, namedKey{}, true)
	}
	return ctx
}

// deny answers a refused connection: 403 with one line naming the host and the mode.
func deny(w http.ResponseWriter, d Decision, mode policy.Mode) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	if d.Rule == GuardRule {
		fmt.Fprintf(w, "qory: egress to %s:%d denied by the wall: link-local addresses are never reached through the proxy, and the runner's own machine only for a host the policy's allow list names\n", d.Host, d.Port)
		return
	}
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
