// Package cluster handles connecting to and interfacing with a redis cluster.
// It also handles connecting to new nodes in the cluster as well as failover.
//
// TODO better docs
package cluster

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	radix "github.com/mediocregopher/radix.v2"
)

// dedupe is used to deduplicate a function invocation, so if multiple
// go-routines call it at the same time only the first will actually run it, and
// the others will block until that one is done.
type dedupe struct {
	l sync.Mutex
	s *sync.Once
}

func newDedupe() *dedupe {
	return &dedupe{s: new(sync.Once)}
}

func (d *dedupe) do(fn func()) {
	d.l.Lock()
	s := d.s
	d.l.Unlock()

	s.Do(func() {
		fn()
		d.l.Lock()
		d.s = new(sync.Once)
		d.l.Unlock()
	})
}

////////////////////////////////////////////////////////////////////////////////

// Cluster contains all information about a redis cluster needed to interact
// with it, including a set of pools to each of its instances. All methods on
// Cluster are thread-safe
type Cluster struct {
	pf radix.PoolFunc

	// used to deduplicate calls to sync
	syncDedupe *dedupe

	sync.RWMutex
	pools map[string]radix.Client
	tt    Topo

	errCh   chan error // TODO expose this somehow
	closeCh chan struct{}
}

// NewCluster initializes and returns a Cluster instance. It will try every
// address given until it finds a usable one. From there it use CLUSTER SLOTS to
// discover the cluster topology and make all the necessary connections.
//
// The PoolFunc is used to make the internal pools for the instances discovered
// here and all new ones in the future. If nil is given then
// radix.DefaultPoolFunc will be used.
func NewCluster(pf radix.PoolFunc, addrs ...string) (*Cluster, error) {
	if pf == nil {
		pf = radix.DefaultPoolFunc
	}
	c := &Cluster{
		pf:         pf,
		syncDedupe: newDedupe(),
		pools:      map[string]radix.Client{},
		closeCh:    make(chan struct{}),
		errCh:      make(chan error, 1),
	}

	// make a pool to base the cluster on
	for _, addr := range addrs {
		p, err := pf("tcp", addr)
		if err != nil {
			continue
		}
		c.pools[addr] = p
		break
	}

	if err := c.Sync(); err != nil {
		for _, p := range c.pools {
			p.Close()
		}
		return nil, err
	}

	go c.syncEvery(30 * time.Second) // TODO make period configurable?

	return c, nil
}

func (c *Cluster) err(err error) {
	select {
	case c.errCh <- err:
	default:
	}
}

// may return nil, nil if no pool for the addr
func (c *Cluster) rpool(addr string) (radix.Client, error) {
	c.RLock()
	defer c.RUnlock()
	if addr == "" {
		for _, p := range c.pools {
			return p, nil
		}
		return nil, errors.New("no pools available")
	} else if p, ok := c.pools[addr]; ok {
		return p, nil
	}
	return nil, nil
}

// if addr is "" returns a random pool. If addr is given but there's no pool for
// it one will be created on-the-fly
func (c *Cluster) pool(addr string) (radix.Client, error) {
	p, err := c.rpool(addr)
	if p != nil || err != nil {
		return p, err
	}

	// if the pool isn't available make it on-the-fly. This behavior isn't
	// _great_, but theoretically the syncEvery process should clean up any
	// extraneous pools which aren't really needed

	// it's important that the cluster pool set isn't locked while this is
	// happening, because this could block for a while
	if p, err = c.pf("tcp", addr); err != nil {
		return nil, err
	}

	// we've made a new pool, but we need to double-check someone else didn't
	// make one at the same time and add it in first. If they did, close this
	// one and return that one
	c.Lock()
	if p2, ok := c.pools[addr]; ok {
		c.Unlock()
		p.Close()
		return p2, nil
	}
	c.pools[addr] = p
	c.Unlock()
	return p, nil
}

// Topo will pick a randdom node in the cluster, call CLUSTER SLOTS on it, and
// unmarshal the result into a Topo instance, returning that instance
func (c *Cluster) Topo() (Topo, error) {
	p, err := c.pool("")
	if err != nil {
		return Topo{}, err
	}
	return c.topo(p)
}

