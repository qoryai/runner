package proxy_test

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

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
