// Package plugin is the neutral plugin contract module that both goose and
// external plugin repositories import. It defines the canonical Outbound,
// Provider, and registry APIs that the goose engine and its plugins share.
//
// This module is dependency-free and must NEVER import goose (neither
// github.com/goose-network/goose nor any of its subpackages). Keeping it
// free of the goose import is what lets external plugin repos depend on a
// small, stable contract without pulling in the entire engine, and lets
// goose itself depend on this module without forming a cycle. The types and
// registry functions here are the canonical definitions; goose's internal
// packages and plugins alike program against these.
//
// An external repo implements Outbound and calls Register in an init()
// function, then adds itself to the engine build via a blank import:
//
//	import _ "github.com/example/goose-plugin-shadowsocks"
//
// Built-in protocols (direct, http, socks5) live under
// github.com/goose-network/goose/plugins and register the same way.
//
// # Providers
//
// In addition to static outbounds configured in the config store, a plugin
// can act as a Provider: a dynamic source of outbound configs that the engine
// polls and merges into a managed pool. This is how a plugin that discovers
// its own proxy servers (e.g. Psiphon, which fetches and refreshes a remote
// server list) feeds those servers into the engine's pool. A Provider is
// registered with RegisterProvider and instantiated from a ProviderSpec in
// the config; the engine runs a goroutine that calls Outbounds, diffs the
// result against the config store, and rebuilds the pool on change.
//
// A zero-config provider plugin may additionally declare itself the default
// for fresh deployments with RegisterDefaultProvider; see DefaultProvider.
package plugin

import (
	"context"
	"net"
	"sync"
	"time"
)

// Network names a transport network for a target address.
type Network string

const (
	NetworkTCP Network = "tcp"
	NetworkUDP Network = "udp"
)

// Location is the geographic/network location of an outbound, resolved from
// its dial address by the geo package against an offline IP database.
type Location struct {
	Country  string
	Province string
	City     string
	ISP      string
	// Raw is the original lookup record, kept for debugging/serialization.
	Raw string
}

// LatencySample is one historical latency observation of an outbound.
type LatencySample struct {
	At      time.Time
	Latency time.Duration
	OK      bool
}

// OutboundStats is the live, in-memory performance view of an outbound,
// maintained by the metrics/health subsystem and read by selectors/filters.
type OutboundStats struct {
	// RecentLatency is an exponentially-smoothed estimate of dial latency.
	RecentLatency time.Duration
	// SuccessRate is the fraction of recent dials that succeeded (0..1).
	SuccessRate float64
	// LastSeen is when this outbound was last used successfully.
	LastSeen time.Time
	// Samples is the raw recent window, for selectors that need it.
	Samples []LatencySample
}

// Outbound is a single upstream proxy the engine can dial through. It is the
// contract implemented by every plugin (built-in or external).
type Outbound interface {
	// ID is the stable, unique identifier of this outbound within the pool.
	ID() string
	// Protocol is the plugin name, e.g. "direct", "http", "socks5".
	Protocol() string
	// DialContext dials the target through this outbound. The returned
	// connection is the first hop of the chain; the router is responsible
	// for further chaining if configured.
	DialContext(ctx context.Context, network Network, target string) (net.Conn, error)
	// Address is the outbound's own dial endpoint (host:port), or empty for
	// "direct". Used for geo-location and metrics.
	Address() string
	// Location returns the resolved location, or nil if unknown.
	Location() *Location
	// Stats returns a snapshot of recent performance.
	Stats() OutboundStats
}

// OutboundFactory builds an Outbound from a plugin-specific config map. The
// engine calls factories registered with the plugin registry.
type OutboundFactory func(cfg map[string]any) (Outbound, error)

// OutboundConfig is the config-form description of a single outbound as
// produced by a Provider. It is the same shape as config.OutboundSpec but
// lives in the public package so providers do not depend on internal/config.
// The engine converts these to OutboundSpecs when merging into the store.
type OutboundConfig struct {
	// ID is the stable, unique outbound ID within the pool. Providers should
	// namespace their IDs (e.g. "psiphon-<tag>") to avoid colliding with
	// user-configured static outbounds.
	ID string
	// Protocol is the outbound plugin name to build, e.g. "socks5".
	Protocol string
	// Config is the plugin-specific config passed to the protocol's Factory.
	Config map[string]any
}

// Provider is a dynamic source of outbound configs. The engine instantiates a
// Provider from a ProviderSpec, then periodically (and on Watch signals)
// calls Outbounds to discover the current set of proxy servers and merges
// them into a managed pool. A Provider that refreshes its server list in the
// background (e.g. Psiphon's remote server list fetcher) signals Watch after
// each refresh so the engine re-polls promptly.
type Provider interface {
	// Name is the provider plugin name, e.g. "psiphon".
	Name() string
	// Outbounds returns the current set of outbound configs the provider
	// wants in its pool. The engine diffs this against the previously-merged
	// set and updates the config store accordingly. Returning an empty slice
	// is valid and empties the pool (e.g. while the provider is still
	// bootstrapping).
	Outbounds(ctx context.Context) ([]OutboundConfig, error)
	// Watch returns a channel the engine selects on to be notified that the
	// outbound set may have changed, prompting an immediate re-poll. It need
	// not be buffered; the engine coalesces. Return nil if the provider has
	// no change signal (the engine falls back to periodic polling).
	Watch() <-chan struct{}
}

