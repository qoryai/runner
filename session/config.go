package session

import "github.com/qoryai/runner/internal/policy"

// Policy is the run's policy document, contracts/runner/v1/policy.schema.json, as the
// caller hands it to the runner. The runner validates it against the schema before
// anything starts and pins it for the run with the digest of its canonical JSON.
type Policy struct {
	// Version is the document version, 1.
	Version int `json:"version"`
	// Egress is the egress mode and the allow list.
	Egress PolicyEgress `json:"egress"`
}

// PolicyEgress is the egress section of a [Policy].
type PolicyEgress struct {
	// Mode is observe or enforce.
	Mode string `json:"mode"`
	// Allow are lower-case host names, or *. suffixes, in the contract's grammar. Nil
	// and empty are the same: nothing, which under enforce reaches nothing.
	Allow []string `json:"allow,omitempty"`
}

// ReadPolicy reads a policy document from bytes, YAML or JSON by name's extension,
// JSON when it has none, and validates it against the schema: a run's own policy file,
// read by the command that starts the run.
func ReadPolicy(name string, b []byte) (*Policy, error) {
	p, err := policy.Parse(name, b)
	if err != nil {
		return nil, &policy.Error{Name: name, Err: err}
	}
	return &Policy{Version: p.Version, Egress: PolicyEgress{Mode: string(p.Egress.Mode), Allow: p.Egress.Allow}}, nil
}

// Under returns the policy as it stands under a ceiling, the machine's own: a policy
// narrows only. A nil ceiling, or one in mode observe, forbids nothing and the policy
// stands as it is. Under a ceiling in mode enforce the mode is enforce: a policy in
// mode observe asks for no limit of its own and gets the ceiling, and one in mode
// enforce gets its entries the ceiling covers; an entry it does not cover is dropped,
// as a harness declaration's is.
func (p *Policy) Under(ceiling *Policy) *Policy {
	if ceiling == nil || ceiling.Egress.Mode != string(policy.Enforce) {
		return p
	}
	if p.Egress.Mode != string(policy.Enforce) {
		c := *ceiling
		return &c
	}
	top := &policy.Loaded{Policy: policy.Policy{Egress: policy.Egress{Allow: ceiling.Egress.Allow}}}
	allow := p.Egress.Allow
	if allow == nil {
		allow = []string{}
	}
	return &Policy{Version: p.Version, Egress: PolicyEgress{Mode: string(policy.Enforce), Allow: top.Narrow(allow)}}
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
