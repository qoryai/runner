package proxy_test

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qoryai/runner/internal/policy"
	"github.com/qoryai/runner/internal/proxy"
)

// through returns a client that sends everything through the proxy, trusting the test
// server's certificate.
func through(t *testing.T, p *proxy.Proxy, tlsServer *httptest.Server) *http.Client {
	t.Helper()
	u, err := url.Parse(p.URL())
	if err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{Proxy: http.ProxyURL(u)}
	if tlsServer != nil {
		tr.TLSClientConfig = &tls.Config{RootCAs: tlsServer.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}
	}
	return &http.Client{Transport: tr}
}

type observer struct {
	mu   sync.Mutex
	seen []proxy.Decision
}

func (o *observer) observe(d proxy.Decision) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = append(o.seen, d)
}

func (o *observer) last(t *testing.T) proxy.Decision {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.seen) == 0 {
		t.Fatal("nothing observed")
	}
	return o.seen[len(o.seen)-1]
}

// TestEnforceAllowsListedHostsAndDeniesTheRest pins the proxy in enforce mode against
// a loopback origin: a plain request and a CONNECT tunnel to an allowed host go
// through and are observed with their rule; the same to a host outside the list are
// refused with 403 before anything is dialled, and observed as denied.
func TestEnforceAllowsListedHostsAndDeniesTheRest(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "plain "+r.URL.Path) }))
	defer origin.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "secure "+r.URL.Path) }))
	defer secure.Close()
	var o observer
	p, err := proxy.Listen("", policy.Enforce, []string{"127.0.0.1"}, o.observe)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	c := through(t, p, secure)

	resp, err := c.Get(origin.URL + "/a")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "plain /a" {
		t.Errorf("plain through proxy: %q", body)
	}
	if d := o.last(t); d.Method != "HTTP" || !d.Allowed || d.Rule != "127.0.0.1" || d.Host != "127.0.0.1" {
		t.Errorf("plain decision %+v", d)
	}

	resp, err = c.Get(secure.URL + "/b")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "secure /b" {
		t.Errorf("tunnel through proxy: %q", body)
	}
	if d := o.last(t); d.Method != "CONNECT" || !d.Allowed || d.Rule != "127.0.0.1" {
		t.Errorf("tunnel decision %+v", d)
	}

	// The same origins by a name the list does not hold.
	deniedPlain := strings.Replace(origin.URL, "127.0.0.1", "localhost", 1)
	resp, err = c.Get(deniedPlain + "/c")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "denied by policy") {
		t.Errorf("denied plain: %d %q", resp.StatusCode, body)
	}
	if d := o.last(t); d.Allowed || d.Rule != "" || d.Host != "localhost" {
		t.Errorf("denied plain decision %+v", d)
	}
	deniedTunnel := strings.Replace(secure.URL, "127.0.0.1", "localhost", 1)
	if _, err := c.Get(deniedTunnel + "/d"); err == nil || !strings.Contains(err.Error(), "Forbidden") {
		t.Errorf("denied tunnel: %v", err)
	}
	if d := o.last(t); d.Allowed || d.Method != "CONNECT" {
		t.Errorf("denied tunnel decision %+v", d)
	}
}

// TestObserveAllowsEverythingAndStillRecords pins observe mode: nothing is denied, and
// a host no entry matches is recorded with an empty rule.
func TestObserveAllowsEverythingAndStillRecords(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer origin.Close()
	var o observer
	p, err := proxy.Listen("", policy.Observe, nil, o.observe)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	resp, err := through(t, p, nil).Get(strings.Replace(origin.URL, "127.0.0.1", "localhost", 1))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status %d", resp.StatusCode)
	}
	if d := o.last(t); !d.Allowed || d.Rule != "" || d.Host != "localhost" {
		t.Errorf("decision %+v", d)
	}
}

// TestEnvNamesTheProxyInBothCases pins the six variables and the loopback exemption.
func TestEnvNamesTheProxyInBothCases(t *testing.T) {
	p, err := proxy.Listen("", policy.Observe, nil, func(proxy.Decision) {})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	env := strings.Join(p.Env(), "\n")
	for _, want := range []string{"HTTP_PROXY=" + p.URL(), "http_proxy=" + p.URL(), "HTTPS_PROXY=" + p.URL(), "https_proxy=" + p.URL(), "NO_PROXY=localhost,127.0.0.1,::1", "no_proxy=localhost,127.0.0.1,::1"} {
		if !strings.Contains(env, want) {
			t.Errorf("missing %s", want)
		}
	}
}

