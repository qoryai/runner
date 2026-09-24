package proxy_test

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/qoryai/runner/internal/policy"
	"github.com/qoryai/runner/internal/proxy"
)

// toolHost is a name that exists nowhere: a tool serves it, and the proxy never dials it.
const toolHost = "files.tools.internal"

// seen is what a tool received of one request.
type seen struct {
	host, target, requestID, pathRule, body, trailer string
}

// listenTool starts a tool on a Unix socket that answers every request with 200 and
// remembers what it received, by path.
func listenTool(t *testing.T) (string, map[string]seen, *sync.Mutex, func()) {
	t.Helper()
	// A socket path has a short limit on some systems; the test's own directory can
	// exceed it.
	dir, err := os.MkdirTemp("", "qt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	got := map[string]seen{}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got[r.URL.Path] = seen{host: r.Host, target: r.RequestURI, requestID: r.Header.Get(proxy.RequestIDHeader), pathRule: r.Header.Get(proxy.PathRuleHeader), body: string(b), trailer: r.Trailer.Get("X-Checksum")}
		mu.Unlock()
		w.Header().Set("X-Tool", "answered")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, "from the tool")
	})}
	go srv.Serve(ln)
	stop := func() { srv.Close() }
	t.Cleanup(stop)
	return sock, got, &mu, stop
}

// TestAToolIsHandedWhatThePathRuleLetThrough pins the proxy's side of a tool: a request
// to a host the tool serves is decided by the policy's path rules first, then handed to
// the tool over its socket with the proxy's own headers and nothing the session sent
// under their prefix, streamed with its trailers, and recorded with the tool's name,
// the proxy's id of it and the status the tool answered. A request the rules refuse
// never reaches the tool, and a tool that is gone is a failed dial.
func TestAToolIsHandedWhatThePathRuleLetThrough(t *testing.T) {
	sock, got, mu, stop := listenTool(t)
	obs := &observer{}
	p, err := proxy.Listen("", policy.Enforce, []string{toolHost}, nil, obs.observe)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ca, err := proxy.NewCA("test")
	if err != nil {
		t.Fatal(err)
	}
	p.Terminate(ca, nil, map[string][]string{toolHost: {"/media/acme/shop/*"}}, []proxy.Tool{{Name: "files", Hosts: []string{toolHost}, Socket: sock}})
	if got := p.Terminated(); len(got) != 1 || got[0] != toolHost {
		t.Errorf("the terminated hosts are %v", got)
	}
	trusted := x509.NewCertPool()
	trusted.AppendCertsFromPEM(ca.PEM())
	proxyURL, _ := url.Parse(p.URL())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: trusted}}}
	do := func(req *http.Request) (int, string) {
		t.Helper()
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", req.URL, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	req, _ := http.NewRequest("GET", "https://"+toolHost+"/media/acme/shop/a.png?acl", nil)
	req.Header.Set("Qory-Path-Rule", "/")
	req.Header.Set("qory-request-id", "the-sessions-own")
	req.Header.Set("Qory-Anything", "the-sessions-own")
	if code, body := do(req); code != 201 || body != "from the tool" {
		t.Errorf("a path the rule covers: %d %q", code, body)
	}
	// A body of unknown length goes as it comes, chunked, with its trailer.
	pr, pw := io.Pipe()
	put, _ := http.NewRequest("PUT", "https://"+toolHost+"/media/acme/shop/b.bin", pr)
	put.ContentLength = -1
	put.Trailer = http.Header{"X-Checksum": nil}
	go func() {
		for i := range 3 {
			fmt.Fprintf(pw, "part %d;", i)
		}
		put.Trailer.Set("X-Checksum", "the-sum")
		pw.Close()
	}()
	if code, _ := do(put); code != 201 {
		t.Errorf("a streamed upload: %d", code)
	}
	other, _ := http.NewRequest("GET", "https://"+toolHost+"/media/acme/other/c.png", nil)
	if code, body := do(other); code != 403 || !strings.Contains(body, "no path rule") {
		t.Errorf("a path no rule covers: %d %q", code, body)
	}
	plain, _ := http.NewRequest("GET", "http://"+toolHost+"/media/acme/shop/d.png", nil)
	if code, _ := do(plain); code != 201 {
		t.Errorf("a plain request to the tool's host: %d", code)
	}

	mu.Lock()
	a, b, d := got["/media/acme/shop/a.png"], got["/media/acme/shop/b.bin"], got["/media/acme/shop/d.png"]
	_, reached := got["/media/acme/other/c.png"]
	mu.Unlock()
	if a.host != toolHost || a.target != "/media/acme/shop/a.png?acl" || a.pathRule != "/media/acme/shop/*" || a.requestID == "" || a.requestID == "the-sessions-own" {
		t.Errorf("the tool received %+v", a)
	}
	if b.body != "part 0;part 1;part 2;" || b.trailer != "the-sum" {
		t.Errorf("the tool received the upload as %q with the trailer %q", b.body, b.trailer)
	}
	if d.host != toolHost || d.pathRule != "/media/acme/shop/*" {
		t.Errorf("the tool received the plain request as %+v", d)
	}
	if reached {
		t.Error("a path no rule covers reached the tool")
	}
	byPath := map[string]proxy.Decision{}
	for _, dec := range obs.all() {
		if dec.Method == "CONNECT" {
			t.Errorf("a connection to a tool's host was recorded as a tunnel: %+v", dec)
		}
		byPath[dec.Path] = dec
	}
	if dec := byPath["/media/acme/shop/a.png"]; dec.Method != "HTTPS" || dec.Tool != "files" || dec.Status != 201 || dec.RequestID != a.requestID || dec.Credential != "" || dec.Outcome != proxy.Connected || !dec.Allowed {
		t.Errorf("the request to the tool was recorded as %+v", dec)
	}
	if dec := byPath["/media/acme/shop/d.png"]; dec.Method != "HTTP" || dec.Tool != "files" || dec.Status != 201 || dec.RequestID != d.requestID {
		t.Errorf("the plain request to the tool was recorded as %+v", dec)
	}
	if dec := byPath["/media/acme/other/c.png"]; dec.Allowed || dec.Outcome != proxy.Refused || dec.Tool != "files" || dec.Status != 0 {
		t.Errorf("the refused request was recorded as %+v", dec)
	}

	stop()
	gone, _ := http.NewRequest("GET", "https://"+toolHost+"/media/acme/shop/e.png", nil)
	if code, _ := do(gone); code != http.StatusBadGateway {
		t.Errorf("a request to a tool that is gone: %d", code)
	}
	for _, dec := range obs.all() {
		if dec.Path == "/media/acme/shop/e.png" && (dec.Outcome != proxy.DialFailed || dec.Status != 0) {
			t.Errorf("the request to a tool that is gone was recorded as %+v", dec)
		}
	}
}

