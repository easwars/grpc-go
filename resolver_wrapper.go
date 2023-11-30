/*
 *
 * Copyright 2017 gRPC authors.
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

package grpc

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/internal/channelz"
	"google.golang.org/grpc/internal/grpcsync"
	"google.golang.org/grpc/internal/pretty"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/serviceconfig"
)

// resolverStateUpdater wraps the single method used by ccResolverWrapper to
// report a state update from the actual resolver implementation.
type resolverStateUpdater interface {
	updateResolverState(s resolver.State, err error) error
}

type genWrapper struct {
	parent           *ccResolverWrapper
	serializer       *grpcsync.CallbackSerializer // To serialize all incoming calls.
	serializerCancel context.CancelFunc           // To close the serializer, accessed only from close().
}

func (w *genWrapper) close() {
	w.serializerCancel()
}

func (w *genWrapper) UpdateState(s resolver.State) error {
	w.parent.mu.Lock()
	if !w.parent.isCurrentLocked(w) {
		w.parent.mu.Unlock()
		return fmt.Errorf("state update %+v received from closed resolver", s)
	}

	errCh := make(chan error, 1)
	ok := w.serializer.Schedule(func(context.Context) {
		w.parent.mu.Unlock()
		errCh <- w.parent.UpdateState(s)
	})
	if !ok {
		// The only time when Schedule() fail to add the callback to the
		// serializer is when the serializer is closed, and this happens only
		// when the resolver wrapper is closed.
		w.parent.mu.Unlock()
		return nil
	}
	return <-errCh
}

func (w *genWrapper) ReportError(err error) {
	w.parent.mu.Lock()
	if !w.parent.isCurrentLocked(w) {
		w.parent.mu.Unlock()
		return
	}

	ok := w.serializer.Schedule(func(context.Context) {
		w.parent.mu.Unlock()
		w.parent.ReportError(err)
	})
	if !ok {
		// The only time when Schedule() fail to add the callback to the
		// serializer is when the serializer is closed, and this happens only
		// when the resolver wrapper is closed.
		w.parent.mu.Unlock()
	}
}

func (w *genWrapper) NewAddress(addrs []resolver.Address) {
	w.parent.mu.Lock()
	if !w.parent.isCurrentLocked(w) {
		w.parent.mu.Unlock()
		return
	}

	ok := w.serializer.Schedule(func(context.Context) {
		w.parent.mu.Unlock()
		w.parent.NewAddress(addrs)
	})
	if !ok {
		// The only time when Schedule() fail to add the callback to the
		// serializer is when the serializer is closed, and this happens only
		// when the resolver wrapper is closed.
		w.parent.mu.Unlock()
	}
}

func (w *genWrapper) ParseServiceConfig(scJSON string) *serviceconfig.ParseResult {
	return parseServiceConfig(scJSON)
}

// ccResolverWrapper is a wrapper on top of cc for resolvers.
// It implements resolver.ClientConn interface.
type ccResolverWrapper struct {
	// The following fields are initialized when the wrapper is created and are
	// read-only afterwards, and therefore can be accessed without a mutex.
	cc                  resolverStateUpdater
	channelzID          *channelz.Identifier
	ignoreServiceConfig bool
	opts                ccResolverWrapperOpts

	// All incoming (resolver --> gRPC) calls are guaranteed to execute in a
	// mutually exclusive manner as they are scheduled on the serializer.
	// Fields accessed *only* in these serializer callbacks, can therefore be
	// accessed without a mutex.
	curState resolver.State

	// mu guards access to the below fields.
	mu       sync.Mutex
	resolver resolver.Resolver // Accessed only from outgoing calls.
	gw       *genWrapper
}

// ccResolverWrapperOpts wraps the arguments to be passed when creating a new
// ccResolverWrapper.
type ccResolverWrapperOpts struct {
	target     resolver.Target       // User specified dial target to resolve.
	builder    resolver.Builder      // Resolver builder to use.
	bOpts      resolver.BuildOptions // Resolver build options to use.
	channelzID *channelz.Identifier  // Channelz identifier for the channel.
}

// newCCResolverWrapper uses the resolver.Builder to build a Resolver and
// returns a ccResolverWrapper object which wraps the newly built resolver.
func newCCResolverWrapper(cc resolverStateUpdater, opts ccResolverWrapperOpts) *ccResolverWrapper {
	return &ccResolverWrapper{
		cc:                  cc,
		channelzID:          opts.channelzID,
		ignoreServiceConfig: opts.bOpts.DisableServiceConfig,
		opts:                opts,
	}
}

func (ccr *ccResolverWrapper) isCurrentLocked(gw *genWrapper) bool {
	return ccr.gw == gw
}

func (ccr *ccResolverWrapper) resolveNow(o resolver.ResolveNowOptions) {
	ccr.mu.Lock()
	defer ccr.mu.Unlock()

	// ccr.resolver field is set only after the call to Build() returns. But in
	// the process of building, the resolver may send an error update which when
	// propagated to the balancer may result in a re-resolution request.
	if ccr.resolver == nil {
		return
	}
	ccr.resolver.ResolveNow(o)
}

func (ccr *ccResolverWrapper) close() {
	ccr.mu.Lock()

	gw := ccr.gw
	if gw == nil {
		ccr.mu.Unlock()
		return
	}
	ccr.gw = nil

	channelz.Info(logger, ccr.channelzID, "Closing the name resolver")

	// Close the serializer to ensure that no more calls from the resolver are
	// handled, before actually closing the resolver.
	gw.serializerCancel()
	r := ccr.resolver
	ccr.mu.Unlock()

	// Give enqueued callbacks a chance to finish.
	<-gw.serializer.Done()

	// Spawn a goroutine to close the resolver (since it may block trying to
	// cleanup all allocated resources) and return early.
	if r != nil {
		go r.Close()
	}
}

func (ccr *ccResolverWrapper) enterIdleMode() {
	ccr.close()
}

func (ccr *ccResolverWrapper) exitIdleMode() error {
	ccr.mu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	ccr.gw = &genWrapper{
		parent:           ccr,
		serializer:       grpcsync.NewCallbackSerializer(ctx),
		serializerCancel: cancel,
	}
	ccr.mu.Unlock()

	// Cannot hold the lock at build time because the resolver can send an
	// update or error inline and these incoming calls grab the lock to schedule
	// a callback in the serializer.
	r, err := ccr.opts.builder.Build(ccr.opts.target, ccr.gw, ccr.opts.bOpts)
	if err != nil {
		cancel()
		return err
	}

	// Any error reported by the resolver at build time that leads to a
	// re-resolution request from the balancer is dropped by grpc until we
	// return from this function. So, we don't have to handle pending resolveNow
	// requests here.
	ccr.mu.Lock()
	ccr.resolver = r
	ccr.mu.Unlock()
	return nil
}

// UpdateState is called by resolver implementations to report new state to gRPC
// which includes addresses and service config.
func (ccr *ccResolverWrapper) UpdateState(s resolver.State) error {
	if s.Endpoints == nil {
		s.Endpoints = make([]resolver.Endpoint, 0, len(s.Addresses))
		for _, a := range s.Addresses {
			ep := resolver.Endpoint{Addresses: []resolver.Address{a}, Attributes: a.BalancerAttributes}
			ep.Addresses[0].BalancerAttributes = nil
			s.Endpoints = append(s.Endpoints, ep)
		}
	}

	ccr.addChannelzTraceEvent(s)
	ccr.curState = s
	if err := ccr.cc.updateResolverState(ccr.curState, nil); err == balancer.ErrBadResolverState {
		return balancer.ErrBadResolverState
	}
	return nil
}

// ReportError is called by resolver implementations to report errors
// encountered during name resolution to gRPC.
func (ccr *ccResolverWrapper) ReportError(err error) {
	channelz.Warningf(logger, ccr.channelzID, "ccResolverWrapper: reporting error to cc: %v", err)
	ccr.cc.updateResolverState(resolver.State{}, err)
}

// NewAddress is called by the resolver implementation to send addresses to
// gRPC.
func (ccr *ccResolverWrapper) NewAddress(addrs []resolver.Address) {
	ccr.addChannelzTraceEvent(resolver.State{Addresses: addrs, ServiceConfig: ccr.curState.ServiceConfig})
	ccr.curState.Addresses = addrs
	ccr.cc.updateResolverState(ccr.curState, nil)
}

// ParseServiceConfig is called by resolver implementations to parse a JSON
// representation of the service config.
func (ccr *ccResolverWrapper) ParseServiceConfig(scJSON string) *serviceconfig.ParseResult {
	return parseServiceConfig(scJSON)
}

// addChannelzTraceEvent adds a channelz trace event containing the new
// state received from resolver implementations.
func (ccr *ccResolverWrapper) addChannelzTraceEvent(s resolver.State) {
	var updates []string
	var oldSC, newSC *ServiceConfig
	var oldOK, newOK bool
	if ccr.curState.ServiceConfig != nil {
		oldSC, oldOK = ccr.curState.ServiceConfig.Config.(*ServiceConfig)
	}
	if s.ServiceConfig != nil {
		newSC, newOK = s.ServiceConfig.Config.(*ServiceConfig)
	}
	if oldOK != newOK || (oldOK && newOK && oldSC.rawJSONString != newSC.rawJSONString) {
		updates = append(updates, "service config updated")
	}
	if len(ccr.curState.Addresses) > 0 && len(s.Addresses) == 0 {
		updates = append(updates, "resolver returned an empty address list")
	} else if len(ccr.curState.Addresses) == 0 && len(s.Addresses) > 0 {
		updates = append(updates, "resolver returned new addresses")
	}
	channelz.Infof(logger, ccr.channelzID, "Resolver state updated: %s (%v)", pretty.ToJSON(s), strings.Join(updates, "; "))
}
