package libkbfs

import (
	"sync"

	"golang.org/x/net/context"
)

// repeatedWaitGroup can be used in place of a sync.WaitGroup when
// code may need to repeatedly wait for a set of tasks to finish.
// (sync.WaitGroup requires special mutex usage to make this work
// properly, which can easily lead to deadlocks.)  We use a mutex,
// int, and channel to track and synchronize on the number of
// outstanding tasks.
type repeatedWaitGroup struct {
	lock     sync.Mutex
	num      int
	isIdleCh chan struct{} // leave as nil when initializing
}

// Add indicates that a number of tasks have begun.
func (rwg *repeatedWaitGroup) Add(delta int) {
	rwg.lock.Lock()
	defer rwg.lock.Unlock()
	if rwg.isIdleCh == nil {
		rwg.isIdleCh = make(chan struct{})
	}
	if rwg.num+delta < 0 {
		panic("repeatedWaitGroup count would be negative")
	}
	rwg.num += delta
	if rwg.num == 0 {
		close(rwg.isIdleCh)
		rwg.isIdleCh = nil
	}
}

// Wait blocks until either the underlying task count goes to 0, or
// the gien context is canceled.
func (rwg *repeatedWaitGroup) Wait(ctx context.Context) error {
	isIdleCh := func() chan struct{} {
		rwg.lock.Lock()
		defer rwg.lock.Unlock()
		return rwg.isIdleCh
	}()

	if isIdleCh == nil {
		return nil
	}

	select {
	case <-isIdleCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Done indicates that one task has completed.
func (rwg *repeatedWaitGroup) Done() {
	rwg.Add(-1)
}
