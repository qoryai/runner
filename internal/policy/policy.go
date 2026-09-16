// Package policy reads the run policy and answers what it allows.
//
// The policy is the document of contracts/runner/v1/policy.schema.json, read once from
// a file outside the checkout and pinned for the run. [Load] reads it: a path that
// cannot be read is a [*Error] and no run; no path is the absent policy, mode observe
// with nothing denied. The schema is the reader: a document the schema refuses is
// refused here with the schema's message.
//
// A policy narrows only. [Loaded.Narrow] intersects it with the egress a harness
// declared, and [Match] says which entry of an allow list covers a host. Nothing here
// grants: the widest a policy can be is the absent one.
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/qoryai/runner/contracts"
)

// Mode is the egress mode of a policy.
type Mode string

// The two modes. Observe records every connection and denies none; Enforce denies a
// connection to a host outside the allow list and records the denial.
const (
	Observe Mode = "observe"
	Enforce Mode = "enforce"
)

// Policy is the policy document.
type Policy struct {
	Version int    `json:"version"`
	Egress  Egress `json:"egress"`
}

// Egress is the policy's egress section.
type Egress struct {
	Mode  Mode     `json:"mode"`
	Allow []string `json:"allow"`
}

// Loaded is a policy as read for a run: the document, where it came from and its
// digest, which is the version stamp of the run's policy.
type Loaded struct {
	Policy Policy
	// Source is "file" when a file was read, "none" when there was none.
	Source string
	// Path is the file read, when Source is "file".
	Path string
	// Digest is the hex sha256 of the file's bytes, when Source is "file".
	Digest string
}

// Error is a policy that could not be read or is not a policy. A run does not start
// on it.
type Error struct {
	Path string
	Err  error
}

func (e *Error) Error() string { return "policy " + e.Path + ": " + e.Err.Error() }

// Unwrap returns the underlying error.
func (e *Error) Unwrap() error { return e.Err }

// Load reads the policy at path, or returns the absent policy when path is empty. A
// path that does not exist is an error: absent means not configured, not misnamed.
func Load(path string) (*Loaded, error) {
	if path == "" {
		return &Loaded{Policy: Policy{Version: 1, Egress: Egress{Mode: Observe}}, Source: "none"}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, &Error{Path: path, Err: err}
	}
	p, err := Parse(path, b)
	if err != nil {
		return nil, &Error{Path: path, Err: err}
	}
	sum := sha256.Sum256(b)
	return &Loaded{Policy: *p, Source: "file", Path: path, Digest: hex.EncodeToString(sum[:])}, nil
}

// Parse validates the bytes of a policy document against the schema and decodes it.
// name chooses YAML or JSON by its extension.
func Parse(name string, b []byte) (*Policy, error) {
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

// Narrow returns the effective allow list: the policy's entries when nothing was
// declared, else the declared entries the policy covers, in declared order without
// duplicates. A declared entry the policy does not cover is dropped; a declaration can
// only lower the ceiling.
func (l *Loaded) Narrow(declared []string) []string {
	if declared == nil {
		return append([]string{}, l.Policy.Egress.Allow...)
	}
	var out []string
	seen := map[string]bool{}
	for _, d := range declared {
		d = strings.ToLower(d)
		if seen[d] {
			continue
		}
		for _, entry := range l.Policy.Egress.Allow {
			if Covers(entry, d) {
				out = append(out, d)
				seen[d] = true
				break
			}
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// Covers reports whether an allow entry covers a declared entry: a name is covered by
// the same name or by a suffix pattern above it; a pattern is covered by the same
// pattern or by a suffix pattern above it. "*.github.com" covers "api.github.com" and
// "*.api.github.com", not "github.com".
func Covers(entry, declared string) bool {
	entry, declared = strings.ToLower(entry), strings.ToLower(declared)
	if entry == declared {
		return true
	}
	suffix, isPattern := strings.CutPrefix(entry, "*.")
	if !isPattern {
		return false
	}
	name := strings.TrimPrefix(declared, "*.")
	return strings.HasSuffix(name, "."+suffix)
}

// Match returns the first entry of allow that matches host, and whether one did. A
// host is compared lower-case and without a trailing dot; an IP literal matches only
// an identical entry; a pattern "*.x" matches any host with at least one label before
// ".x" and never "x" itself.
func Match(allow []string, host string) (string, bool) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	ip := net.ParseIP(host) != nil
	for _, entry := range allow {
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
