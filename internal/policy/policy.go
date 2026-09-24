// Package policy reads the run policy and answers what it allows.
//
// The policy is the document of contracts/runner/v1/policy.schema.json, given to the
// runner once by its caller and pinned for the run. [Read] reads it from bytes: a
// document the schema refuses is a [*Error] and no run; [None] is the absent policy,
// mode observe with no list to deny by. The schema is the reader: a refused document
// carries the schema's message.
//
// A policy narrows only. [Match] says which entry of a list, the allow list's or the
// deny list's, covers a host, and [Covers] whether one entry stands above another,
// which is how a policy is put under a ceiling. Nothing here grants: the widest a
// policy can be is the absent one.
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"github.com/qoryai/runner/contracts"
)

// Mode is the egress mode of a policy.
type Mode string

// The two modes. Observe records every connection and denies only what the deny list
// names; Enforce denies a connection to a host outside the allow list as well, and
// records the denial. The deny list is decided first in either mode.
const (
	Observe Mode = "observe"
	Enforce Mode = "enforce"
)

// Policy is the policy document.
type Policy struct {
	Version     int        `json:"version"`
	Egress      Egress     `json:"egress"`
	Credentials []Selected `json:"credentials,omitempty"`
	Tools       []Selected `json:"tools,omitempty"`
}

// Egress is the policy's egress section.
type Egress struct {
	Mode  Mode     `json:"mode"`
	Allow []string `json:"allow"`
	// Deny are the hosts the session may not reach, in Allow's grammar, in either
	// mode: a host an entry covers is denied before Allow and before Mode are
	// consulted, with the entry as its rule.
	Deny  []string            `json:"deny,omitempty"`
	Paths map[string][]string `json:"paths,omitempty"`
}

// Selected is one credential or tool of the machine's the policy lets the run use.
type Selected struct {
	Name     string `json:"name"`
	Argument string `json:"argument,omitempty"`
}

// Loaded is a policy as read for a run: the document, where it came from and its
// digest, which is the version stamp of the run's policy.
type Loaded struct {
	Policy Policy
	// Source is "config" when a document was given, "fetched" when the server's run
	// configuration holds it, "none" when there was none.
	Source string
	// Digest is the hex sha256 of the document as canonical JSON, the runner's own
	// serialization of it, when Source is "config" or "fetched": the version stamp of
	// the run's policy, the same for the same policy however it was written.
	Digest string
	// URL is where the run configuration was fetched from, and RunConfiguration the
	// server's digest of it, opaque, when Source is "fetched".
	URL              string
	RunConfiguration string
}

// Error is a document that is not a policy. A run does not start on it.
type Error struct {
	// Name is what the caller called the document: a file name, or "policy".
	Name string
	Err  error
}

func (e *Error) Error() string { return "policy " + e.Name + ": " + e.Err.Error() }

// Unwrap returns the underlying error.
func (e *Error) Unwrap() error { return e.Err }

// None is the absent policy: observe everything with no list to deny by, source none.
func None() *Loaded {
	return &Loaded{Policy: Policy{Version: 1, Egress: Egress{Mode: Observe}}, Source: "none"}
}

// Read reads a policy document from bytes, YAML or JSON by name's extension, JSON
// when it has none, validates it and pins it with its digest. A refused document is a
// [*Error] naming name.
func Read(name string, b []byte) (*Loaded, error) {
	p, err := Parse(name, b)
	if err != nil {
		return nil, &Error{Name: name, Err: err}
	}
	canonical, err := json.Marshal(p)
	if err != nil {
		return nil, &Error{Name: name, Err: err}
	}
	sum := sha256.Sum256(canonical)
	return &Loaded{Policy: *p, Source: "config", Digest: hex.EncodeToString(sum[:])}, nil
}

