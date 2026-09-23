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

// Dialer is the minimal dial capability a plugin needs from the engine: it
// dials a next-hop address (used when chaining). The engine provides an
// implementation that honors the configured stack (system or gvisor).
type Dialer interface {
	// DialNext dials the given address as the next hop of a chain.
	DialNext(ctx context.Context, network string, address string) (net.Conn, error)
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
