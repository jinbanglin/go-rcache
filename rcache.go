package rcache

import (
	"sync"
	"time"

	"github.com/micro/go-log"
	"github.com/micro/go-micro/registry"
)

// Cache is the registry cache interface
type Cache interface {
	// embed the registry interface
	registry.Registry
	// stop the cache watcher
	Stop()
}

type cache struct {
	registry.Registry
	ttl time.Duration

	// registry cache
	sync.RWMutex
	cache map[string][]*registry.Service
	ttls  map[string]time.Time

	exit chan bool
}

var (
	DefaultTTL = time.Minute
)

func (c *cache) quit() bool {
	select {
	case <-c.exit:
		return true
	default:
		return false
	}
}

// cp copies a service. Because we're caching handing back pointers would
// create a race condition, so we do this instead its fast enough
func (c *cache) cp(current []*registry.Service) []*registry.Service {
	var services []*registry.Service

	for _, service := range current {
		// copy service
		s := new(registry.Service)
		*s = *service

		// copy nodes
		var nodes []*registry.Node
		for _, node := range service.Nodes {
			n := new(registry.Node)
			*n = *node
			nodes = append(nodes, n)
		}
		s.Nodes = nodes

		// copy endpoints
		var eps []*registry.Endpoint
		for _, ep := range service.Endpoints {
			e := new(registry.Endpoint)
			*e = *ep
			eps = append(eps, e)
		}
		s.Endpoints = eps

		// append service
		services = append(services, s)
	}

	return services
}

func (c *cache) del(service string) {
	delete(c.cache, service)
	delete(c.ttls, service)
}

func (c *cache) get(service string) ([]*registry.Service, error) {
	c.Lock()
	defer c.Unlock()

	// check the cache first
	services, ok := c.cache[service]
	ttl, kk := c.ttls[service]

	// got results, copy and return
	if ok && len(services) > 0 {
		// only return if its less than the ttl
		if kk && time.Since(ttl) < c.ttl {
			return c.cp(services), nil
		}
	}

	// cache miss or ttl expired

	// now ask the registry
	services, err := c.Registry.GetService(service)
	if err != nil {
		return nil, err
	}

	// we didn't have any results so cache
	c.cache[service] = c.cp(services)
	c.ttls[service] = time.Now().Add(c.ttl)
	return services, nil
}

func (c *cache) set(service string, services []*registry.Service) {
	c.cache[service] = services
	c.ttls[service] = time.Now().Add(c.ttl)
}

func (c *cache) update(res *registry.Result) {
	if res == nil || res.Service == nil {
		return
	}

	c.Lock()
	defer c.Unlock()

	services, ok := c.cache[res.Service.Name]
	if !ok {
		// we're not going to cache anything
		// unless there was already a lookup
		return
	}

	if len(res.Service.Nodes) == 0 {
		switch res.Action {
		case "delete":
			c.del(res.Service.Name)
		}
		return
	}

	// existing service found
	var service *registry.Service
	var index int
	for i, s := range services {
		if s.Version == res.Service.Version {
			service = s
			index = i
		}
	}

	switch res.Action {
	case "create", "update":
		if service == nil {
			c.set(res.Service.Name, append(services, res.Service))
			return
		}

		// append old nodes to new service
		for _, cur := range service.Nodes {
			var seen bool
			for _, node := range res.Service.Nodes {
				if cur.Id == node.Id {
					seen = true
					break
				}
			}
			if !seen {
				res.Service.Nodes = append(res.Service.Nodes, cur)
			}
		}

		services[index] = res.Service
		c.set(res.Service.Name, services)
	case "delete":
		if service == nil {
			return
		}

		var nodes []*registry.Node

		// filter cur nodes to remove the dead one
		for _, cur := range service.Nodes {
			var seen bool
			for _, del := range res.Service.Nodes {
				if del.Id == cur.Id {
					seen = true
					break
				}
			}
			if !seen {
				nodes = append(nodes, cur)
			}
		}

		// still got nodes, save and return
		if len(nodes) > 0 {
			service.Nodes = nodes
			services[index] = service
			c.set(service.Name, services)
			return
		}

		// zero nodes left

		// only have one thing to delete
		// nuke the thing
		if len(services) == 1 {
			c.del(service.Name)
			return
		}

		// still have more than 1 service
		// check the version and keep what we know
		var srvs []*registry.Service
		for _, s := range services {
			if s.Version != service.Version {
				srvs = append(srvs, s)
			}
		}

		// save
		c.set(service.Name, srvs)
	}
}

// run starts the cache watcher loop
// it creates a new watcher if there's a problem
// reloads the watcher if Init is called
// and returns when Close is called
func (c *cache) run() {
	go c.tick()

	for {
		// exit early if already dead
		if c.quit() {
			return
		}

		// create new watcher
		w, err := c.Registry.Watch()
		if err != nil {
			log.Log(err)
			time.Sleep(time.Second)
			continue
		}

		// watch for events
		if err := c.watch(w); err != nil {
			log.Log(err)
			continue
		}
	}
}

// check cache and expire on each tick
func (c *cache) tick() {
	t := time.NewTicker(time.Minute)

	for {
		select {
		case <-t.C:
			c.Lock()
			for service, expiry := range c.ttls {
				if d := time.Since(expiry); d > c.ttl {
					// TODO: maybe refresh the cache rather than blowing it away
					c.del(service)
				}
			}
			c.Unlock()
		case <-c.exit:
			return
		}
	}
}

// watch loops the next event and calls update
// it returns if there's an error
func (c *cache) watch(w registry.Watcher) error {
	defer w.Stop()

	// manage this loop
	go func() {
		// wait for exit
		<-c.exit
		w.Stop()
	}()

	for {
		res, err := w.Next()
		if err != nil {
			return err
		}
		c.update(res)
	}
}

func (c *cache) GetService(service string) ([]*registry.Service, error) {
	// get the service
	// try the cache first
	// if that fails go directly to the registry
	services, err := c.get(service)
	if err != nil {
		return nil, err
	}

	// if there's nothing left, return
	if len(services) == 0 {
		return nil, registry.ErrNotFound
	}

	// return services
	return services, nil
}

func (c *cache) ListServices() ([]*registry.Service, error) {
	var services []*registry.Service

	c.RLock()
	defer c.RUnlock()

	for k, _ := range c.cache {
		services = append(services, &registry.Service{Name: k})
	}

	return services, nil
}

func (c *cache) Stop() {
	select {
	case <-c.exit:
		return
	default:
		close(c.exit)
	}
}

func (c *cache) String() string {
	return "rcache"
}

// New returns a new cache
func New(r registry.Registry) Cache {
	c := &cache{
		Registry: r,
		ttl:      DefaultTTL,
		cache:    make(map[string][]*registry.Service),
		ttls:     make(map[string]time.Time),
		exit:     make(chan bool),
	}

	go c.run()
	return c
}