// TestNonProxyRequestIsRefused pins that a request without an absolute target, which
// is what a program sends to an origin and not to a proxy, gets a 400 and no decision.
func TestNonProxyRequestIsRefused(t *testing.T) {
	var o observer
	p, err := proxy.Listen("", policy.Observe, nil, o.observe)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	resp, err := http.Get(p.URL() + "/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || len(o.seen) != 0 {
		t.Errorf("status %d, %d decisions", resp.StatusCode, len(o.seen))
	}
}

// TestGuardRefusesThisMachineAndLinkLocal pins the guard of a proxy that serves an
// enclosure: in observe mode, which denies nothing else, loopback by address and by
// name, the metadata address and a name that resolves to loopback are all refused, the
// literal ones with a denied decision naming the wall's rule, and nothing reaches the
// origin.
func TestGuardRefusesThisMachineAndLinkLocal(t *testing.T) {
	hits := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer origin.Close()
	var o observer
	p, err := proxy.Listen("", policy.Observe, nil, o.observe)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.Guard(nil)
	port := origin.URL[strings.LastIndex(origin.URL, ":"):]
	for _, c := range []struct {
		url    string
		status int
		rule   string
	}{
		{origin.URL, 403, proxy.GuardRule},
		{"http://localhost" + port, 403, proxy.GuardRule},
		{"http://169.254.169.254/latest/meta-data/", 403, proxy.GuardRule},
		{"http://[::1]" + port, 403, proxy.GuardRule},
		// A name that resolves to loopback passes the decision and is refused when dialled.
		{"http://localtest.me" + port, 502, ""},
	} {
		resp, err := through(t, p, nil).Get(c.url)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		d := o.last(t)
		if c.url == "http://localtest.me"+port && resp.StatusCode != 502 {
			t.Skipf("localtest.me did not resolve to loopback here: status %d", resp.StatusCode)
		}
		if resp.StatusCode != c.status || d.Rule != c.rule || (c.status == 403 && d.Allowed) {
			t.Errorf("%s: status %d, decision %+v", c.url, resp.StatusCode, d)
		}
	}
	if hits != 0 {
		t.Errorf("%d requests reached this machine's own listener through a guarded proxy", hits)
	}
}

// TestGuardOpensThisMachineToAHostThePolicyNames pins the other half: an allow entry
// that names the host itself reaches this machine through a guarded proxy, in observe
// mode as well, recorded with that entry as its rule; a *. suffix over the host does
// not, and no entry opens the link-local range.
func TestGuardOpensThisMachineToAHostThePolicyNames(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "the local server") }))
	defer origin.Close()
	port := origin.URL[strings.LastIndex(origin.URL, ":"):]
	for _, mode := range []policy.Mode{policy.Observe, policy.Enforce} {
		var o observer
		// The effective list holds a name a declaration narrowed out of the policy's
		// suffix; only what the policy itself names opens this machine.
		p, err := proxy.Listen("", mode, []string{"127.0.0.1", "app.localtest.me", "169.254.169.254"}, o.observe)
		if err != nil {
			t.Fatal(err)
		}
		p.Guard([]string{"127.0.0.1", "*.localtest.me", "169.254.169.254"})
		resp, err := through(t, p, nil).Get(origin.URL)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if d := o.last(t); string(body) != "the local server" || !d.Allowed || d.Rule != "127.0.0.1" {
			t.Errorf("%s: a named host: %q, decision %+v", mode, body, d)
		}
		for _, u := range []string{"http://app.localtest.me" + port, "http://169.254.169.254/"} {
			resp, err := through(t, p, nil).Get(u)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 || string(body) == "the local server" {
				t.Errorf("%s: %s reached through a guarded proxy: %d %q", mode, u, resp.StatusCode, body)
			}
		}
		p.Close()
	}
}

// TestRequireServesOnlyConnectionsThatOpenWithTheToken pins the gate: with a token
// required, a connection that opens with the preamble is served as ever, and one that
// does not is closed unanswered and told of.
func TestRequireServesOnlyConnectionsThatOpenWithTheToken(t *testing.T) {
	p, err := proxy.Listen("", policy.Observe, nil, func(proxy.Decision) {})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	refused := make(chan struct{}, 4)
	p.Require("the-runs-token", func() { refused <- struct{}{} })
	ask := func(open string) string {
		c, err := net.Dial("tcp", p.Addr())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		io.WriteString(c, open+"GET / HTTP/1.1\r\nHost: x\r\n\r\n")
		b, _ := io.ReadAll(c)
		return string(b)
	}
	if got := ask(proxy.Preamble + " the-runs-token\n"); !strings.HasPrefix(got, "HTTP/1.1 400") {
		t.Errorf("with the token the proxy answered %q, want its refusal of a request that names no target", got)
	}
	for _, open := range []string{"", proxy.Preamble + " another-runs-token\n"} {
		if got := ask(open); got != "" {
			t.Errorf("opened with %q the proxy answered %q", open, got)
		}
		select {
		case <-refused:
		case <-time.After(5 * time.Second):
			t.Errorf("opened with %q: nobody was told", open)
		}
	}
}

