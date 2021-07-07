package balancerutil

import (
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/resolver"
)
type State int

const (
	Inactive State = iota
	Active
)

type BalancerManager struct {
	balancer balancer.Balancer

	mu    sync.Mutex
	state State
	// TODO(easwars): This should be an unbounded channel. Refactor
	// scStateUpdateBuffer to work with interface{}.
	workerCh chan interface{}
	// This is closed when the worker exits.
	doneCh   chan struct{}
	subConns map[balancer.SubConn]struct{}
}

// This implements the balancer.SubConn interface
type scWrapper struct {
	// Additionally this will have a channel to notify that the addrConn is
	// available.
}

func (scw *scWrapper) UpdateAddresses(addrs []resolver.Address) {
	// This should be mostly similar to acBalancerWrapper. We might have to
	// export some methods from the ClientConn, or some refactoring to make
	// this happen. But shouldn't be too hard.a
}

func (scw *scWrapper) Connect {
	// Block on the notify channel from the worker (which is closed once we get
	// an addrConn object from the ClientConn

	// Call connect on the underlying addrConn
}


func NewBalancerManager(cc grpc.ClientConn, b balancer.Builder, bopts balancer.BuildOptions) *BalancerManager {
}

func (b *BalancerManager) NewSubConn(addrs []resolver.Address, opts balancer.NewSubConnOptions) (balancer.SubConn, error) {
	// Grab the lock
	// Check the state: if inactive, return
	// Create a new scWrapper object. This does not have an initialized addrConn at this point.
	// Push update on worker channel
	// Store the scWrapper in the map
	// Unlock
	// Return the scWrapper
}

func (b *BalancerManager) RemoveSubConn(sc balancer.SubConn) {
	// Should be mostly similar to NewSubConn, although it doesnt need anything
	// to pushed on the channel. Can be completely inline.
}

func (b *BalancerManager) UpdateBalancerState(s connectivity.State, p balancer.Picker) {
	// Similar to NewSubConn. Grabs lock, checks state and pushes update on the
	// worker channel.
}

func (b *BalancerManager) ResolveNow(o resolver.ResolveNowOption) {
	// Similar to NewSubConn. Grabs lock, checks state and pushes update on the
	// worker channel.
}

func (b *BalancerManager) Target() string {}

func (b *BalancerManager) UpdateClientConnState(ccs *balancer.ClientConnState) {
	// TODO(easwars): Add code in the caller (ClientConn) to filter out grpclb
	// addresses.

	// Grab the lock
	// Check the state: if inactive, return
	// Push update on worker channel
	// Unlock
	// Return
}

func (b *BalancerManager) UpdateSubConnState(sc balancer.SubConn, s connectivity.State) {
	// Same as UpdateClientConnState. Grabs the lock, checks the state and
	// pushes the update on the unbounded channel.
}

func (b *BalancerManager) Close() {
	// Grab the lock
	// Set the state to Inactive
	// Close the worker channel
	// Unlock

	// Wait for done channel to be closed by the worker goroutine
	// Call Close() on the balancer
	// At this point, we are sure that nobody will touch the subConns map.
	// Iterate through it and call gRPC to remove addrConn, and remove it from
	// our map.
	// Return
}

func (b *BalancerManager) watcher() {
	for {
		up, ok := <-b.workerCh
		if !ok {
			// This means that the balancer manager is closed.
			close(b.doneCh)
			return
		}
		switch up.(type) {
		case ccUpdate:
			// Call balancer.UpdateClientConnState()
		case scUpdate:
			// Call balancer.UpdateSubConnState
		case newSubConn:
			// Call newAddrConn on the clientConn. This is non blocking
			// Store the return value in the scWrapper
			// notify on the channel within the scWrapper
		case newBalancerState:
			// Call the appropriate method on the ClientConn.
		}
	}
}
