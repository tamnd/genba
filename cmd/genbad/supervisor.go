package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/api"
	"github.com/tamnd/genba/store"
)

// The kinds a stored connector can have. They are the same words the log lines
// and the administration screen already use for the two connectors this binary
// ships, so a row somebody added from the interface reads the same as a row
// that came from a flag.
const (
	kindCorpus = "corpus"
	kindBucket = "bucket"
)

// supervisor runs the connectors and remembers how they were configured.
//
// It is the half of the connector story the command line cannot do. A flag is
// read once, at startup, by somebody with a shell on the machine, and every
// change to it is a deployment. This is the other way round: the configuration
// is in the store, it is changed by somebody with the administrator role, and
// the change takes effect without a restart because this is what starts and
// stops the crawlers.
//
// The two coexist and they are not equal. A connector named on the command line
// is marked here and cannot be changed from the interface, because the next
// restart would read the command line again and undo whatever was typed. Saying
// so is [api.ErrUnmanaged], and it is better than either lying about it or
// hiding a running connector from the screen that lists them.
type supervisor struct {
	store  store.Store
	feeds  store.Feeds
	tenant string
	track  *indexing
	ops    *operations
	log    *slog.Logger

	// creds are the object storage credentials, read from the environment at
	// startup. They are held here rather than stored with the connector because
	// a database is backed up, replicated and read by more people than a process
	// environment is, and a secret that reaches one of those places cannot be
	// recalled from it. A bucket added from the interface uses the credentials
	// this process already holds, which also means a deployment that has none
	// cannot be talked into crawling a private bucket through the API.
	creds credentials

	mu      sync.Mutex
	config  map[string]store.Feed
	running map[string]*runner
	fixed   map[string]bool
}

// runner is one connector that is going.
type runner struct {
	cancel  context.CancelFunc
	wait    func()
	trigger chan struct{}
}

// credentials are the object storage keys, as the environment gave them.
type credentials struct {
	Access  string
	Secret  string
	Session string
}

var _ api.Supervisor = (*supervisor)(nil)

// newSupervisor builds one. The store has to be able to remember a connector
// across a restart, which is why the caller checks for [store.Feeds] first: a
// deployment whose driver cannot is better told that than offered a form whose
// answers disappear at the next deployment.
func newSupervisor(st store.Store, feeds store.Feeds, tenant string, creds credentials, track *indexing, ops *operations, log *slog.Logger) *supervisor {
	return &supervisor{
		store:   st,
		feeds:   feeds,
		tenant:  tenant,
		track:   track,
		ops:     ops,
		log:     log,
		creds:   creds,
		config:  make(map[string]store.Feed),
		running: make(map[string]*runner),
		fixed:   make(map[string]bool),
	}
}

// fix marks a source as configured on the command line.
//
// Called for every flag configured connector before the stored ones are
// restored, so that a stored connector that has since been given the same name
// on the command line does not quietly start a second crawler against the same
// source name and write two sets of documents over each other.
func (s *supervisor) fix(source string) {
	if source == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fixed[source] = true
}

// restore starts the connectors that were configured before this process came
// up.
//
// A connector that cannot be built is a line in the log and a row on the screen
// rather than a refusal to start. The directory it points at may be on a volume
// that is not mounted yet, and a server that will not come up because of one
// connector is a server whose search is down for a reason that has nothing to
// do with search.
func (s *supervisor) restore(ctx context.Context) error {
	saved, err := s.feeds.Feeds(ctx, s.tenant)
	if err != nil {
		return fmt.Errorf("reading the configured connectors: %w", err)
	}
	for _, f := range saved {
		s.mu.Lock()
		clash := s.fixed[f.Source]
		s.config[f.Source] = f
		s.mu.Unlock()
		if clash {
			s.log.Warn("a stored connector has the same name as one on the command line, the command line wins",
				"source", f.Source)
			continue
		}
		if !f.Enabled {
			// Registered anyway, so that a connector somebody switched off is
			// on the screen with an off switch rather than gone from it.
			s.ops.register(api.Connector{
				Source: f.Source, Kind: f.Kind, Tenant: f.Tenant, Managed: true,
			}, nil)
			continue
		}
		if err := s.launch(ctx, f); err != nil {
			s.log.Error("starting a configured connector", "source", f.Source, "error", err)
		}
	}
	return nil
}