// TestUnderObserveAToolGetsNoRuleForAPathNoneCovers pins that observing lets a request
// through to the tool without a path rule: the tool, which holds its own secrets, reads
// the missing header as no rule of the run's.
func TestUnderObserveAToolGetsNoRuleForAPathNoneCovers(t *testing.T) {
	sock, got, mu, _ := listenTool(t)
	obs := &observer{}
	p, err := proxy.Listen("", policy.Observe, nil, nil, obs.observe)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ca, err := proxy.NewCA("test")
	if err != nil {
		t.Fatal(err)
	}
	p.Terminate(ca, nil, map[string][]string{toolHost: {"/media/acme/shop/*"}}, []proxy.Tool{{Name: "files", Hosts: []string{toolHost}, Socket: sock}})
	trusted := x509.NewCertPool()
	trusted.AppendCertsFromPEM(ca.PEM())
	proxyURL, _ := url.Parse(p.URL())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: trusted}}}
	req, _ := http.NewRequest("GET", "https://"+toolHost+"/media/acme/other/c.png", nil)
	req.Header.Set(proxy.PathRuleHeader, "/media/acme/other/*")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	mu.Lock()
	s, reached := got["/media/acme/other/c.png"]
	mu.Unlock()
	if resp.StatusCode != 201 || !reached || s.pathRule != "" {
		t.Errorf("under observe the tool answered %d, was reached %v, with the path rule %q", resp.StatusCode, reached, s.pathRule)
	}
	for _, dec := range obs.all() {
		if dec.Path == "/media/acme/other/c.png" && (!dec.Allowed || dec.PathRule != "" || dec.Tool != "files") {
			t.Errorf("the observed request was recorded as %+v", dec)
		}
	}
}

// all is every decision observed so far.
func (o *observer) all() []proxy.Decision {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]proxy.Decision(nil), o.seen...)
}