// ProviderFactory builds a Provider from a plugin-specific config map.
type ProviderFactory func(cfg map[string]any) (Provider, error)

// ProviderStatus is a plugin's live, self-reported observability view: what
// it is doing right now, beyond the outbound set Outbounds returns. The
// engine merges it into its per-provider status endpoint (and the web UI)
// to answer "is this provider healthy, and what is it up to?" without an
// operator having to read engine logs.
//
// Details is a flat map of scalar values for the UI: strings, numbers,
// booleans, and string/number slices render directly. Keep it small —
// it is polled, not streamed.
type ProviderStatus struct {
	// State is a short, human-readable phase word, e.g. "running",
	// "establishing", "error". Plugins pick their own vocabulary; the
	// engine surfaces it verbatim.
	State string `json:"state"`
	// Message is one line of context for the state, e.g. the last error or
	// a progress note. Empty means "nothing to say".
	Message string `json:"message"`
	// Details carries plugin-specific live facts (counts, addresses,
	// timestamps). Keys are the plugin's own; the UI renders them as-is.
	Details map[string]any `json:"details,omitempty"`
}

// StatusReporter is the optional interface a Provider implements to expose
// its live status. The engine polls it alongside Outbounds for its
// observability endpoint; a provider that does not implement it is still
// fully functional — the engine reports only what it can observe itself
// (poll times, errors, outbound counts).
type StatusReporter interface {
	// Status returns the provider's current self-assessment. It must be
	// cheap, non-blocking, and safe to call concurrently; a panic inside is
	// the plugin's bug, but a slow Status stalls the status endpoint, so
	// keep it to an in-memory snapshot.
	Status() ProviderStatus
}

// DefaultProvider is a provider plugin's declaration of the zero-config
// default the engine should seed on a fresh deployment — a config store with
// no providers at all, such as the docker image started without a config
// file. A zero-config plugin (one that runs on its embedded defaults with an
// empty config map) calls RegisterDefaultProvider from its init() to make
// itself that default; the engine asks LookupDefaultProvider at startup and,
// when nothing is configured, seeds a provider spec from the declaration so
// a fresh deployment is routable — and shows a provider in its admin UI —
// out of the box.
//
// The engine owns the seeding policy, not the plugin: seeding is skipped
// when any provider is already configured (an explicit provider list opts
// out) and can be disabled by the operator. A plugin that is NOT
// zero-config should not declare a default — a declaration whose config
// cannot stand alone would seed a broken provider.
type DefaultProvider struct {
	// ID is the seeded provider spec's id. Empty means the Provider name.
	ID string
	// Provider is the registered provider name (RegisterProvider). Must be
	// non-empty.
	Provider string
	// Config is the plugin config for the seeded spec. Nil means the
	// zero-config form; the plugin's own defaults apply.
	Config map[string]any
}

// defaultProvider holds the engine-wide default-provider declaration.
var defaultProvider *DefaultProvider

// RegisterDefaultProvider declares this plugin as the fresh-deployment
// default provider (see DefaultProvider). Intended for init(). Panics when
// another plugin already declared a default — the default slot is singular
// and a binary linking two default-declaring plugins should fail loudly at
// startup rather than pick an init-order winner.
func RegisterDefaultProvider(dp DefaultProvider) {
	mu.Lock()
	defer mu.Unlock()
	if dp.Provider == "" {
		panic("plugin: RegisterDefaultProvider with empty Provider")
	}
	if defaultProvider != nil {
		panic("plugin: RegisterDefaultProvider called twice (" + defaultProvider.Provider + " and " + dp.Provider + ")")
	}
	if dp.ID == "" {
		dp.ID = dp.Provider
	}
	defaultProvider = &dp
}

// LookupDefaultProvider returns the declared fresh-deployment default
// provider, or false when no plugin declared one.
func LookupDefaultProvider() (DefaultProvider, bool) {
	mu.RLock()
	defer mu.RUnlock()
	if defaultProvider == nil {
		return DefaultProvider{}, false
	}
	return *defaultProvider, true
}