// TestTerminateSetsTheCredentialAndHoldsThePaths pins termination: to a host a
// credential is for, the proxy answers with the run's authority, sets the token on a
// path the credential covers and nowhere else, denies the rest under enforce, records
// every request, and leaves every other host a tunnel it does not read.
func TestTerminateSetsTheCredentialAndHoldsThePaths(t *testing.T) {
	var mu sync.Mutex
	got := map[string]string{}
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got[r.URL.Path] = r.Header.Get("Authorization") + "|" + r.Host
		mu.Unlock()
		if r.URL.Path == "/acme/shop/expired" {
			w.WriteHeader(http.StatusUnauthorized)
		}
		io.WriteString(w, "from the origin")
	}))
	defer origin.Close()
	host, port, _ := net.SplitHostPort(origin.Listener.Addr().String())

	var decisions []proxy.Decision
	p, err := proxy.Listen("", policy.Enforce, []string{host}, func(d proxy.Decision) { mu.Lock(); decisions = append(decisions, d); mu.Unlock() })
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ca, err := proxy.NewCA("test")
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(origin.Certificate())
	p.TrustUpstream(roots)
	rejected := 0
	p.Terminate(ca, []proxy.Credential{{
		Name: "product", Hosts: []string{host}, Scheme: "basic", Username: "x-access-token", Paths: []string{"/acme/shop/*", "/graphql"},
		Token: func() string { return "the-token" }, Rejected: func() { rejected++ },
	}}, nil)

	trusted := x509.NewCertPool()
	trusted.AppendCertsFromPEM(ca.PEM())
	proxyURL, _ := url.Parse(p.URL())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: trusted}}}
	get := func(path string, header ...string) (int, string) {
		req, _ := http.NewRequest("GET", "https://"+net.JoinHostPort(host, port)+path, nil)
		req.Header.Set("Authorization", "Bearer the-sessions-placeholder")
		if len(header) == 2 {
			if header[0] == "Host" {
				req.Host = header[1]
			} else {
				req.Header.Set(header[0], header[1])
			}
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:the-token"))
	if code, body := get("/acme/shop/pulls?state=open"); code != 200 || body != "from the origin" {
		t.Errorf("a covered path: %d %q", code, body)
	}
	if code, body := get("/other-org/repo"); code != 403 || !strings.Contains(body, "no path rule") {
		t.Errorf("another organization's path: %d %q", code, body)
	}
	for _, two := range []string{"/acme/shop/..%2f..%2fother-org/repo", "/acme/shop//x", "/acme/shop/%2e%2e/x"} {
		if code, _ := get(two); code != 403 {
			t.Errorf("%s: %d, want the wall's refusal", two, code)
		}
	}
	if code, _ := get("/acme/shop/fronted", "Host", "another.example"); code != 200 {
		t.Errorf("a request naming another Host: %d", code)
	}
	get("/acme/shop/expired")
	mu.Lock()
	defer mu.Unlock()
	if got["/acme/shop/pulls"] != want+"|"+net.JoinHostPort(host, port) {
		t.Errorf("the origin saw %q on a covered path", got["/acme/shop/pulls"])
	}
	if _, reached := got["/other-org/repo"]; reached {
		t.Error("a denied path reached the origin")
	}
	if !strings.HasSuffix(got["/acme/shop/fronted"], "|"+net.JoinHostPort(host, port)) {
		t.Errorf("the origin was asked for the host %q", got["/acme/shop/fronted"])
	}
	if rejected != 1 {
		t.Errorf("the credential heard of %d rejections, want 1", rejected)
	}
	var first proxy.Decision
	for _, d := range decisions {
		if d.Method == "CONNECT" {
			t.Errorf("a terminated connection was recorded as a tunnel: %+v", d)
		}
		if d.Path == "/acme/shop/pulls" {
			first = d
		}
		if strings.Contains(d.Path, "?") {
			t.Errorf("a query in the record: %+v", d)
		}
	}
	if first.Method != "HTTPS" || first.RequestMethod != "GET" || first.Credential != "product" || first.PathRule != "/acme/shop/*" || !first.Allowed {
		t.Errorf("the covered request was recorded as %+v", first)
	}
}