// stop cancels every connector this supervisor started and waits for them.
//
// It is deferred by the caller in the same place the flag configured feeds are
// waited for, and for the same reason: a process that returned from run while a
// crawler was still writing would be a process whose last sync is half in the
// store.
func (s *supervisor) stop() {
	s.mu.Lock()
	running := make([]*runner, 0, len(s.running))
	for source, r := range s.running {
		running = append(running, r)
		delete(s.running, source)
	}
	s.mu.Unlock()
	for _, r := range running {
		r.cancel()
		r.wait()
	}
}

// Add saves a connector and starts it.
//
// The order is deliberate: build it first, so that settings nobody can run are
// refused before they are written down; then write it down, so that a process
// that dies in the next instant comes back up with it; then start it. A start
// that fails after the write leaves a configured connector that is not running,
// which is what the screen says and what an operator can fix, and is a great
// deal better than a connector that is running and will be gone after a
// restart.
func (s *supervisor) Add(ctx context.Context, f store.Feed) error {
	if err := s.owns(f.Tenant, f.Source); err != nil {
		return err
	}
	if err := f.Check(); err != nil {
		return fmt.Errorf("%w: %w", api.ErrBadConnector, err)
	}

	built, err := s.build(f)
	if err != nil {
		return err
	}
	// Built to find out whether it can be, then released. What is being tested
	// is the directory, the policy and the credentials, and holding a watcher
	// open between here and the start below would leak one every time somebody
	// edits a field of a connector that is switched off.
	release(built)

	f.Tenant = s.tenant
	if err := s.feeds.SaveFeed(ctx, f); err != nil {
		return fmt.Errorf("saving the connector: %w", err)
	}
	s.mu.Lock()
	s.config[f.Source] = f
	s.mu.Unlock()

	s.halt(f.Source)
	if !f.Enabled {
		s.ops.register(api.Connector{
			Source: f.Source, Kind: f.Kind, Tenant: f.Tenant, Managed: true,
		}, nil)
		s.ops.enable(f.Source, false)
		return nil
	}
	return s.launch(ctx, f)
}

// Remove stops a connector and forgets how it was configured.
//
// The documents it indexed stay. Forgetting how a corpus was read is not a
// decision to delete the corpus, and a call that did both would make an
// operator's undo cost a full crawl, which on a large source is hours.
func (s *supervisor) Remove(ctx context.Context, tenant, source string) error {
	if err := s.owns(tenant, source); err != nil {
		return err
	}
	if err := s.known(source); err != nil {
		return err
	}
	s.halt(source)
	if err := s.feeds.DropFeed(ctx, s.tenant, source); err != nil {
		return fmt.Errorf("forgetting the connector: %w", err)
	}
	s.mu.Lock()
	delete(s.config, source)
	s.mu.Unlock()
	s.ops.forget(source)
	return nil
}

// Start switches a configured connector on.
func (s *supervisor) Start(ctx context.Context, tenant, source string) error {
	return s.switchTo(ctx, tenant, source, true)
}

// Stop switches a configured connector off and keeps its settings.
func (s *supervisor) Stop(ctx context.Context, tenant, source string) error {
	return s.switchTo(ctx, tenant, source, false)
}

// switchTo is start and stop, which differ in one bool and in nothing else.
func (s *supervisor) switchTo(ctx context.Context, tenant, source string, on bool) error {
	if err := s.owns(tenant, source); err != nil {
		return err
	}
	if err := s.known(source); err != nil {
		return err
	}

	s.mu.Lock()
	f := s.config[source]
	s.mu.Unlock()
	if f.Enabled == on {
		// Already in the state it was asked for. Not an error, so that two
		// operators pressing the same button do not have to explain a failure
		// to each other, and no write, so that the row's own age is not moved
		// by a request that changed nothing.
		return nil
	}

	f.Enabled = on
	if err := s.feeds.SaveFeed(ctx, f); err != nil {
		return fmt.Errorf("saving the connector: %w", err)
	}
	s.mu.Lock()
	s.config[source] = f
	s.mu.Unlock()

	if !on {
		s.halt(source)
		s.ops.enable(source, false)
		return nil
	}
	return s.launch(ctx, f)
}

// Sync asks a running connector for a run now.
//
// It returns as soon as the run has been asked for. A crawl of a real source
// takes minutes, and a request that waited for one would time out somewhere in
// the middle and leave nobody able to say whether it ran.
//
// A connector that is already syncing is not asked twice. The trigger has room
// for one waiting run, so pressing the button during a crawl means one more
// crawl after it rather than a queue of them, which is the behaviour somebody
// pressing a button that appears to do nothing will produce.
func (s *supervisor) Sync(_ context.Context, tenant, source string) error {
	if err := s.owns(tenant, source); err != nil {
		return err
	}
	if err := s.known(source); err != nil {
		return err
	}
	s.mu.Lock()
	r, running := s.running[source]
	s.mu.Unlock()
	if !running {
		return fmt.Errorf("%w: %s is switched off, so there is nothing to sync", api.ErrBadConnector, source)
	}
	select {
	case r.trigger <- struct{}{}:
	default:
	}
	return nil
}