func (c *Cluster) topo(p radix.Client) (Topo, error) {
	var tt Topo
	err := p.Do(radix.Cmd(&tt, "CLUSTER", "SLOTS"))
	return tt, err
}

// Sync will synchronize the Cluster with the actual cluster, making new pools
// to new instances and removing ones from instances no longer in the cluster.
// This will be called periodically automatically, but you can manually call it
// at any time as well
func (c *Cluster) Sync() error {
	p, err := c.pool("")
	if err != nil {
		return err
	}
	c.syncDedupe.do(func() {
		err = c.sync(p)
	})
	return err
}

// while this method is normally deduplicated by the Sync method's use of
// dedupe it is perfectly thread-safe on its own and can be used whenever.
func (c *Cluster) sync(p radix.Client) error {
	tt, err := c.topo(p)
	if err != nil {
		return err
	}

	for _, t := range tt {
		// call pool just to ensure one exists for this addr
		if _, err := c.pool(t.Addr); err != nil {
			return fmt.Errorf("error connecting to %s: %s", t.Addr, err)
		}
	}

	// this is a big bit of code to totally lockdown the cluster for, but at the
	// same time Close _shouldn't_ block significantly
	c.Lock()
	defer c.Unlock()
	c.tt = tt

	tm := tt.Map()
	for addr, p := range c.pools {
		if _, ok := tm[addr]; !ok {
			p.Close()
			delete(c.pools, addr)
		}
	}

	return nil
}

func (c *Cluster) syncEvery(d time.Duration) {
	go func() {
		t := time.NewTicker(d)
		defer t.Stop()

		for {
			select {
			case <-t.C:
				if err := c.Sync(); err != nil {
					c.err(err)
				}
			case <-c.closeCh:
				return
			}
		}
	}()
}

func (c *Cluster) addrForKey(key []byte) string {
	if key == nil {
		return ""
	}
	s := Slot(key)
	c.RLock()
	defer c.RUnlock()
	for _, t := range c.tt {
		for _, slot := range t.Slots {
			if s >= slot[0] && s < slot[1] {
				return t.Addr
			}
		}
	}
	return ""
}

const doAttempts = 5

// Do performs an Action on a redis instance in the cluster, with the instance
// being determeined by the key returned from the Action's Key() method.
//
// If the Action is a CmdAction then Cluster will handled MOVED and ASK errors
// correctly, for other Action types those errors will be returned as is.
func (c *Cluster) Do(a radix.Action) error {
	return c.doInner(a, c.addrForKey(a.Key()), false, doAttempts)
}

func (c *Cluster) doInner(a radix.Action, addr string, ask bool, attempts int) error {
	p, err := c.pool(addr)
	if err != nil {
		return err
	}

	err = p.Do(radix.WithConn(a.Key(), func(conn radix.Conn) error {
		if ask {
			if err := radix.CmdNoKey(nil, "ASKING").Run(conn); err != nil {
				return err
			}
		}
		return a.Run(conn)
	}))

	if err == nil {
		return nil
	} else if _, ok := a.(radix.CmdAction); !ok {
		return err
	}

	msg := err.Error()
	moved := strings.HasPrefix(msg, "MOVED ")
	ask = strings.HasPrefix(msg, "ASK ")
	if !moved && !ask {
		return err
	}

	// if we get an ASK there's no need to do a sync quite yet, we can continue
	// normally. But MOVED always prompts a sync. In the following section we
	// figure out what address to use based on the returned error so the sync
	// isn't used _immediately_, but it still needs to happen.
	if moved {
		if err := c.Sync(); err != nil {
			return err
		}
	}

	msgParts := strings.Split(msg, " ")
	if len(msgParts) < 3 {
		return fmt.Errorf("malformed MOVED/ASK error %q", msg)
	}
	addr = msgParts[2]

	if attempts--; attempts <= 0 {
		return errors.New("cluster action redirected too many times")
	}

	return c.doInner(a, addr, ask, attempts)
}

// Close cleans up all goroutines spawned by Cluster and closes all of its
// Pools.
func (c *Cluster) Close() {
	close(c.closeCh)
	close(c.errCh)
	c.Lock()
	defer c.Unlock()

	for _, p := range c.pools {
		p.Close()
	}
	return
}
