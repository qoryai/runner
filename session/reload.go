package session

import (
	"context"
	"fmt"
	"sync"

	"github.com/qoryai/runner/internal/policy"
	"github.com/qoryai/runner/internal/server"
	"github.com/qoryai/runner/internal/sink"
)

// live is a run's server while the run goes: the configuration document as
// discovered, the run configuration in force, and the reload that keeps both current
// from the digests the server's answers carry. One reload runs at a time; answers
// that arrive during one are coalesced into the next pass.
type live struct {
	client            *server.Client
	forge, repository string
	report            func(string)
	ctx               context.Context
	cancel            context.CancelFunc
	wg                sync.WaitGroup

	mu         sync.Mutex
	conf       *server.Configuration
	confDigest string
	// runDigest is the server's digest of the run configuration in force, empty when
	// none was fetched.
	runDigest string
	posts     *sink.Server
	// apply puts a fetched policy in force; nil until the proxy exists.
	apply func(*policy.Loaded)
	// want is what the server's answers last said is in force.
	want           server.Digests
	running, dirty bool
}

// discover fetches the server's configuration document for a run.
func discover(ctx context.Context, cfg *server.Config, spec Spec) (*live, error) {
	client := &server.Client{Config: cfg, UserAgent: "qory-runner/" + spec.RunnerVersion}
	conf, digest, err := client.Discover(ctx)
	if err != nil {
		return nil, err
	}
	l := &live{client: client, forge: spec.Labels["forge"], repository: spec.Labels["repository"], report: spec.Report, conf: conf, confDigest: digest}
	l.ctx, l.cancel = context.WithCancel(ctx)
	return l, nil
}

// fetch fetches the run configuration at runURL for the run's labels and reads its
// policy, which is then the run's, with the source fetched and the server's digest.
func (l *live) fetch(ctx context.Context, runURL string) (*policy.Loaded, error) {
	rc, digest, err := l.client.RunConfiguration(ctx, runURL, l.forge, l.repository)
	if err != nil {
		return nil, err
	}
	pol, err := policy.Read("run-configuration", rc.SecurityPolicy)
	if err != nil {
		return nil, fmt.Errorf("run configuration %s: %w", runURL, err)
	}
	pol.Source, pol.URL, pol.RunConfiguration = "fetched", runURL, digest
	l.mu.Lock()
	l.runDigest = digest
	l.mu.Unlock()
	return pol, nil
}

// start arms the reload with what puts a policy in force, once the proxy exists.
func (l *live) start(apply func(*policy.Loaded)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.apply = apply
	l.kick()
}

// digests takes what an answer said is in force, on the sink's goroutine, and
// schedules a reload when it differs from what the run holds. A digest an answer did
// not carry means nothing.
func (l *live) digests(d server.Digests) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if d.Configuration != "" {
		l.want.Configuration = d.Configuration
	}
	if d.RunConfiguration != "" {
		l.want.RunConfiguration = d.RunConfiguration
	}
	l.kick()
}

// kick starts the reload loop when something differs and none runs, or marks a
// running one to go again. Called with the lock held.
func (l *live) kick() {
	if l.apply == nil || l.ctx.Err() != nil || !l.stale() {
		return
	}
	if l.running {
		l.dirty = true
		return
	}
	l.running = true
	l.wg.Add(1)
	go l.loop()
}

// stale reports whether what the server said is in force differs from what the run
// holds. Called with the lock held.
func (l *live) stale() bool {
	if l.want.Configuration != "" && l.want.Configuration != l.confDigest {
		return true
	}
	return l.conf.Run != nil && l.want.RunConfiguration != "" && l.want.RunConfiguration != l.runDigest
}

// loop runs passes until nothing is marked dirty or the run ends.
func (l *live) loop() {
	defer l.wg.Done()
	for {
		l.pass()
		l.mu.Lock()
		if !l.dirty || l.ctx.Err() != nil {
			l.running = false
			l.mu.Unlock()
			return
		}
		l.mu.Unlock()
	}
}

// pass reloads what differs: the configuration document first, whose sections are
// used from then on, then the run configuration, which is put in force. A fetch that
// fails is reported and leaves what the run holds; the next answer asks again.
func (l *live) pass() {
	l.mu.Lock()
	want, conf, confDigest, runDigest, apply := l.want, l.conf, l.confDigest, l.runDigest, l.apply
	l.dirty = false
	l.mu.Unlock()
	fetchRun := false
	if want.Configuration != "" && want.Configuration != confDigest {
		next, digest, err := l.client.Discover(l.ctx)
		if err != nil {
			l.report("the reload failed: " + err.Error())
			return
		}
		l.mu.Lock()
		l.conf, l.confDigest = next, digest
		l.mu.Unlock()
		l.posts.SetTarget(sink.Target{URL: next.Events.URL, Types: next.Events.Types})
		// A run section that appears names a run configuration the run does not hold
		// yet; one that disappears leaves the policy in force as it is.
		fetchRun = next.Run != nil && runDigest == ""
		conf = next
	}
	if conf.Run == nil || !(fetchRun || (want.RunConfiguration != "" && want.RunConfiguration != runDigest)) {
		return
	}
	pol, err := l.fetch(l.ctx, conf.Run.URL)
	if err != nil {
		l.report("the reload failed: " + err.Error())
		return
	}
	apply(pol)
}

// stop ends the reload: a pass in flight is cancelled and waited for.
func (l *live) stop() {
	l.cancel()
	l.wg.Wait()
}
