package discovery

import (
	"errors"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/calmlax/aevons-gateway/internal/model"

	frameworkconsul "github.com/calmlax/aevons-framework/core/consul"
	"golang.org/x/sync/singleflight"
)

type cachedInstances struct {
	instances  []frameworkconsul.Instance
	expiresAt  time.Time
	staleUntil time.Time
}

type Resolver struct {
	registry     *frameworkconsul.Registry
	refreshEvery time.Duration
	staleIfError time.Duration
	rr           sync.Map
	cache        sync.Map
	sf           singleflight.Group
}

func NewResolver(registry *frameworkconsul.Registry, cfg model.DiscoveryConfig) *Resolver {
	refreshEvery := time.Duration(cfg.RefreshSeconds) * time.Second
	if refreshEvery <= 0 {
		refreshEvery = 3 * time.Second
	}

	staleIfError := time.Duration(cfg.StaleIfErrorSeconds) * time.Second
	if staleIfError <= 0 {
		staleIfError = 30 * time.Second
	}

	return &Resolver{
		registry:     registry,
		refreshEvery: refreshEvery,
		staleIfError: staleIfError,
	}
}

// Resolve uses cached discovery results on the hot path.
// Consul is refreshed only when the cache is stale; if refresh fails, a short-lived
// stale snapshot is still allowed to keep the gateway available during transient issues.
func (r *Resolver) Resolve(rule *model.ServiceRule) (*frameworkconsul.Instance, error) {
	if rule == nil {
		return nil, errors.New("service rule is nil")
	}
	if rule.Discovery != "consul" {
		return nil, errors.New("unsupported discovery type: " + rule.Discovery)
	}
	if r.registry == nil {
		return nil, errors.New("consul registry is not initialized")
	}

	serviceName := rule.Name
	now := time.Now()

	if cached, ok := r.loadCache(serviceName); ok && len(cached.instances) > 0 && now.Before(cached.expiresAt) {
		return r.selectInstance(rule, cached.instances), nil
	}

	instances, err := r.refresh(serviceName)
	if err == nil && len(instances) > 0 {
		return r.selectInstance(rule, instances), nil
	}

	if cached, ok := r.loadCache(serviceName); ok && len(cached.instances) > 0 && now.Before(cached.staleUntil) {
		return r.selectInstance(rule, cached.instances), nil
	}

	if err != nil {
		return nil, err
	}
	return nil, errors.New("no healthy instances for service " + serviceName)
}

func (r *Resolver) refresh(serviceName string) ([]frameworkconsul.Instance, error) {
	value, err, _ := r.sf.Do(serviceName, func() (any, error) {
		instances, discoverErr := r.registry.Discover(serviceName)
		if discoverErr != nil {
			return nil, discoverErr
		}
		if len(instances) == 0 {
			return nil, errors.New("no healthy instances for service " + serviceName)
		}

		snapshot := cloneInstances(instances)
		now := time.Now()
		r.cache.Store(serviceName, cachedInstances{
			instances:  snapshot,
			expiresAt:  now.Add(r.refreshEvery),
			staleUntil: now.Add(r.refreshEvery + r.staleIfError),
		})
		return snapshot, nil
	})
	if err != nil {
		return nil, err
	}
	instances, _ := value.([]frameworkconsul.Instance)
	return instances, nil
}

func (r *Resolver) loadCache(serviceName string) (cachedInstances, bool) {
	value, ok := r.cache.Load(serviceName)
	if !ok {
		return cachedInstances{}, false
	}
	cached, ok := value.(cachedInstances)
	if !ok {
		return cachedInstances{}, false
	}
	return cached, true
}

func (r *Resolver) selectInstance(rule *model.ServiceRule, instances []frameworkconsul.Instance) *frameworkconsul.Instance {
	index := 0
	switch rule.LoadBalance {
	case "random":
		index = rand.Intn(len(instances))
	default:
		counterAny, _ := r.rr.LoadOrStore(rule.Name, &atomic.Uint64{})
		counter := counterAny.(*atomic.Uint64)
		index = int(counter.Add(1)-1) % len(instances)
	}

	instance := instances[index]
	return &instance
}

func cloneInstances(src []frameworkconsul.Instance) []frameworkconsul.Instance {
	return append([]frameworkconsul.Instance(nil), src...)
}