// UpstreamDialer dials a fetch target through the upstream proxy configured
// on a provider spec (the spec's upstream_proxy: a specific outbound, a
// pool, or an access-pool fallback — the engine owns the reference's
// semantics). The engine resolves the reference against live config and
// injects the dialer into the provider's config map under the reserved
// UpstreamDialerKey before instantiating the plugin; URL-fetching providers
// type-assert it out with AsUpstreamDialer and wire it as their HTTP
// transport's dialer so their downloads honor the route. The key is absent
// (or the assertion fails) when no upstream proxy is configured — fetch
// directly in that case.
//
// The returned connection is a tunnel to address: the upstream's proxy
// handshake has already been performed, and the target protocol (TLS, HTTP,
// ...) runs over it directly.
type UpstreamDialer interface {
	// DialUpstream dials address (host:port) through the provider's
	// configured upstream proxy. network is "tcp" or "udp".
	DialUpstream(ctx context.Context, network, address string) (net.Conn, error)
}

// UpstreamDialerKey is the reserved provider config-map key the engine uses
// to inject an UpstreamDialer into a provider before instantiation. Plugin
// configs must not use the "_upstream_dialer" name for their own fields.
const UpstreamDialerKey = "_upstream_dialer"

// AsUpstreamDialer extracts the engine-injected upstream dialer from a
// provider config map, or nil when no upstream proxy is configured.
func AsUpstreamDialer(cfg map[string]any) UpstreamDialer {
	if cfg == nil {
		return nil
	}
	d, _ := cfg[UpstreamDialerKey].(UpstreamDialer)
	return d
}

// Dialer is the minimal dial capability a plugin needs from the engine: it
// dials a next-hop address (used when chaining). The engine provides an
// implementation that honors the configured stack (system or gvisor).
type Dialer interface {
	// DialNext dials the given address as the next hop of a chain.
	DialNext(ctx context.Context, network string, address string) (net.Conn, error)
}

// StackDialer is the engine's layer-3/4 dial capability: a dial through the
// engine's configured network stack — the host kernel's system stack, or the
// userspace gvisor netstack. A plugin that embeds a proxy client and dials
// its own servers should route those connections through the engine stack
// instead of opening bare sockets, so the engine governs all transport.
//
// The engine injects its stack dialer into a provider's config map under
// the reserved StackDialerKey ("_dialer") before instantiation — the same
// key and value the router injects into outbound configs. Providers
// type-assert it out with AsStackDialer and wire it into their client as
// its underlying dialer. The key is absent when no stack dialer is
// available; dial directly in that case.
//
// The address may be a domain; the dialer resolves it.
type StackDialer interface {
	// Dial connects to address (host:port) through the engine stack.
	// network is "tcp" or "udp".
	Dial(ctx context.Context, network, address string) (net.Conn, error)
}

// StackDialerKey is the reserved provider/outbound config-map key carrying
// the engine's stack dialer. Plugin configs must not use the "_dialer" name
// for their own fields.
const StackDialerKey = "_dialer"

// AsStackDialer extracts the engine-injected stack dialer from a provider
// config map, or nil when none was injected.
func AsStackDialer(cfg map[string]any) StackDialer {
	if cfg == nil {
		return nil
	}
	d, _ := cfg[StackDialerKey].(StackDialer)
	return d
}

// registry holds all registered outbound factories by protocol name.
var (
	mu        sync.RWMutex
	factories = map[string]OutboundFactory{}
)

// Register associates a protocol name with its factory. It is intended to be
// called from init(). Register panics on duplicate registration to surface
// plugin conflicts at startup.
func Register(name string, f OutboundFactory) {
	mu.Lock()
	defer mu.Unlock()
	if f == nil {
		panic("plugin: Register(" + name + ") with nil factory")
	}
	if _, dup := factories[name]; dup {
		panic("plugin: Register called twice for " + name)
	}
	factories[name] = f
}

// Lookup returns the factory for a protocol, or false if unregistered.
func Lookup(name string) (OutboundFactory, bool) {
	mu.RLock()
	defer mu.RUnlock()
	f, ok := factories[name]
	return f, ok
}

// Names returns all registered protocol names.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(factories))
	for n := range factories {
		out = append(out, n)
	}
	return out
}

// --- Provider registry ---

var (
	providerFactories = map[string]ProviderFactory{}
)

// RegisterProvider associates a provider name with its factory. Intended to be
// called from init(). Panics on duplicate registration.
func RegisterProvider(name string, f ProviderFactory) {
	mu.Lock()
	defer mu.Unlock()
	if f == nil {
		panic("plugin: RegisterProvider(" + name + ") with nil factory")
	}
	if _, dup := providerFactories[name]; dup {
		panic("plugin: RegisterProvider called twice for " + name)
	}
	providerFactories[name] = f
}

// LookupProvider returns the provider factory for a name, or false if
// unregistered.
func LookupProvider(name string) (ProviderFactory, bool) {
	mu.RLock()
	defer mu.RUnlock()
	f, ok := providerFactories[name]
	return f, ok
}

// ProviderNames returns all registered provider names.
func ProviderNames() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(providerFactories))
	for n := range providerFactories {
		out = append(out, n)
	}
	return out
}