// owns rejects a request about somebody else's connector.
//
// This binary serves one tenant, so the only way the tenant can differ is a
// caller naming another one, and the answer is the same as for a connector that
// does not exist. A connector on the command line is refused separately,
// because that one is worth explaining: there is a unit file to edit.
func (s *supervisor) owns(tenant, source string) error {
	if tenant != s.tenant {
		return fmt.Errorf("%w: %s", genba.ErrNotFound, source)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fixed[source] {
		return api.ErrUnmanaged
	}
	return nil
}

// known rejects a request about a connector nobody configured.
func (s *supervisor) known(source string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.config[source]; !ok {
		return fmt.Errorf("%w: %s", genba.ErrNotFound, source)
	}
	return nil
}

// launch starts one connector under its own context.
//
// Its own, so that stopping one connector does not stop the others, and
// deliberately not the caller's: the call that starts a connector is an
// administration request whose context ends when the response is written, and a
// crawler that ended with it would stop a few milliseconds after it started.
// What it keeps from the caller is the values, so a request id follows the
// crawl into the log, and what it drops is the deadline. Shutdown is
// [supervisor.stop], which cancels these by hand and waits for them.
func (s *supervisor) launch(ctx context.Context, f store.Feed) error {
	built, err := s.build(f)
	if err != nil {
		return err
	}
	trigger := make(chan struct{}, 1)
	built.Managed = true
	built.Trigger = trigger

	run, cancel := context.WithCancel(context.WithoutCancel(ctx))
	wait, err := runFeed(run, s.store, built, s.log)
	if err != nil {
		cancel()
		release(built)
		return err
	}
	s.mu.Lock()
	s.running[f.Source] = &runner{cancel: cancel, wait: wait, trigger: trigger}
	s.mu.Unlock()
	s.ops.enable(f.Source, true)
	return nil
}

// halt stops one connector if it is running, and waits for its last sync.
//
// Waiting is the part that matters. A source that is being replaced has to have
// stopped writing before the replacement starts, or two crawlers write the same
// document ids from two different directories and the index is whichever of
// them was last.
func (s *supervisor) halt(source string) {
	s.mu.Lock()
	r, ok := s.running[source]
	delete(s.running, source)
	s.mu.Unlock()
	if !ok {
		return
	}
	r.cancel()
	r.wait()
}

// build turns a stored configuration into a feed ready to run.
//
// Everything it can refuse is refused here, which is what makes adding a
// connector from the interface answer immediately with the field that is wrong
// rather than accepting it and failing in a log line a minute later.
func (s *supervisor) build(f store.Feed) (feed, error) {
	switch f.Kind {
	case kindCorpus:
		var c corpusConfig
		if err := decodeConfig(f.Config, &c); err != nil {
			return feed{}, err
		}
		opts, err := c.options(f.Source)
		if err != nil {
			return feed{}, err
		}
		if err := opts.validate(); err != nil {
			return feed{}, fmt.Errorf("%w: %w", api.ErrBadConnector, err)
		}
		built, err := corpusFeed(opts, s.tenant, s.track, s.ops, s.log)
		if err != nil {
			return feed{}, fmt.Errorf("%w: %w", api.ErrBadConnector, err)
		}
		return built, nil
	case kindBucket:
		var c bucketConfig
		if err := decodeConfig(f.Config, &c); err != nil {
			return feed{}, err
		}
		opts, err := c.options(f.Source, s.creds)
		if err != nil {
			return feed{}, err
		}
		if err := opts.validate(); err != nil {
			return feed{}, fmt.Errorf("%w: %w", api.ErrBadConnector, err)
		}
		built, err := bucketFeed(opts, s.tenant, s.track, s.ops, s.log)
		if err != nil {
			return feed{}, fmt.Errorf("%w: %w", api.ErrBadConnector, err)
		}
		return built, nil
	default:
		return feed{}, fmt.Errorf("%w: there is no connector of kind %q, only %q and %q",
			api.ErrBadConnector, f.Kind, kindCorpus, kindBucket)
	}
}

// release gives back whatever a built feed is holding.
func release(f feed) {
	if f.Release != nil {
		f.Release()
	}
}

// decodeConfig reads a connector's settings, and says so when it cannot.
//
// An absent config is an empty object rather than an error, so that a kind
// whose fields all have defaults can be added with nothing but a name, and so
// that the message about the field that is missing comes from validation rather
// than from the JSON decoder.
func decodeConfig(raw json.RawMessage, into any) error {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("%w: the settings are not readable: %w", api.ErrBadConnector, err)
	}
	return nil
}

