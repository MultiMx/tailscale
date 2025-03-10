// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package eventbus

import (
	"context"
	"reflect"
	"slices"
	"sync"

	"tailscale.com/util/set"
)

// Bus is an event bus that distributes published events to interested
// subscribers.
type Bus struct {
	write    chan any
	stop     goroutineShutdownControl
	snapshot chan chan []any

	topicsMu sync.Mutex // guards everything below.
	topics   map[reflect.Type][]*subscribeState

	// Used for introspection/debugging only, not in the normal event
	// publishing path.
	clients set.Set[*Client]
}

// New returns a new bus. Use [PublisherOf] to make event publishers,
// and [Bus.Queue] and [Subscribe] to make event subscribers.
func New() *Bus {
	stopCtl, stopWorker := newGoroutineShutdown()
	ret := &Bus{
		write:    make(chan any),
		stop:     stopCtl,
		snapshot: make(chan chan []any),
		topics:   map[reflect.Type][]*subscribeState{},
		clients:  set.Set[*Client]{},
	}
	go ret.pump(stopWorker)
	return ret
}

// Client returns a new client with no subscriptions. Use [Subscribe]
// to receive events, and [Publish] to emit events.
//
// The client's name is used only for debugging, to tell humans what
// piece of code a publisher/subscriber belongs to. Aim for something
// short but unique, for example "kernel-route-monitor" or "taildrop",
// not "watcher".
func (b *Bus) Client(name string) *Client {
	ret := &Client{
		name: name,
		bus:  b,
		pub:  set.Set[publisher]{},
	}
	b.topicsMu.Lock()
	defer b.topicsMu.Unlock()
	b.clients.Add(ret)
	return ret
}

// Close closes the bus. Implicitly closes all clients, publishers and
// subscribers attached to the bus.
//
// Close blocks until the bus is fully shut down. The bus is
// permanently unusable after closing.
func (b *Bus) Close() {
	b.stop.StopAndWait()

	var clients set.Set[*Client]
	b.topicsMu.Lock()
	clients, b.clients = b.clients, set.Set[*Client]{}
	b.topicsMu.Unlock()

	for c := range clients {
		c.Close()
	}
}

func (b *Bus) pump(stop goroutineShutdownWorker) {
	defer stop.Done()
	var vals queue
	acceptCh := func() chan any {
		if vals.Full() {
			return nil
		}
		return b.write
	}
	for {
		// Drain all pending events. Note that while we're draining
		// events into subscriber queues, we continue to
		// opportunistically accept more incoming events, if we have
		// queue space for it.
		for !vals.Empty() {
			val := vals.Peek()
			dests := b.dest(reflect.ValueOf(val).Type())
			for _, d := range dests {
			deliverOne:
				for {
					select {
					case d.write <- val:
						break deliverOne
					case <-d.stop.WaitChan():
						// Queue closed, don't block but continue
						// delivering to others.
						break deliverOne
					case in := <-acceptCh():
						vals.Add(in)
					case <-stop.Stop():
						return
					case ch := <-b.snapshot:
						ch <- vals.Snapshot()
					}
				}
			}
			vals.Drop()
		}

		// Inbound queue empty, wait for at least 1 work item before
		// resuming.
		for vals.Empty() {
			select {
			case <-stop.Stop():
				return
			case val := <-b.write:
				vals.Add(val)
			case ch := <-b.snapshot:
				ch <- nil
			}
		}
	}
}

func (b *Bus) dest(t reflect.Type) []*subscribeState {
	b.topicsMu.Lock()
	defer b.topicsMu.Unlock()
	return b.topics[t]
}

func (b *Bus) shouldPublish(t reflect.Type) bool {
	b.topicsMu.Lock()
	defer b.topicsMu.Unlock()
	return len(b.topics[t]) > 0
}

func (b *Bus) subscribe(t reflect.Type, q *subscribeState) (cancel func()) {
	b.topicsMu.Lock()
	defer b.topicsMu.Unlock()
	b.topics[t] = append(b.topics[t], q)
	return func() {
		b.unsubscribe(t, q)
	}
}

func (b *Bus) unsubscribe(t reflect.Type, q *subscribeState) {
	b.topicsMu.Lock()
	defer b.topicsMu.Unlock()
	// Topic slices are accessed by pump without holding a lock, so we
	// have to replace the entire slice when unsubscribing.
	// Unsubscribing should be infrequent enough that this won't
	// matter.
	i := slices.Index(b.topics[t], q)
	if i < 0 {
		return
	}
	b.topics[t] = slices.Delete(slices.Clone(b.topics[t]), i, i+1)
}

func newGoroutineShutdown() (goroutineShutdownControl, goroutineShutdownWorker) {
	ctx, cancel := context.WithCancel(context.Background())

	ctl := goroutineShutdownControl{
		startShutdown:    cancel,
		shutdownFinished: make(chan struct{}),
	}
	work := goroutineShutdownWorker{
		startShutdown:    ctx.Done(),
		shutdownFinished: ctl.shutdownFinished,
	}

	return ctl, work
}

// goroutineShutdownControl is a helper type to manage the shutdown of
// a worker goroutine. The worker goroutine should use the
// goroutineShutdownWorker related to this controller.
type goroutineShutdownControl struct {
	startShutdown    context.CancelFunc
	shutdownFinished chan struct{}
}

func (ctl *goroutineShutdownControl) Stop() {
	ctl.startShutdown()
}

func (ctl *goroutineShutdownControl) Wait() {
	<-ctl.shutdownFinished
}

func (ctl *goroutineShutdownControl) WaitChan() <-chan struct{} {
	return ctl.shutdownFinished
}

func (ctl *goroutineShutdownControl) StopAndWait() {
	ctl.Stop()
	ctl.Wait()
}

// goroutineShutdownWorker is a helper type for a worker goroutine to
// be notified that it should shut down, and to report that shutdown
// has completed. The notification is triggered by the related
// goroutineShutdownControl.
type goroutineShutdownWorker struct {
	startShutdown    <-chan struct{}
	shutdownFinished chan struct{}
}

func (work *goroutineShutdownWorker) Stop() <-chan struct{} {
	return work.startShutdown
}

func (work *goroutineShutdownWorker) Done() {
	close(work.shutdownFinished)
}
