/*
 *
 * Copyright 2020 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package rls

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/balancer/rls/internal/cache"
	"google.golang.org/grpc/balancer/rls/internal/keys"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/resolver"
)

var _ balancer.Balancer = (*rlsBalancer)(nil)
var _ balancer.V2Balancer = (*rlsBalancer)(nil)

type adaptiveThrottler interface {
	ShouldThrottle() bool
	RegisterBackendResponse(throttled bool)
}

type rlsBalancer struct {
	ctx        context.Context
	cancel     context.CancelFunc
	cc         balancer.ClientConn
	opts       balancer.BuildOptions
	expiryFreq time.Duration
	throttler  adaptiveThrottler

	lbCfg *lbConfig

	rlsC *rlsClient

	cacheMu      sync.Mutex
	dataCache    cache.LRU
	pendingCache map[cache.Key]cache.Entry

	ccUpdateCh chan *balancer.ClientConnState
}

func (lb *rlsBalancer) run() {
	for {
		select {
		case u := <-lb.ccUpdateCh:
			lb.handleClientConnUpdate(u)
		case <-lb.ctx.Done():
			return
		}
	}
}

func (lb *rlsBalancer) handleClientConnUpdate(ccs *balancer.ClientConnState) {
	grpclog.Infof("rls: received service config: %+v", ccs.BalancerConfig)
	newCfg, _ := ccs.BalancerConfig.(*lbConfig)
	if newCfg == nil {
		// This should ideally never happen because the service config parsing would
		// have failed for invalid configs.
		return
	}

	oldCfg := lb.lbCfg
	if oldCfg == nil {
		// This is the first time we are receiving a service config.
		oldCfg = &lbConfig{}
	}

	if cmp.Equal(newCfg, oldCfg) {
		// The service config remains unchanged. Do nothing.
		return
	}

	if newCfg.cacheSizeBytes != oldCfg.cacheSizeBytes {
		lb.cacheMu.Lock()
		lb.dataCache.Resize(newCfg.cacheSizeBytes)
		lb.cacheMu.Unlock()
	}

	lb.updateControlChannel(newCfg.lookupService, newCfg.lookupServiceTimeout)

	if !keys.BuilderMapEqual(newCfg.kbMap, oldCfg.kbMap) || newCfg.rpStrategy != oldCfg.rpStrategy {
		// TODO(easwars): Need to create a new picker object and send update to gRPC.
	}

	// TODO(easwars): Need to handle updates to defaultTarget by creating a new
	// childPolicyWrapper for it.

	// TODO(easwars): Need to handle updates to childPolicyName and/or configs.
	// The balancer will maintain a map[target]childPolicyWrapper and cache
	// entries will contain a reference to these childPolicyWrapper objects. We
	// need to store the actual childPolicy inside the wrapper such that we don't
	// have to update all the cache entries to update the childPolicy. Just
	// updating the wrapper objects should make the updated childPolicy visible to
	// the cache entries.
}

func (lb *rlsBalancer) UpdateClientConnState(ccs balancer.ClientConnState) error {
	select {
	case lb.ccUpdateCh <- &ccs:
	case <-lb.ctx.Done():
	}
	return nil
}

func (lb *rlsBalancer) ResolverError(error) {
	// ResolverError is called by gRPC when the name resolver reports an error.
	// TODO(easwars): How do we handle this?
}

func (lb *rlsBalancer) UpdateSubConnState(_ balancer.SubConn, _ balancer.SubConnState) {
	grpclog.Error("rlsbalancer.UpdateSubConnState is not implemented")
}

func (lb *rlsBalancer) Close() {
	lb.cancel()
}

func (lb *rlsBalancer) HandleSubConnStateChange(_ balancer.SubConn, _ connectivity.State) {
	grpclog.Errorf("UpdateSubConnState should be called instead of HandleSubConnStateChange")
}

func (ls *rlsBalancer) HandleResolvedAddrs(_ []resolver.Address, _ error) {
	grpclog.Errorf("UpdateClientConnState should be called instead of HandleResolvedAddrs")
}

func (lb *rlsBalancer) makeRLSRequest(path string, km keys.KeyMap) {
}

func dialRLS(server string, opts balancer.BuildOptions) (*grpc.ClientConn, error) {
	var dopts []grpc.DialOption
	if dialer := opts.Dialer; dialer != nil {
		dopts = append(dopts, grpc.WithContextDialer(dialer))
	}
	dopts = append(dopts, dialCreds(server, opts))

	cc, err := grpc.Dial(server, dopts...)
	if err != nil {
		// An error from a non-blocking dial indicates something serious.
		return nil, fmt.Errorf("rls: grpc.Dial(%s): %v", server, err)
	}
	return cc, nil
}

// TODO(easwars): Make sure the RLS channel uses the same authority as the
// parent channel during authorization.
func dialCreds(server string, opts balancer.BuildOptions) grpc.DialOption {
	switch {
	case opts.DialCreds != nil:
		if err := opts.DialCreds.OverrideServerName(server); err != nil {
			grpclog.Warningf("rls: failed to override server name in credentials: %v, using Insecure", err)
			return grpc.WithInsecure()
		}
		return grpc.WithTransportCredentials(opts.DialCreds)
	case opts.CredsBundle != nil:
		return grpc.WithTransportCredentials(opts.CredsBundle.TransportCredentials())
	default:
		grpclog.Warning("rls: no credentials available, using Insecure")
		return grpc.WithInsecure()
	}
}

// updateControlChannel updates the RLS client if required.
func (lb *rlsBalancer) updateControlChannel(service string, timeout time.Duration) {
	if service != lb.lbCfg.lookupService || timeout != lb.lbCfg.lookupServiceTimeout {
		cc, err := dialRLS(service, lb.opts)
		if err != nil {
			grpclog.Errorf("rls: dialRLS(%s, %v): %v", service, lb.opts, err)
			// A non-blocking dial has failed. We should continue to use the old
			// control channel if one exists, and return so that the rest of the
			// config updates can be processes.
			return
		}
		lb.rlsC = newRLSClient(cc, lb.opts.Target.Endpoint, timeout)
	}
}