// corpusConfig is the directory connector's settings, as they are stored and as
// they arrive from the interface.
//
// It is not [corpusOptions]. The intervals here are strings such as "30s"
// because that is what somebody types and what reads back out of a database as
// what they typed, where a duration on the wire is a count of nanoseconds that
// nobody can check by eye. There is no name either: the source name is the
// connector's key and is not a setting that can disagree with it.
type corpusConfig struct {
	Dir       string `json:"dir"`
	ACL       string `json:"acl,omitempty"`
	Identity  string `json:"identity,omitempty"`
	Domain    string `json:"domain,omitempty"`
	Refresh   string `json:"refresh,omitempty"`
	Watch     bool   `json:"watch,omitempty"`
	Reconcile string `json:"reconcile,omitempty"`
}

func (c corpusConfig) options(source string) (corpusOptions, error) {
	refresh, err := interval(c.Refresh, "refresh")
	if err != nil {
		return corpusOptions{}, err
	}
	reconcile, err := interval(c.Reconcile, "reconcile")
	if err != nil {
		return corpusOptions{}, err
	}
	o := corpusOptions{
		Dir:       c.Dir,
		Name:      source,
		ACL:       c.ACL,
		Identity:  c.Identity,
		Domain:    c.Domain,
		Refresh:   refresh,
		Watch:     c.Watch,
		Reconcile: reconcile,
	}
	if o.Dir == "" {
		return corpusOptions{}, fmt.Errorf("%w: a directory connector needs a dir", api.ErrBadConnector)
	}
	// The same defaults the flags carry, so that the shortest thing somebody
	// can add from the interface is the shortest thing they could type.
	if o.ACL == "" {
		o.ACL = aclTenant
	}
	if o.ACL == aclOS && o.Identity == "" {
		o.Identity = "unix"
	}
	return o, nil
}

// bucketConfig is the object storage connector's settings.
//
// There is nothing here for the keys. See [supervisor.creds].
type bucketConfig struct {
	Bucket    string  `json:"bucket"`
	Endpoint  string  `json:"endpoint"`
	Region    string  `json:"region,omitempty"`
	Prefix    string  `json:"prefix,omitempty"`
	ACL       string  `json:"acl,omitempty"`
	Identity  string  `json:"identity,omitempty"`
	Domain    string  `json:"domain,omitempty"`
	PathStyle bool    `json:"path_style,omitempty"`
	Refresh   string  `json:"refresh,omitempty"`
	Reconcile string  `json:"reconcile,omitempty"`
	Rate      float64 `json:"rate,omitempty"`
	Burst     int     `json:"burst,omitempty"`
	Retries   int     `json:"retries,omitempty"`
}

func (c bucketConfig) options(source string, creds credentials) (bucketOptions, error) {
	refresh, err := interval(c.Refresh, "refresh")
	if err != nil {
		return bucketOptions{}, err
	}
	reconcile, err := interval(c.Reconcile, "reconcile")
	if err != nil {
		return bucketOptions{}, err
	}
	o := bucketOptions{
		Bucket:    c.Bucket,
		Endpoint:  c.Endpoint,
		Region:    c.Region,
		Prefix:    c.Prefix,
		Name:      source,
		ACL:       c.ACL,
		Identity:  c.Identity,
		Domain:    c.Domain,
		PathStyle: c.PathStyle,
		Refresh:   refresh,
		Reconcile: reconcile,
		Rate:      c.Rate,
		Burst:     c.Burst,
		Retries:   c.Retries,
		Access:    creds.Access,
		Secret:    creds.Secret,
		Session:   creds.Session,
	}
	if o.Bucket == "" {
		return bucketOptions{}, fmt.Errorf("%w: an object storage connector needs a bucket", api.ErrBadConnector)
	}
	if o.Region == "" {
		o.Region = "us-east-1"
	}
	if o.ACL == "" {
		o.ACL = aclTenant
	}
	return o, nil
}

// interval parses one of the durations a connector is configured with.
//
// Empty is zero, which every caller reads as once and never again, and a
// negative one is refused here rather than by the validation below so that the
// message names the field.
func interval(s, name string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%w: %s is not a duration, try 30s or 5m", api.ErrBadConnector, name)
	}
	if d < 0 {
		return 0, fmt.Errorf("%w: %s is negative", api.ErrBadConnector, name)
	}
	return d, nil
}