// Parse validates the bytes of a policy document against the schema and decodes it.
// name chooses YAML or JSON by its extension, JSON when it has none.
func Parse(name string, b []byte) (*Policy, error) {
	if !strings.Contains(name, ".") {
		name += ".json"
	}
	doc, err := contracts.Decode(name, b)
	if err != nil {
		return nil, err
	}
	schema, err := contracts.Compile("policy.schema.json")
	if err != nil {
		return nil, err
	}
	if err := schema.Validate(doc); err != nil {
		return nil, err
	}
	j, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	var p Policy
	if err := json.Unmarshal(j, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// Covers reports whether an allow entry covers another: a name is covered by the same
// name or by a suffix pattern above it; a pattern is covered by the same pattern or by
// a suffix pattern above it. "*.github.com" covers "api.github.com" and
// "*.api.github.com", not "github.com".
func Covers(entry, other string) bool {
	entry, other = strings.ToLower(entry), strings.ToLower(other)
	if entry == other {
		return true
	}
	suffix, isPattern := strings.CutPrefix(entry, "*.")
	if !isPattern {
		return false
	}
	name := strings.TrimPrefix(other, "*.")
	return strings.HasSuffix(name, "."+suffix)
}

// Match returns the first entry of a list, the allow list or the deny list, that
// matches host, and whether one did. A host is compared lower-case and without a
// trailing dot; an IP literal matches only an identical entry; a pattern "*.x"
// matches any host with at least one label before ".x" and never "x" itself.
func Match(entries []string, host string) (string, bool) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	ip := net.ParseIP(host) != nil
	for _, entry := range entries {
		e := strings.ToLower(entry)
		if e == host {
			return entry, true
		}
		if ip {
			continue
		}
		if suffix, ok := strings.CutPrefix(e, "*."); ok && strings.HasSuffix(host, "."+suffix) {
			return entry, true
		}
	}
	return "", false
}

// String names the mode for messages.
func (m Mode) String() string { return string(m) }

// Validate reports whether the mode is one of the two.
func (m Mode) Validate() error {
	switch m {
	case Observe, Enforce:
		return nil
	}
	return fmt.Errorf("egress mode %q is neither observe nor enforce", string(m))
}

// MatchPath returns the first of patterns that matches path, and whether one did. A
// pattern is a path matched whole, or up to a final * matched as a prefix. The
// comparison is exact, case included: on a host that ignores case this denies a
// spelling the host would have taken, never the reverse.
func MatchPath(patterns []string, path string) (string, bool) {
	for _, p := range patterns {
		if prefix, ok := strings.CutSuffix(p, "*"); ok {
			if strings.HasPrefix(path, prefix) {
				return p, true
			}
		} else if p == path {
			return p, true
		}
	}
	return "", false
}

// CoversPath reports whether a path pattern covers another: the same pattern, or a
// prefix pattern whose prefix the other starts with.
func CoversPath(entry, other string) bool {
	if entry == other {
		return true
	}
	prefix, ok := strings.CutSuffix(entry, "*")
	return ok && strings.HasPrefix(strings.TrimSuffix(other, "*"), prefix)
}

// CleanPath is the path of a request as a path rule reads it, and whether it can be
// read one way only. escaped is the path as sent. It is refused when it holds an
// encoded slash, backslash, dot or percent sign, a backslash, an empty segment, or a
// dot segment: a proxy and a server that disagree on any of those disagree on which
// rule applies.
func CleanPath(escaped string) (string, bool) {
	if escaped == "" {
		return "/", true
	}
	lower := strings.ToLower(escaped)
	for _, bad := range []string{"%2f", "%5c", "%2e", "%25", "\\", "//", "/./", "/../"} {
		if strings.Contains(lower, bad) {
			return "", false
		}
	}
	if !strings.HasPrefix(escaped, "/") || strings.HasSuffix(escaped, "/.") || strings.HasSuffix(escaped, "/..") {
		return "", false
	}
	for i := 0; i < len(escaped); i++ {
		if c := escaped[i]; c < 0x21 || c == 0x7f {
			return "", false
		}
	}
	return escaped, true
}
