package session

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
