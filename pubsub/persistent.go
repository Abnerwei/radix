package pubsub

import (
	"sync"
	"time"

	radix "github.com/mediocregopher/radix.v2"
)

type persistent struct {
	dial func() (radix.Conn, error)

	l           sync.Mutex
	curr        Conn
	subs, psubs chanSet
	closeCh     chan struct{}
}

// NewPersistent is like New, but instead of taking in an existing radix.Conn to
// wrap it will create one on the fly. If the connection is ever terminated then
// a new one will be created using the given function. None of the methods on
// the returned Conn will ever return an error, they will instead block until a
// connection can be successfully reinstated.
func NewPersistent(dialFn func() (radix.Conn, error)) Conn {
	p := &persistent{
		dial:    dialFn,
		subs:    chanSet{},
		psubs:   chanSet{},
		closeCh: make(chan struct{}),
	}
	p.refresh()
	return p
}

func (p *persistent) refresh() {
	if p.curr != nil {
		p.curr.Close()
	}

	attempt := func() Conn {
		c, err := p.dial()
		if err != nil {
			return nil
		}
		errCh := make(chan error, 1)
		pc := newInner(c, errCh)

		for msgCh, channels := range p.subs.inverse() {
			if err := pc.Subscribe(msgCh, channels...); err != nil {
				pc.Close()
				return nil
			}
		}

		for msgCh, patterns := range p.psubs.inverse() {
			if err := pc.PSubscribe(msgCh, patterns...); err != nil {
				pc.Close()
				return nil
			}
		}

		go func() {
			select {
			case <-errCh:
				p.l.Lock()
				// It's possible that one of the methods (e.g. Subscribe)
				// already had the lock, saw the error, and called refresh. This
				// check prevents a double-refresh in that case.
				if p.curr == pc {
					p.refresh()
				}
				p.l.Unlock()
			case <-p.closeCh:
			}
		}()
		return pc
	}

	for {
		if p.curr = attempt(); p.curr != nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (p *persistent) Subscribe(msgCh chan<- Message, channels ...string) error {
	p.l.Lock()
	defer p.l.Unlock()

	// add first, so if the actual call fails then refresh will catch it
	for _, channel := range channels {
		p.subs.add(channel, msgCh)
	}

	if err := p.curr.Subscribe(msgCh, channels...); err != nil {
		p.refresh()
	}
	return nil
}

func (p *persistent) Unsubscribe(msgCh chan<- Message, channels ...string) error {
	p.l.Lock()
	defer p.l.Unlock()

	// remove first, so if the actual call fails then refresh will catch it
	for _, channel := range channels {
		p.subs.del(channel, msgCh)
	}

	if err := p.curr.Unsubscribe(msgCh, channels...); err != nil {
		p.refresh()
	}
	return nil
}

func (p *persistent) PSubscribe(msgCh chan<- Message, channels ...string) error {
	p.l.Lock()
	defer p.l.Unlock()

	// add first, so if the actual call fails then refresh will catch it
	for _, channel := range channels {
		p.psubs.add(channel, msgCh)
	}

	if err := p.curr.PSubscribe(msgCh, channels...); err != nil {
		p.refresh()
	}
	return nil
}

func (p *persistent) PUnsubscribe(msgCh chan<- Message, channels ...string) error {
	p.l.Lock()
	defer p.l.Unlock()

	// remove first, so if the actual call fails then refresh will catch it
	for _, channel := range channels {
		p.psubs.del(channel, msgCh)
	}

	if err := p.curr.PUnsubscribe(msgCh, channels...); err != nil {
		p.refresh()
	}
	return nil
}

func (p *persistent) Ping() error {
	p.l.Lock()
	defer p.l.Unlock()

	for {
		if err := p.curr.Ping(); err == nil {
			break
		}
		p.refresh()
	}
	return nil
}

func (p *persistent) Close() error {
	p.l.Lock()
	defer p.l.Unlock()
	close(p.closeCh)
	return p.curr.Close()
}
