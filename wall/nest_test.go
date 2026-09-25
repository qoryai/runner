package wall

import (
	"encoding/json"
	"errors"
	"testing"
)

// TestNestReadsItsArguments pins the one shape Nest takes: the user, then the launch
// after --.
func TestNestReadsItsArguments(t *testing.T) {
	user, argv, err := parseNest([]string{"--user", "1000:1000", "--", "claude", "-p", "hi"})
	if err != nil || user != "1000:1000" || len(argv) != 3 || argv[0] != "claude" {
		t.Errorf("got %q %v %v", user, argv, err)
	}
	for _, args := range [][]string{nil, {"--user", "1000"}, {"--user", "1000", "claude"}, {"--user", "1000", "--"}, {"--user", "1000", "--", ""}, {"-u", "1000", "--", "claude"}} {
		if _, _, err := parseNest(args); err == nil {
			t.Errorf("%q was read", args)
		}
	}
}

// TestNestFindsTheAgentsIDs pins how the agent's user and group are found: numbers as
// given, names in the image's files, and a number with no group only for a user of the
// image's, since the daemon's socket goes to that group. Root is refused either way.
func TestNestFindsTheAgentsIDs(t *testing.T) {
	image := func(kind, key string) (int, int, bool) {
		switch kind + ":" + key {
		case "name:agent", "uid:1000":
			return 1000, 1001, true
		case "group:docker":
			return 0, 2375, true
		case "name:root", "uid:0":
			return 0, 0, true
		}
		return 0, 0, false
	}
	for spec, want := range map[string][2]int{
		"1000:1000":    {1000, 1000},
		"agent":        {1000, 1001},
		"1000":         {1000, 1001},
		"agent:docker": {1000, 2375},
		"1000:2375":    {1000, 2375},
		"5000:5000":    {5000, 5000},
	} {
		uid, gid, err := nestIDs(spec, image)
		if err != nil || uid != want[0] || gid != want[1] {
			t.Errorf("%s: got %d:%d %v, want %d:%d", spec, uid, gid, err, want[0], want[1])
		}
	}
	for _, spec := range []string{"5000", "nobody", "1000:nogroup", "0:0", "root", "1000:0", "0"} {
		if uid, gid, err := nestIDs(spec, image); err == nil {
			t.Errorf("%s: got %d:%d", spec, uid, gid)
		}
	}
}

// TestNestGivesTheInnerContainersTheProxyByAddress pins the agent's docker
// configuration: the proxy the enclosure's environment names, its name resolved, since
// the containers the agent starts do not resolve it, with NO_PROXY carried; nothing
// when there is no proxy; an error when the name does not resolve.
func TestNestGivesTheInnerContainersTheProxyByAddress(t *testing.T) {
	resolve := func(name string) ([]string, error) {
		if name == "qory-proxy" {
			return []string{"172.25.0.2"}, nil
		}
		return nil, errors.New("no such host")
	}
	env := func(vars map[string]string) func(string) string {
		return func(k string) string { return vars[k] }
	}
	proxies := func(b []byte) map[string]string {
		t.Helper()
		var c struct {
			Proxies struct{ Default map[string]string } `json:"proxies"`
		}
		if err := json.Unmarshal(b, &c); err != nil {
			t.Fatal(err)
		}
		return c.Proxies.Default
	}

	b, err := nestProxies(env(map[string]string{"HTTPS_PROXY": "http://qory-proxy:3128", "NO_PROXY": "localhost,127.0.0.1"}), resolve)
	if err != nil {
		t.Fatal(err)
	}
	if p := proxies(b); p["httpProxy"] != "http://172.25.0.2:3128" || p["httpsProxy"] != "http://172.25.0.2:3128" || p["noProxy"] != "localhost,127.0.0.1" {
		t.Errorf("got %v", p)
	}
	b, err = nestProxies(env(map[string]string{"HTTP_PROXY": "http://10.0.0.5:3128"}), resolve)
	if err != nil {
		t.Fatal(err)
	}
	if p := proxies(b); p["httpsProxy"] != "http://10.0.0.5:3128" || p["noProxy"] != "" {
		t.Errorf("an address is kept as it is: got %v", p)
	}
	if b, err := nestProxies(env(nil), resolve); b != nil || err != nil {
		t.Errorf("no proxy, and a configuration: %s %v", b, err)
	}
	if _, err := nestProxies(env(map[string]string{"HTTPS_PROXY": "http://elsewhere:3128"}), resolve); err == nil {
		t.Error("a name that does not resolve was written")
	}
	if _, err := nestProxies(env(map[string]string{"HTTPS_PROXY": "::"}), resolve); err == nil {
		t.Error("a proxy that is not a URL was written")
	}
}
