package session

import (
	"github.com/qoryai/runner/internal/credential"
	"github.com/qoryai/runner/internal/policy"
)

// Policy is the run's policy document, contracts/runner/v1/policy.schema.json, as the
// caller hands it to the runner. The runner validates it against the schema before
// anything starts and pins it for the run with the digest of its canonical JSON.
type Policy struct {
	// Version is the document version, 1.
	Version int `json:"version"`
	// Egress is the egress mode and the allow list.
	Egress PolicyEgress `json:"egress"`
	// Credentials are the credentials of [Spec.Credentials] the run may use, by name.
	Credentials []PolicyCredential `json:"credentials,omitempty"`
}

// PolicyCredential selects one credential the machine defines.
type PolicyCredential struct {
	Name string `json:"name"`
	// Argument is what the run asks the credential for, a repository say.
	Argument string `json:"argument,omitempty"`
}

// PolicyEgress is the egress section of a [Policy].
type PolicyEgress struct {
	// Mode is observe or enforce.
	Mode string `json:"mode"`
	// Allow are lower-case host names, or *. suffixes, in the contract's grammar. Nil
	// and empty are the same: nothing, which under enforce reaches nothing.
	Allow []string `json:"allow,omitempty"`
	// Paths are the paths the session may ask of a host, by host. A host listed is one
	// the proxy terminates TLS for.
	Paths map[string][]string `json:"paths,omitempty"`
}

// ReadPolicy reads a policy document from bytes, YAML or JSON by name's extension,
// JSON when it has none, and validates it against the schema: a run's own policy file,
// read by the command that starts the run.
func ReadPolicy(name string, b []byte) (*Policy, error) {
	p, err := policy.Parse(name, b)
	if err != nil {
		return nil, &policy.Error{Name: name, Err: err}
	}
	out := &Policy{Version: p.Version, Egress: PolicyEgress{Mode: string(p.Egress.Mode), Allow: p.Egress.Allow, Paths: p.Egress.Paths}}
	for _, c := range p.Credentials {
		out.Credentials = append(out.Credentials, PolicyCredential{Name: c.Name, Argument: c.Argument})
	}
	return out, nil
}

// Under returns the policy as it stands under a ceiling, the machine's own: a policy
// narrows only. A nil ceiling, or one in mode observe, forbids nothing and the policy
// stands as it is. Under a ceiling in mode enforce the mode is enforce: a policy in
// mode observe asks for no limit of its own and gets the ceiling, and one in mode
// enforce gets its entries the ceiling covers; an entry it does not cover is dropped,
// as a harness declaration's is. Path rules narrow the same way: a host both name keeps
// the policy's paths the ceiling's cover, and a host one of them names keeps its rules.
// The credentials are the policy's own: a ceiling defines them and selects none.
func (p *Policy) Under(ceiling *Policy) *Policy {
	if ceiling == nil || ceiling.Egress.Mode != string(policy.Enforce) {
		return p
	}
	if p.Egress.Mode != string(policy.Enforce) {
		c := *ceiling
		c.Credentials = p.Credentials
		return &c
	}
	top := &policy.Loaded{Policy: policy.Policy{Egress: policy.Egress{Allow: ceiling.Egress.Allow}}}
	allow := p.Egress.Allow
	if allow == nil {
		allow = []string{}
	}
	paths := map[string][]string{}
	for host, rules := range ceiling.Egress.Paths {
		paths[host] = rules
	}
	for host, rules := range p.Egress.Paths {
		above, both := ceiling.Egress.Paths[host]
		if !both {
			paths[host] = rules
			continue
		}
		kept := []string{}
		for _, r := range rules {
			for _, a := range above {
				if policy.CoversPath(a, r) {
					kept = append(kept, r)
					break
				}
			}
		}
		paths[host] = kept
	}
	if len(paths) == 0 {
		paths = nil
	}
	return &Policy{Version: p.Version, Egress: PolicyEgress{Mode: string(policy.Enforce), Allow: top.Narrow(allow), Paths: paths}, Credentials: p.Credentials}
}

// Webhook is the webhook configuration, contracts/runner/v1/webhook.schema.json, as
// the caller hands it to the runner: where to post every event as well as writing it,
// signed with the secret. The runner validates it before the ping.
type Webhook struct {
	// Version is the document version, 1.
	Version int `json:"version"`
	// URL is https, or http to a loopback address.
	URL string `json:"url"`
	// Secret signs every delivery; at least 16 characters, shared with the receiver.
	Secret string `json:"secret"`
	// Events are the types to post, full names or "*"; nil is every type.
	Events []string `json:"events,omitempty"`
}

// Credential is one credential as the machine defines it, [Spec.Credentials]: a token
// the runner holds outside the enclosure and the proxy sets on the requests to the
// hosts it is for. A run's policy selects credentials by name and defines none. Exactly
// one of Env, File and Adapter says where the token comes from.
//
// An adapter is a program of the machine's that knows one kind of host, a source code
// host say. The runner starts it outside the enclosure and reads one JSON document
// from its standard output, contracts/runner/v1/credential.schema.json: the token, when
// it expires, and how it is used, the hosts, the scheme and the paths, because hosts
// differ in those and the runner knows none of them.
type Credential struct {
	// Name is what a policy selects it by.
	Name string
	// Env names a variable of the runner's own environment that holds the token.
	Env string
	// File is a path that holds the token, read again whenever it is used.
	File string
	// Adapter is the program and its arguments; ${argument} in an argument is replaced
	// by the argument the run's policy gives.
	Adapter []string
	// Argument is a regular expression the policy's argument must match whole; empty
	// means a policy passes none. Only an adapter takes one.
	Argument string
	// Hosts, Scheme, Username, Header and Paths say how a token from Env or File is
	// used: the scheme is bearer, basic with Username, or header with Header, and nil
	// Paths are every path. For an Adapter, which says all that itself, Hosts and Paths
	// are the most it may claim, when they are set.
	Hosts                    []string
	Scheme, Username, Header string
	Paths                    []string
	// Placeholders are variables the enclosure gets with a value that is no credential,
	// for a program that does not start without one set.
	Placeholders []string
}

// Check refuses a definition that cannot be one, so a command reading the machine's
// configuration says so before any run selects it.
func (c Credential) Check() error { return credential.Definition(c).Check() }
