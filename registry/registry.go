package registry

import (
	"encoding/json"
	"sync"
	"time"

	steno "github.com/cloudfoundry/gosteno"
	"github.com/cloudfoundry/yagnats"

	"github.com/hjinkim-cf1/gorouter/config"
	"github.com/hjinkim-cf1/gorouter/route"
)

type RegistryInterface interface {
	Register(uri route.Uri, endpoint *route.Endpoint)
	Unregister(uri route.Uri, endpoint *route.Endpoint)
	Lookup(uri route.Uri) *route.Pool
	StartPruningCycle()
	StopPruningCycle()
	NumUris() int
	NumEndpoints() int
	MarshalJSON() ([]byte, error)
}

type RouteRegistry struct {
	sync.RWMutex

	logger *steno.Logger

	byUri map[route.Uri]*route.Pool

	pruneStaleDropletsInterval time.Duration
	dropletStaleThreshold      time.Duration

	messageBus yagnats.NATSConn

	ticker           *time.Ticker
	timeOfLastUpdate time.Time
}

func NewRouteRegistry(c *config.Config, mbus yagnats.NATSConn) *RouteRegistry {
	r := &RouteRegistry{}

	r.logger = steno.NewLogger("router.registry")

	r.byUri = make(map[route.Uri]*route.Pool)

	r.pruneStaleDropletsInterval = c.PruneStaleDropletsInterval
	r.dropletStaleThreshold = c.DropletStaleThreshold

	r.messageBus = mbus

	return r
}

func (r *RouteRegistry) Register(uri route.Uri, endpoint *route.Endpoint) {
	t := time.Now()
	r.Lock()

	uri = uri.ToLower()

	pool, found := r.byUri[uri]
	if !found {
		pool = route.NewPool(r.dropletStaleThreshold / 4)
		r.byUri[uri] = pool
	}

	pool.Put(endpoint)

	r.timeOfLastUpdate = t
	r.Unlock()
}

func (r *RouteRegistry) Unregister(uri route.Uri, endpoint *route.Endpoint) {
	r.Lock()

	uri = uri.ToLower()

	pool, found := r.byUri[uri]
	if found {
		pool.Remove(endpoint)

		if pool.IsEmpty() {
			delete(r.byUri, uri)
		}
	}

	r.Unlock()
}

func (r *RouteRegistry) Lookup(uri route.Uri) *route.Pool {
	r.RLock()

	uri = uri.ToLower()
	var err error
	pool, found := r.byUri[uri]
	for !found && err == nil {
		uri, err = uri.NextWildcard()
		pool, found = r.byUri[uri]
	}

	r.RUnlock()

	return pool
}

func (r *RouteRegistry) StartPruningCycle() {
	if r.pruneStaleDropletsInterval > 0 {
		r.Lock()
		r.ticker = time.NewTicker(r.pruneStaleDropletsInterval)
		r.Unlock()

		go func() {
			for {
				select {
				case <-r.ticker.C:
					r.logger.Debug("Start to check and prune stale droplets")
					r.pruneStaleDroplets()
				}
			}
		}()
	}
}

func (r *RouteRegistry) StopPruningCycle() {
	r.Lock()
	if r.ticker != nil {
		r.ticker.Stop()
	}
	r.Unlock()
}

func (registry *RouteRegistry) NumUris() int {
	registry.RLock()
	uriCount := len(registry.byUri)
	registry.RUnlock()

	return uriCount
}

func (r *RouteRegistry) TimeOfLastUpdate() time.Time {
	r.RLock()
	t := r.timeOfLastUpdate
	r.RUnlock()

	return t
}

func (r *RouteRegistry) NumEndpoints() int {
	r.RLock()
	uris := make(map[string]struct{})
	f := func(endpoint *route.Endpoint) {
		uris[endpoint.CanonicalAddr()] = struct{}{}
	}
	for _, pool := range r.byUri {
		pool.Each(f)
	}
	r.RUnlock()

	return len(uris)
}

func (r *RouteRegistry) MarshalJSON() ([]byte, error) {
	r.RLock()
	defer r.RUnlock()

	return json.Marshal(r.byUri)
}

func (r *RouteRegistry) pruneStaleDroplets() {
	r.Lock()
	for k, pool := range r.byUri {
		pool.PruneEndpoints(r.dropletStaleThreshold)
		if pool.IsEmpty() {
			delete(r.byUri, k)
		}
	}
	r.Unlock()
}

func (r *RouteRegistry) pauseStaleTracker() {
	r.Lock()
	t := time.Now()

	for _, pool := range r.byUri {
		pool.MarkUpdated(t)
	}

	r.Unlock()
}
