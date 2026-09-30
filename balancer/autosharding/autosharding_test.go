/*
 *
 * Copyright 2026 gRPC authors.
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

package autosharding_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/balancer/autosharding"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/experimental/balancer/hostname"
	"google.golang.org/grpc/internal/grpctest"
	"google.golang.org/grpc/internal/testutils"
	"google.golang.org/grpc/resolver"
)

const (
	defaultTestTimeout      = 10 * time.Second
	defaultTestShortTimeout = 10 * time.Millisecond
)

type s struct {
	grpctest.Tester
}

func Test(t *testing.T) {
	grpctest.RunSubTests(t, s{})
}

// newTestEndpoint returns a resolver.Endpoint with the given address and
// hostname attribute (if non-empty).
func newTestEndpoint(addr, host string) resolver.Endpoint {
	ep := resolver.Endpoint{Addresses: []resolver.Address{{Addr: addr}}}
	if host != "" {
		ep = hostname.Set(ep, host)
	}
	return ep
}

type testClientConn struct {
	grpc.ClientConnInterface
	key string
}

func defaultClientConnProvider(key string) (grpc.ClientConnInterface, func(), error) {
	return &testClientConn{key: key}, func() {}, nil
}

// resolverStateWithChannelFactoryAndEndpoints returns a resolver.State with the
// given endpoints and the defaultClientConnProvider set as the
// ClientConnProvider.
func resolverStateWithChannelFactoryAndEndpoints(endpoints []resolver.Endpoint) resolver.State {
	return grpc.SetClientConnProvider(resolver.State{Endpoints: endpoints}, defaultClientConnProvider)
}

// Tests scenarios where an update from the name resolver is invalid and
// verifies that the balancer transitions to TransientFailure with an
// appropriate error picker. A subsequent valid update should transition the
// channel back to Idle with a queueing picker.
func (s) TestUpdateClientConnState_ResolverError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	defaultTestCfg := &autosharding.LBConfig{
		ChannelFactoryKey:  "test-factory-key",
		AutoShardingTarget: "test-target-%s",
		KeyHeaderName:      "test-header-name",
	}
	providerErr := errors.New("channel factory error")

	tests := []struct {
		name          string
		resolverState resolver.State
		wantPickerErr error
	}{
		{
			name:          "empty-endpoints",
			resolverState: resolverStateWithChannelFactoryAndEndpoints(nil),
			wantPickerErr: errors.New("autosharding: no endpoints from resolver"),
		},
		{
			name:          "endpoints-with-no-addresses",
			resolverState: resolverStateWithChannelFactoryAndEndpoints([]resolver.Endpoint{{Addresses: nil}}),
			wantPickerErr: errors.New("autosharding: no endpoints from resolver"),
		},
		{
			name:          "missing-channel-factory",
			resolverState: resolver.State{Endpoints: []resolver.Endpoint{newTestEndpoint("1.1.1.1:1", "host-0")}},
			wantPickerErr: errors.New("autosharding: no channel factory found in resolver state"),
		},
		{
			name: "channel-factory-returns-error",
			resolverState: grpc.SetClientConnProvider(
				resolver.State{Endpoints: []resolver.Endpoint{newTestEndpoint("1.1.1.1:1", "host-0")}},
				func(string) (grpc.ClientConnInterface, func(), error) {
					return nil, nil, providerErr
				},
			),
			wantPickerErr: fmt.Errorf("autosharding: failed to create gRPC channel for key %q: %v", defaultTestCfg.ChannelFactoryKey, providerErr),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cc := testutils.NewBalancerClientConn(t)
			b := balancer.Get(autosharding.Name).Build(cc, balancer.BuildOptions{})
			defer b.Close()

			// Invalid resolver state should return ErrBadResolverState and
			// transition the channel to TransientFailure with an error picker.
			err := b.UpdateClientConnState(balancer.ClientConnState{
				ResolverState:  tc.resolverState,
				BalancerConfig: defaultTestCfg,
			})
			if !errors.Is(err, balancer.ErrBadResolverState) {
				t.Fatalf("UpdateClientConnState() error = %v, want %v", err, balancer.ErrBadResolverState)
			}
			if err := cc.WaitForConnectivityState(ctx, connectivity.TransientFailure); err != nil {
				t.Fatalf("WaitForConnectivityState(TransientFailure) failed: %v", err)
			}
			if err := cc.WaitForPickerWithErr(ctx, tc.wantPickerErr); err != nil {
				t.Fatalf("WaitForPickerWithErr(%v) failed: %v", tc.wantPickerErr, err)
			}

			// Valid resolver state should transition the channel back to Idle
			// with a queueing picker.
			err = b.UpdateClientConnState(balancer.ClientConnState{
				ResolverState:  resolverStateWithChannelFactoryAndEndpoints([]resolver.Endpoint{newTestEndpoint("1.1.1.1:1", "host-0")}),
				BalancerConfig: defaultTestCfg,
			})
			if err != nil {
				t.Fatalf("UpdateClientConnState() unexpected error: %v", err)
			}
			if err := cc.WaitForConnectivityState(ctx, connectivity.Idle); err != nil {
				t.Fatalf("WaitForConnectivityState(Idle) failed: %v", err)
			}
			if err := cc.WaitForPickerWithErr(ctx, balancer.ErrNoSubConnAvailable); err != nil {
				t.Fatalf("WaitForPickerWithErr(ErrNoSubConnAvailable) failed: %v", err)
			}
		})
	}
}

// Tests that when UpdateClientConnState is called with an empty endpoint list
// after endpoints were previously configured and connected, the child
// endpointsharding balancer is updated with the empty list (shutting down
// existing SubConns) and the channel transitions to TransientFailure.
func (s) TestUpdateClientConnState_EmptyEndpointsClosesChildSubConns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	cc := testutils.NewBalancerClientConn(t)
	b := balancer.Get(autosharding.Name).Build(cc, balancer.BuildOptions{})
	defer b.Close()

	ep := newTestEndpoint("1.1.1.1:1", "host-0")
	if err := b.UpdateClientConnState(balancer.ClientConnState{
		ResolverState:  resolverStateWithChannelFactoryAndEndpoints([]resolver.Endpoint{ep}),
		BalancerConfig: defaultTestCfg,
	}); err != nil {
		t.Fatalf("UpdateClientConnState() unexpected error: %v", err)
	}
	// Drain initial Idle state and queuing picker.
	if err := cc.WaitForConnectivityState(ctx, connectivity.Idle); err != nil {
		t.Fatalf("WaitForConnectivityState(Idle) failed: %v", err)
	}
	<-cc.NewPickerCh

	// Set an assignment and pick to trigger SubConn creation and connection.
	updateAssignmentForTesting(b, &assignment{
		endpointNames: []string{"host-0"},
		slices:        []slice{{startKey: []byte(""), endpoints: []int{0}}},
		generation:    1,
	})
	p := <-cc.NewPickerCh
	if _, err := p.Pick(balancer.PickInfo{Ctx: newContextWithShardingKey(ctx, "a")}); !errors.Is(err, balancer.ErrNoSubConnAvailable) {
		t.Fatalf("Pick() error = %v, want %v", err, balancer.ErrNoSubConnAvailable)
	}

	var sc *testutils.TestSubConn
	select {
	case sc = <-cc.NewSubConnCh:
	case <-ctx.Done():
		t.Fatal("Timeout waiting for SubConn creation")
	}
	select {
	case <-sc.ConnectCh:
	case <-ctx.Done():
		t.Fatal("Timeout waiting for SubConn.Connect()")
	}
	sc.UpdateState(balancer.SubConnState{ConnectivityState: connectivity.Connecting})
	sc.UpdateState(balancer.SubConnState{ConnectivityState: connectivity.Ready})
	if err := cc.WaitForConnectivityState(ctx, connectivity.Ready); err != nil {
		t.Fatalf("WaitForConnectivityState(Ready) failed: %v", err)
	}
	<-cc.NewPickerCh

	// Send an empty endpoint list and verify that the SubConn is shut down and
	// the channel reports TransientFailure with errNoEndpointsFromNR.
	err := b.UpdateClientConnState(balancer.ClientConnState{
		ResolverState:  resolverStateWithChannelFactoryAndEndpoints(nil),
		BalancerConfig: defaultTestCfg,
	})
	if !errors.Is(err, balancer.ErrBadResolverState) {
		t.Fatalf("UpdateClientConnState() error = %v, want %v", err, balancer.ErrBadResolverState)
	}
	select {
	case shutDownSC := <-cc.ShutdownSubConnCh:
		if shutDownSC != sc {
			t.Errorf("ShutdownSubConn = %v, want %v", shutDownSC, sc)
		}
	case <-ctx.Done():
		t.Fatal("Timeout waiting for SubConn shutdown")
	}
	if err := cc.WaitForConnectivityState(ctx, connectivity.TransientFailure); err != nil {
		t.Fatalf("WaitForConnectivityState(TransientFailure) failed: %v", err)
	}
	if err := cc.WaitForPickerWithErr(ctx, errNoEndpointsFromNR); err != nil {
		t.Fatalf("WaitForPickerWithErr(%v) failed: %v", errNoEndpointsFromNR, err)
	}
}

/*

var defaultTestCfg = &lbConfig{
	ChannelFactoryKey:        "test-factory-key",
	AutoShardingTarget:       "test-target-%s",
	KeyHeaderName:            testHeaderName,
	EnableFallback:           true,
	InitialAssignmentTimeout: iserviceconfig.Duration(60 * time.Second),
}

type testClientConn struct {
	grpc.ClientConnInterface
	key string
}

func defaultClientConnProvider(key string) (grpc.ClientConnInterface, func(), error) {
	return &testClientConn{key: key}, func() {}, nil
}

func newTestEndpoint(addr, host string) resolver.Endpoint {
	ep := resolver.Endpoint{Addresses: []resolver.Address{{Addr: addr}}}
	if host != "" {
		ep = hostname.Set(ep, host)
	}
	return ep
}

func resolverStateWithChannelFactoryAndEndpoints(endpoints []resolver.Endpoint) resolver.State {
	return grpc.SetClientConnProvider(resolver.State{Endpoints: endpoints}, defaultClientConnProvider)
}

// updateAssignmentForTesting simulates receiving a new assignment from the
// autosharding client.
func updateAssignmentForTesting(b balancer.Balancer, a *assignment) {
	ab := b.(*autoshardingBalancer)
	ab.mu.Lock()
	defer ab.mu.Unlock()
	ab.assignment = a
	ab.shouldRegenerateSliceMap = true
	ab.updateStateAndPickerLocked()
}



// Tests ResolverError behavior both before any valid endpoints are received
// (channel transitions to TransientFailure with the resolver error) and after
// valid endpoints are present (channel retains its active state and picker).
func (s) TestResolverError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	cc := testutils.NewBalancerClientConn(t)
	b := balancer.Get(Name).Build(cc, balancer.BuildOptions{})
	defer b.Close()

	// 1. ResolverError before any valid endpoints are received.
	resolverErr := errors.New("test name resolver error")
	b.ResolverError(resolverErr)
	if err := cc.WaitForConnectivityState(ctx, connectivity.TransientFailure); err != nil {
		t.Fatalf("WaitForConnectivityState(TransientFailure) failed: %v", err)
	}
	if err := cc.WaitForPickerWithErr(ctx, resolverErr); err != nil {
		t.Fatalf("WaitForPickerWithErr(%v) failed: %v", resolverErr, err)
	}

	// 2. Send a valid update, set an assignment, and bring the endpoint to Ready.
	ep := newTestEndpoint("1.1.1.1:1", "host-0")
	if err := b.UpdateClientConnState(balancer.ClientConnState{
		ResolverState:  resolverStateWithChannelFactoryAndEndpoints([]resolver.Endpoint{ep}),
		BalancerConfig: defaultTestCfg,
	}); err != nil {
		t.Fatalf("UpdateClientConnState() unexpected error: %v", err)
	}
	if err := cc.WaitForConnectivityState(ctx, connectivity.Idle); err != nil {
		t.Fatalf("WaitForConnectivityState(Idle) failed: %v", err)
	}
	<-cc.NewPickerCh

	updateAssignmentForTesting(b, &assignment{
		endpointNames: []string{"host-0"},
		slices:        []slice{{startKey: []byte(""), endpoints: []int{0}}},
		generation:    1,
	})
	p := <-cc.NewPickerCh
	if _, err := p.Pick(balancer.PickInfo{Ctx: newContextWithShardingKey(ctx, "a")}); !errors.Is(err, balancer.ErrNoSubConnAvailable) {
		t.Fatalf("Pick() error = %v, want %v", err, balancer.ErrNoSubConnAvailable)
	}

	var sc *testutils.TestSubConn
	select {
	case sc = <-cc.NewSubConnCh:
	case <-ctx.Done():
		t.Fatal("Timeout waiting for SubConn creation")
	}
	select {
	case <-sc.ConnectCh:
	case <-ctx.Done():
		t.Fatal("Timeout waiting for SubConn.Connect()")
	}
	sc.UpdateState(balancer.SubConnState{ConnectivityState: connectivity.Connecting})
	sc.UpdateState(balancer.SubConnState{ConnectivityState: connectivity.Ready})
	if err := cc.WaitForConnectivityState(ctx, connectivity.Ready); err != nil {
		t.Fatalf("WaitForConnectivityState(Ready) failed: %v", err)
	}
	p = <-cc.NewPickerCh

	// 3. ResolverError after valid endpoints exist should not put the channel in
	// TransientFailure; picks should continue to succeed.
	b.ResolverError(resolverErr)
	select {
	case st := <-cc.NewStateCh:
		if st != connectivity.Ready {
			t.Fatalf("Unexpected state transition to %v after ResolverError, want Ready", st)
		}
		p = <-cc.NewPickerCh
	case <-time.After(defaultTestShortTimeout):
	}
	gotPick, err := p.Pick(balancer.PickInfo{Ctx: newContextWithShardingKey(ctx, "a")})
	if err != nil {
		t.Fatalf("Pick() after ResolverError failed: %v", err)
	}
	if gotPick.SubConn != sc {
		t.Errorf("Pick() SubConn = %v, want %v", gotPick.SubConn, sc)
	}
}

// Tests the lifecycle of the gRPC channel created via ClientConnProvider and
// the autoshardingClient across configuration and locality updates, as well as
// on Close().
func (s) TestUpdateClientConnState_ChannelAndClientLifecycle(t *testing.T) {
	cc := testutils.NewBalancerClientConn(t)
	b := balancer.Get(Name).Build(cc, balancer.BuildOptions{})
	ab := b.(*autoshardingBalancer)

	var createdKeys []string
	canceledKeys := make(map[string]int)
	channelsByKey := make(map[string]*testClientConn)
	provider := func(key string) (grpc.ClientConnInterface, func(), error) {
		createdKeys = append(createdKeys, key)
		ch := &testClientConn{key: key}
		channelsByKey[key] = ch
		return ch, func() { canceledKeys[key]++ }, nil
	}

	endpoints := []resolver.Endpoint{newTestEndpoint("1.1.1.1:1", "host-0")}
	baseState := grpc.SetClientConnProvider(resolver.State{Endpoints: endpoints}, provider)

	// 1. Initial update with locality "us-central1".
	cfg1 := &lbConfig{
		ChannelFactoryKey:        "key-1",
		AutoShardingTarget:       "sharding.%s.example.com",
		KeyHeaderName:            testHeaderName,
		InitialAssignmentTimeout: iserviceconfig.Duration(30 * time.Second),
	}
	if err := b.UpdateClientConnState(balancer.ClientConnState{
		ResolverState:  locality.Set(baseState, "us-central1"),
		BalancerConfig: cfg1,
	}); err != nil {
		t.Fatalf("UpdateClientConnState() unexpected error: %v", err)
	}
	if diff := cmp.Diff([]string{"key-1"}, createdKeys); diff != "" {
		t.Fatalf("Provider createdKeys diff (-want +got):\n%s", diff)
	}
	if len(canceledKeys) != 0 {
		t.Fatalf("canceledKeys = %v, want empty", canceledKeys)
	}
	client1 := ab.autoshardingClient
	if client1 == nil {
		t.Fatal("autoshardingClient is nil, want non-nil")
	}
	if got, want := client1.target, "sharding.us-central1.example.com"; got != want {
		t.Errorf("autoshardingClient.target = %q, want %q", got, want)
	}
	if got, want := client1.timeout, 30*time.Second; got != want {
		t.Errorf("autoshardingClient.timeout = %v, want %v", got, want)
	}
	if got, want := client1.uuid, ab.uuid; got == "" || got != want {
		t.Errorf("autoshardingClient.uuid = %q, want non-empty %q", got, want)
	}
	if client1.cc != channelsByKey["key-1"] {
		t.Errorf("autoshardingClient.cc = %v, want %v", client1.cc, channelsByKey["key-1"])
	}

	// 2. Update with the same ChannelFactoryKey and same resolved target:
	// should reuse both the gRPC channel and the autoshardingClient.
	if err := b.UpdateClientConnState(balancer.ClientConnState{
		ResolverState:  locality.Set(baseState, "us-central1"),
		BalancerConfig: cfg1,
	}); err != nil {
		t.Fatalf("UpdateClientConnState() unexpected error: %v", err)
	}
	if diff := cmp.Diff([]string{"key-1"}, createdKeys); diff != "" {
		t.Fatalf("Provider createdKeys diff (-want +got):\n%s", diff)
	}
	if len(canceledKeys) != 0 {
		t.Fatalf("canceledKeys = %v, want empty", canceledKeys)
	}
	if ab.autoshardingClient != client1 {
		t.Errorf("autoshardingClient was recreated when target and key did not change")
	}

	// 3. Update with the same ChannelFactoryKey but a different locality:
	// should reuse the gRPC channel (without canceling it) and create a new
	// autoshardingClient with the updated target and same UUID.
	if err := b.UpdateClientConnState(balancer.ClientConnState{
		ResolverState:  locality.Set(baseState, "europe-west1"),
		BalancerConfig: cfg1,
	}); err != nil {
		t.Fatalf("UpdateClientConnState() unexpected error: %v", err)
	}
	if diff := cmp.Diff([]string{"key-1"}, createdKeys); diff != "" {
		t.Fatalf("Provider createdKeys diff (-want +got):\n%s", diff)
	}
	if len(canceledKeys) != 0 {
		t.Fatalf("canceledKeys = %v, want empty", canceledKeys)
	}
	client2 := ab.autoshardingClient
	if client2 == client1 {
		t.Fatal("autoshardingClient was not recreated when resolved target changed")
	}
	if got, want := client2.target, "sharding.europe-west1.example.com"; got != want {
		t.Errorf("autoshardingClient.target = %q, want %q", got, want)
	}
	if client2.cc != channelsByKey["key-1"] {
		t.Errorf("autoshardingClient.cc = %v, want %v", client2.cc, channelsByKey["key-1"])
	}
	if got, want := client2.uuid, ab.uuid; got != want {
		t.Errorf("autoshardingClient.uuid = %q, want %q", got, want)
	}

	// 4. Update with missing locality attribute: %s should be replaced with "".
	if err := b.UpdateClientConnState(balancer.ClientConnState{
		ResolverState:  baseState,
		BalancerConfig: cfg1,
	}); err != nil {
		t.Fatalf("UpdateClientConnState() unexpected error: %v", err)
	}
	client3 := ab.autoshardingClient
	if client3 == client2 {
		t.Fatal("autoshardingClient was not recreated when locality was removed")
	}
	if got, want := client3.target, "sharding..example.com"; got != want {
		t.Errorf("autoshardingClient.target = %q, want %q", got, want)
	}

	// 5. Update with a new ChannelFactoryKey: should create a new gRPC channel,
	// cancel the old channel, and create a new autoshardingClient.
	cfg2 := &lbConfig{
		ChannelFactoryKey:        "key-2",
		AutoShardingTarget:       "sharding.%s.example.com",
		KeyHeaderName:            testHeaderName,
		InitialAssignmentTimeout: iserviceconfig.Duration(30 * time.Second),
	}
	if err := b.UpdateClientConnState(balancer.ClientConnState{
		ResolverState:  locality.Set(baseState, "us-east1"),
		BalancerConfig: cfg2,
	}); err != nil {
		t.Fatalf("UpdateClientConnState() unexpected error: %v", err)
	}
	if diff := cmp.Diff([]string{"key-1", "key-2"}, createdKeys); diff != "" {
		t.Fatalf("Provider createdKeys diff (-want +got):\n%s", diff)
	}
	if got := canceledKeys["key-1"]; got != 1 {
		t.Errorf("canceledKeys[\"key-1\"] = %d, want 1", got)
	}
	if got := canceledKeys["key-2"]; got != 0 {
		t.Errorf("canceledKeys[\"key-2\"] = %d, want 0", got)
	}
	client4 := ab.autoshardingClient
	if client4 == client3 {
		t.Fatal("autoshardingClient was not recreated when ChannelFactoryKey changed")
	}
	if client4.cc != channelsByKey["key-2"] {
		t.Errorf("autoshardingClient.cc = %v, want %v", client4.cc, channelsByKey["key-2"])
	}
	if got, want := client4.target, "sharding.us-east1.example.com"; got != want {
		t.Errorf("autoshardingClient.target = %q, want %q", got, want)
	}

	// 6. Close() should cancel the active channel and clear references.
	b.Close()
	if got := canceledKeys["key-2"]; got != 1 {
		t.Errorf("canceledKeys[\"key-2\"] after Close() = %d, want 1", got)
	}
	if ab.autoshardingClient != nil || ab.autoshardingChannel != nil || ab.autoshardingChannelClose != nil {
		t.Errorf("Close() did not clear channel/client fields: client=%v, channel=%v", ab.autoshardingClient, ab.autoshardingChannel)
	}
}

// Tests that endpoints with zero addresses are ignored and duplicate endpoints
// (by hostname attribute, or falling back to first address when hostname is
// unset) are deduplicated, keeping the first occurrence.
func (s) TestUpdateClientConnState_EndpointFilteringAndDeduplication(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	cc := testutils.NewBalancerClientConn(t)
	b := balancer.Get(Name).Build(cc, balancer.BuildOptions{})
	defer b.Close()

	endpoints := []resolver.Endpoint{
		// 1. Zero addresses -> ignored.
		{Addresses: nil},
		// 2. Hostname "host-a" -> kept at index 0 with address "1.1.1.1:1".
		newTestEndpoint("1.1.1.1:1", "host-a"),
		// 3. Duplicate hostname "host-a" -> ignored.
		newTestEndpoint("2.2.2.2:2", "host-a"),
		// 4. No hostname attribute -> falls back to Addresses[0].Addr "3.3.3.3:3", kept at index 1.
		newTestEndpoint("3.3.3.3:3", ""),
		// 5. Duplicate fallback hostname "3.3.3.3:3" -> ignored.
		{Addresses: []resolver.Address{{Addr: "3.3.3.3:3"}, {Addr: "4.4.4.4:4"}}},
	}

	if err := b.UpdateClientConnState(balancer.ClientConnState{
		ResolverState:  resolverStateWithChannelFactoryAndEndpoints(endpoints),
		BalancerConfig: defaultTestCfg,
	}); err != nil {
		t.Fatalf("UpdateClientConnState() unexpected error: %v", err)
	}
	<-cc.NewPickerCh

	updateAssignmentForTesting(b, &assignment{
		endpointNames: []string{"host-a", "3.3.3.3:3"},
		slices: []slice{
			{startKey: []byte(""), endpoints: []int{0}},
			{startKey: []byte("m"), endpoints: []int{1}},
		},
		generation: 1,
	})

	p := (<-cc.NewPickerCh).(*picker)
	if got, want := len(p.endpoints), 2; got != want {
		t.Fatalf("len(picker.endpoints) = %d, want %d", got, want)
	}

	// Picking key "a" (slice 0 -> "host-a") should create a SubConn for "1.1.1.1:1".
	if _, err := p.Pick(balancer.PickInfo{Ctx: newContextWithShardingKey(ctx, "a")}); !errors.Is(err, balancer.ErrNoSubConnAvailable) {
		t.Fatalf("Pick(\"a\") error = %v, want %v", err, balancer.ErrNoSubConnAvailable)
	}
	select {
	case sc0 := <-cc.NewSubConnCh:
		if diff := cmp.Diff([]resolver.Address{{Addr: "1.1.1.1:1"}}, sc0.Addresses); diff != "" {
			t.Errorf("sc0.Addresses diff (-want +got):\n%s", diff)
		}
	case <-ctx.Done():
		t.Fatal("Timeout waiting for sc0 creation")
	}

	// Picking key "z" (slice 1 -> "3.3.3.3:3") should create a SubConn for "3.3.3.3:3".
	if _, err := p.Pick(balancer.PickInfo{Ctx: newContextWithShardingKey(ctx, "z")}); !errors.Is(err, balancer.ErrNoSubConnAvailable) {
		t.Fatalf("Pick(\"z\") error = %v, want %v", err, balancer.ErrNoSubConnAvailable)
	}
	select {
	case sc1 := <-cc.NewSubConnCh:
		if diff := cmp.Diff([]resolver.Address{{Addr: "3.3.3.3:3"}}, sc1.Addresses); diff != "" {
			t.Errorf("sc1.Addresses diff (-want +got):\n%s", diff)
		}
	case <-ctx.Done():
		t.Fatal("Timeout waiting for sc1 creation")
	}
}

// Tests that the sliceMap is reused across UpdateClientConnState calls when the
// endpoint count and order do not change, and is regenerated when endpoints are
// reordered, added, removed, or replaced.
func (s) TestUpdateClientConnState_SliceMapRegeneration(t *testing.T) {
	cc := testutils.NewBalancerClientConn(t)
	b := balancer.Get(Name).Build(cc, balancer.BuildOptions{})
	defer b.Close()

	ep0 := newTestEndpoint("1.1.1.1:1", "host-0")
	ep1 := newTestEndpoint("2.2.2.2:2", "host-1")
	ep2 := newTestEndpoint("3.3.3.3:3", "host-2")

	if err := b.UpdateClientConnState(balancer.ClientConnState{
		ResolverState:  resolverStateWithChannelFactoryAndEndpoints([]resolver.Endpoint{ep0, ep1, ep2}),
		BalancerConfig: defaultTestCfg,
	}); err != nil {
		t.Fatalf("UpdateClientConnState() unexpected error: %v", err)
	}
	<-cc.NewPickerCh

	updateAssignmentForTesting(b, &assignment{
		endpointNames: []string{"host-0", "host-1", "host-2"},
		slices: []slice{
			{startKey: []byte(""), endpoints: []int{0, 1}},
			{startKey: []byte("m"), endpoints: []int{1, 2}},
		},
		generation: 1,
	})
	p0 := (<-cc.NewPickerCh).(*picker)
	sm0 := p0.sliceMap
	wantSM0 := &sliceMap{
		slices: []sliceMapEntry{
			{startKey: []byte(""), endpoints: []int{0, 1}},
			{startKey: []byte("m"), endpoints: []int{1, 2}},
		},
		fallbackPool: []int{0, 1, 2},
		generation:   1,
	}
	if diff := cmp.Diff(wantSM0, sm0); diff != "" {
		t.Fatalf("Initial sliceMap diff (-want +got):\n%s", diff)
	}

	// 1. Update with the same endpoints in the same order: sliceMap pointer
	// must be reused.
	if err := b.UpdateClientConnState(balancer.ClientConnState{
		ResolverState:  resolverStateWithChannelFactoryAndEndpoints([]resolver.Endpoint{ep0, ep1, ep2}),
		BalancerConfig: defaultTestCfg,
	}); err != nil {
		t.Fatalf("UpdateClientConnState() unexpected error: %v", err)
	}
	p1 := (<-cc.NewPickerCh).(*picker)
	if p1.sliceMap != sm0 {
		t.Fatalf("Picker sliceMap was regenerated when endpoint order and count did not change")
	}

	// 2. Update with the same endpoints in a different order ([ep1, ep0, ep2]):
	// sliceMap must be regenerated with updated endpoint indices.
	if err := b.UpdateClientConnState(balancer.ClientConnState{
		ResolverState:  resolverStateWithChannelFactoryAndEndpoints([]resolver.Endpoint{ep1, ep0, ep2}),
		BalancerConfig: defaultTestCfg,
	}); err != nil {
		t.Fatalf("UpdateClientConnState() unexpected error: %v", err)
	}
	p2 := (<-cc.NewPickerCh).(*picker)
	if p2.sliceMap == sm0 {
		t.Fatalf("Picker sliceMap was not regenerated when endpoint order changed")
	}
	wantSM2 := &sliceMap{
		slices: []sliceMapEntry{
			{startKey: []byte(""), endpoints: []int{1, 0}},
			{startKey: []byte("m"), endpoints: []int{0, 2}},
		},
		fallbackPool: []int{0, 1, 2},
		generation:   1,
	}
	if diff := cmp.Diff(wantSM2, p2.sliceMap); diff != "" {
		t.Fatalf("Reordered sliceMap diff (-want +got):\n%s", diff)
	}

	// 3. Update with an endpoint removed ([ep1, ep0]): sliceMap must be
	// regenerated.
	if err := b.UpdateClientConnState(balancer.ClientConnState{
		ResolverState:  resolverStateWithChannelFactoryAndEndpoints([]resolver.Endpoint{ep1, ep0}),
		BalancerConfig: defaultTestCfg,
	}); err != nil {
		t.Fatalf("UpdateClientConnState() unexpected error: %v", err)
	}
	p3 := (<-cc.NewPickerCh).(*picker)
	if p3.sliceMap == p2.sliceMap {
		t.Fatalf("Picker sliceMap was not regenerated when endpoint count changed")
	}
	wantSM3 := &sliceMap{
		slices: []sliceMapEntry{
			{startKey: []byte(""), endpoints: []int{1, 0}},
			{startKey: []byte("m"), endpoints: []int{0}},
		},
		fallbackPool: []int{0, 1},
		generation:   1,
	}
	if diff := cmp.Diff(wantSM3, p3.sliceMap); diff != "" {
		t.Fatalf("Removed-endpoint sliceMap diff (-want +got):\n%s", diff)
	}

	// 4. Update replacing ep0 with ep2 while keeping the count at 2 ([ep1, ep2]):
	// sliceMap must be regenerated because ep2 is a new endpoint.
	if err := b.UpdateClientConnState(balancer.ClientConnState{
		ResolverState:  resolverStateWithChannelFactoryAndEndpoints([]resolver.Endpoint{ep1, ep2}),
		BalancerConfig: defaultTestCfg,
	}); err != nil {
		t.Fatalf("UpdateClientConnState() unexpected error: %v", err)
	}
	p4 := (<-cc.NewPickerCh).(*picker)
	if p4.sliceMap == p3.sliceMap {
		t.Fatalf("Picker sliceMap was not regenerated when an endpoint was replaced")
	}
	wantSM4 := &sliceMap{
		slices: []sliceMapEntry{
			{startKey: []byte(""), endpoints: []int{0}},
			{startKey: []byte("m"), endpoints: []int{0, 1}},
		},
		fallbackPool: []int{0, 1},
		generation:   1,
	}
	if diff := cmp.Diff(wantSM4, p4.sliceMap); diff != "" {
		t.Fatalf("Replaced-endpoint sliceMap diff (-want +got):\n%s", diff)
	}
}

// Tests that before an initial assignment (or assignment error) is received,
// the balancer reports connectivity.Idle and queues RPCs with
// balancer.ErrNoSubConnAvailable without creating or connecting any SubConns.
func (s) TestWaitingForInitialAssignment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	cc := testutils.NewBalancerClientConn(t)
	b := balancer.Get(Name).Build(cc, balancer.BuildOptions{})
	defer b.Close()

	ep := newTestEndpoint("1.1.1.1:1", "host-0")
	if err := b.UpdateClientConnState(balancer.ClientConnState{
		ResolverState:  resolverStateWithChannelFactoryAndEndpoints([]resolver.Endpoint{ep}),
		BalancerConfig: defaultTestCfg,
	}); err != nil {
		t.Fatalf("UpdateClientConnState() unexpected error: %v", err)
	}

	if err := cc.WaitForConnectivityState(ctx, connectivity.Idle); err != nil {
		t.Fatalf("WaitForConnectivityState(Idle) failed: %v", err)
	}
	p := <-cc.NewPickerCh
	if _, err := p.Pick(balancer.PickInfo{Ctx: newContextWithShardingKey(ctx, "a")}); !errors.Is(err, balancer.ErrNoSubConnAvailable) {
		t.Fatalf("Pick() before assignment returned %v, want %v", err, balancer.ErrNoSubConnAvailable)
	}

	// No SubConn should be created while waiting for the initial assignment.
	select {
	case sc := <-cc.NewSubConnCh:
		t.Fatalf("Unexpected SubConn creation before assignment: %v", sc)
	case <-time.After(defaultTestShortTimeout):
	}
}

// Tests the connectivity state aggregation rules in updateStateAndPickerLocked.
func (s) TestAggregatedConnectivityState(t *testing.T) {
	tests := []struct {
		name           string
		endpointStates []connectivity.State
		want           connectivity.State
	}{
		{
			name:           "one-ready",
			endpointStates: []connectivity.State{connectivity.Ready},
			want:           connectivity.Ready,
		},
		{
			name:           "one-connecting",
			endpointStates: []connectivity.State{connectivity.Connecting},
			want:           connectivity.Connecting,
		},
		{
			name:           "one-idle",
			endpointStates: []connectivity.State{connectivity.Idle},
			want:           connectivity.Idle,
		},
		{
			name:           "one-transient-failure",
			endpointStates: []connectivity.State{connectivity.TransientFailure},
			want:           connectivity.TransientFailure,
		},
		{
			name:           "one-ready-one-transient-failure",
			endpointStates: []connectivity.State{connectivity.Ready, connectivity.TransientFailure},
			want:           connectivity.Ready,
		},
		{
			name:           "one-ready-two-transient-failure",
			endpointStates: []connectivity.State{connectivity.Ready, connectivity.TransientFailure, connectivity.TransientFailure},
			want:           connectivity.Ready,
		},
		{
			name:           "one-connecting-one-transient-failure",
			endpointStates: []connectivity.State{connectivity.Connecting, connectivity.TransientFailure},
			want:           connectivity.Connecting,
		},
		{
			name:           "one-connecting-two-transient-failure",
			endpointStates: []connectivity.State{connectivity.Connecting, connectivity.TransientFailure, connectivity.TransientFailure},
			want:           connectivity.TransientFailure,
		},
		{
			name:           "one-transient-failure-one-idle",
			endpointStates: []connectivity.State{connectivity.TransientFailure, connectivity.Idle},
			want:           connectivity.Connecting,
		},
		{
			name:           "two-transient-failure-one-idle",
			endpointStates: []connectivity.State{connectivity.TransientFailure, connectivity.TransientFailure, connectivity.Idle},
			want:           connectivity.TransientFailure,
		},
		{
			name:           "one-connecting-one-idle",
			endpointStates: []connectivity.State{connectivity.Connecting, connectivity.Idle},
			want:           connectivity.Connecting,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cc := testutils.NewBalancerClientConn(t)
			b := &autoshardingBalancer{
				ClientConn:  cc,
				lbCfg:       *defaultTestCfg,
				assignment:  &assignment{},
				endpointMap: make(map[string]*endpointState, len(tc.endpointStates)),
				sliceMap:    &sliceMap{},
			}
			for i, cs := range tc.endpointStates {
				host := fmt.Sprintf("host-%d", i)
				b.endpointMap[host] = &endpointState{
					index: i,
					childState: endpointsharding.ChildState{
						State:    balancer.State{ConnectivityState: cs},
						ExitIdle: func() {},
					},
				}
			}

			b.mu.Lock()
			b.updateStateAndPickerLocked()
			b.mu.Unlock()

			got := <-cc.NewStateCh
			if got != tc.want {
				t.Errorf("Aggregated connectivity state = %v, want %v", got, tc.want)
			}
		})
	}
}

// Tests that when an endpoint enters TransientFailure and no other endpoint is
// currently in Connecting, the balancer automatically triggers ExitIdle on the
// lowest-index Idle endpoint without requiring a pick. Also verifies that no
// additional Idle endpoint is woken if another endpoint is already in
// Connecting.
func (s) TestAutoConnectEndpointOnTransientFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	cc := testutils.NewBalancerClientConn(t)
	b := balancer.Get(Name).Build(cc, balancer.BuildOptions{})
	defer b.Close()

	endpoints := []resolver.Endpoint{
		newTestEndpoint("0.0.0.0:0", "host-0"),
		newTestEndpoint("1.1.1.1:1", "host-1"),
		newTestEndpoint("2.2.2.2:2", "host-2"),
		newTestEndpoint("3.3.3.3:3", "host-3"),
	}
	if err := b.UpdateClientConnState(balancer.ClientConnState{
		ResolverState:  resolverStateWithChannelFactoryAndEndpoints(endpoints),
		BalancerConfig: defaultTestCfg,
	}); err != nil {
		t.Fatalf("UpdateClientConnState() unexpected error: %v", err)
	}
	<-cc.NewPickerCh

	updateAssignmentForTesting(b, &assignment{
		endpointNames: []string{"host-0", "host-1", "host-2", "host-3"},
		slices:        []slice{{startKey: []byte(""), endpoints: []int{0}}},
		generation:    1,
	})
	p0 := <-cc.NewPickerCh

	// Trigger a pick to connect the first endpoint (host-0).
	pickCtx := newContextWithShardingKey(ctx, "a")
	if _, err := p0.Pick(balancer.PickInfo{Ctx: pickCtx}); !errors.Is(err, balancer.ErrNoSubConnAvailable) {
		t.Fatalf("Pick() error = %v, want %v", err, balancer.ErrNoSubConnAvailable)
	}
	var sc0 *testutils.TestSubConn
	select {
	case sc0 = <-cc.NewSubConnCh:
	case <-ctx.Done():
		t.Fatal("Timeout waiting for sc0 creation")
	}
	if got, want := sc0.Addresses[0].Addr, "0.0.0.0:0"; got != want {
		t.Fatalf("sc0.Addresses[0].Addr = %q, want %q", got, want)
	}
	select {
	case <-sc0.ConnectCh:
	case <-ctx.Done():
		t.Fatal("Timeout waiting for sc0.Connect()")
	}

	// Move sc0 to TransientFailure. With 1 endpoint in TF and 3 in Idle,
	// aggregated state is Connecting, and host-1 (lowest index Idle) should
	// automatically be asked to connect.
	sc0.UpdateState(balancer.SubConnState{ConnectivityState: connectivity.Connecting})
	sc0.UpdateState(balancer.SubConnState{ConnectivityState: connectivity.TransientFailure})
	if err := cc.WaitForConnectivityState(ctx, connectivity.Connecting); err != nil {
		t.Fatalf("WaitForConnectivityState(Connecting) failed: %v", err)
	}

	var sc1 *testutils.TestSubConn
	select {
	case sc1 = <-cc.NewSubConnCh:
	case <-ctx.Done():
		t.Fatal("Timeout waiting for sc1 creation")
	}
	if got, want := sc1.Addresses[0].Addr, "1.1.1.1:1"; got != want {
		t.Fatalf("sc1.Addresses[0].Addr = %q, want %q", got, want)
	}
	select {
	case <-sc1.ConnectCh:
	case <-ctx.Done():
		t.Fatal("Timeout waiting for sc1.Connect()")
	}

	// Move sc1 to TransientFailure. With 2 endpoints in TF, aggregated state is
	// TransientFailure, and host-2 (next lowest index Idle) should automatically
	// be asked to connect.
	sc1.UpdateState(balancer.SubConnState{ConnectivityState: connectivity.Connecting})
	sc1.UpdateState(balancer.SubConnState{ConnectivityState: connectivity.TransientFailure})
	if err := cc.WaitForConnectivityState(ctx, connectivity.TransientFailure); err != nil {
		t.Fatalf("WaitForConnectivityState(TransientFailure) failed: %v", err)
	}

	var sc2 *testutils.TestSubConn
	select {
	case sc2 = <-cc.NewSubConnCh:
	case <-ctx.Done():
		t.Fatal("Timeout waiting for sc2 creation")
	}
	if got, want := sc2.Addresses[0].Addr, "2.2.2.2:2"; got != want {
		t.Fatalf("sc2.Addresses[0].Addr = %q, want %q", got, want)
	}
	select {
	case <-sc2.ConnectCh:
	case <-ctx.Done():
		t.Fatal("Timeout waiting for sc2.Connect()")
	}
	sc2.UpdateState(balancer.SubConnState{ConnectivityState: connectivity.Connecting})

	// Put sc0 into Connecting (via Ready -> Idle -> Pick -> Connecting).
	sc0.UpdateState(balancer.SubConnState{ConnectivityState: connectivity.Ready})
	if err := cc.WaitForConnectivityState(ctx, connectivity.Ready); err != nil {
		t.Fatalf("WaitForConnectivityState(Ready) failed: %v", err)
	}
	sc0.UpdateState(balancer.SubConnState{ConnectivityState: connectivity.Idle})
	if err := cc.WaitForConnectivityState(ctx, connectivity.Connecting); err != nil {
		t.Fatalf("WaitForConnectivityState(Connecting) failed: %v", err)
	}
	p1 := <-cc.NewPickerCh
	p1.Pick(balancer.PickInfo{Ctx: pickCtx})
	select {
	case <-sc0.ConnectCh:
	case <-ctx.Done():
		t.Fatal("Timeout waiting for sc0.Connect()")
	}
	sc0.UpdateState(balancer.SubConnState{ConnectivityState: connectivity.Connecting})

	// Now transition sc2 to TransientFailure. Because sc0 is still in
	// Connecting, host-3 (Idle) must not be woken.
	sc2.UpdateState(balancer.SubConnState{ConnectivityState: connectivity.TransientFailure})
	select {
	case sc := <-cc.NewSubConnCh:
		t.Fatalf("Unexpected SubConn creation when an endpoint is already Connecting: %v", sc)
	case <-sc0.ConnectCh:
		t.Fatalf("Unexpected Connect() on sc0")
	case <-sc1.ConnectCh:
		t.Fatalf("Unexpected Connect() on sc1")
	case <-sc2.ConnectCh:
		t.Fatalf("Unexpected Connect() on sc2")
	case <-time.After(defaultTestShortTimeout):
	}
}

// Tests end-to-end RPC routing across multiple slices and endpoints, verifying
// that picks lazily connect only the endpoint assigned to the matching slice.
func (s) TestEndToEndPickRouting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	cc := testutils.NewBalancerClientConn(t)
	b := balancer.Get(Name).Build(cc, balancer.BuildOptions{})
	defer b.Close()

	endpoints := []resolver.Endpoint{
		newTestEndpoint("0.0.0.0:0", "host-0"),
		newTestEndpoint("1.1.1.1:1", "host-1"),
	}
	if err := b.UpdateClientConnState(balancer.ClientConnState{
		ResolverState:  resolverStateWithChannelFactoryAndEndpoints(endpoints),
		BalancerConfig: defaultTestCfg,
	}); err != nil {
		t.Fatalf("UpdateClientConnState() unexpected error: %v", err)
	}
	<-cc.NewPickerCh

	// Slice 0 ["", "m") -> host-0; Slice 1 ["m", infinity) -> host-1.
	updateAssignmentForTesting(b, &assignment{
		endpointNames: []string{"host-0", "host-1"},
		slices: []slice{
			{startKey: []byte(""), endpoints: []int{0}},
			{startKey: []byte("m"), endpoints: []int{1}},
		},
		generation: 1,
	})
	p0 := <-cc.NewPickerCh

	// 1. Pick key "z" (matches slice 1 -> host-1). Only host-1 should connect.
	if _, err := p0.Pick(balancer.PickInfo{Ctx: newContextWithShardingKey(ctx, "z")}); !errors.Is(err, balancer.ErrNoSubConnAvailable) {
		t.Fatalf("Pick(\"z\") error = %v, want %v", err, balancer.ErrNoSubConnAvailable)
	}
	var sc1 *testutils.TestSubConn
	select {
	case sc1 = <-cc.NewSubConnCh:
	case <-ctx.Done():
		t.Fatal("Timeout waiting for sc1 creation")
	}
	if got, want := sc1.Addresses[0].Addr, "1.1.1.1:1"; got != want {
		t.Fatalf("sc1.Addresses[0].Addr = %q, want %q", got, want)
	}
	select {
	case <-sc1.ConnectCh:
	case <-ctx.Done():
		t.Fatal("Timeout waiting for sc1.Connect()")
	}

	sc1.UpdateState(balancer.SubConnState{ConnectivityState: connectivity.Connecting})
	sc1.UpdateState(balancer.SubConnState{ConnectivityState: connectivity.Ready})
	if err := cc.WaitForConnectivityState(ctx, connectivity.Ready); err != nil {
		t.Fatalf("WaitForConnectivityState(Ready) failed: %v", err)
	}
	p1 := <-cc.NewPickerCh

	res, err := p1.Pick(balancer.PickInfo{Ctx: newContextWithShardingKey(ctx, "z")})
	if err != nil || res.SubConn != sc1 {
		t.Fatalf("Pick(\"z\") = (%v, %v), want SubConn %v", res, err, sc1)
	}

	// host-0 should still be Idle (no SubConn created yet).
	select {
	case sc := <-cc.NewSubConnCh:
		t.Fatalf("Unexpected SubConn creation for unpicked slice: %v", sc)
	case <-time.After(defaultTestShortTimeout):
	}

	// 2. Pick key "a" (matches slice 0 -> host-0). Now host-0 should connect.
	if _, err := p1.Pick(balancer.PickInfo{Ctx: newContextWithShardingKey(ctx, "a")}); !errors.Is(err, balancer.ErrNoSubConnAvailable) {
		t.Fatalf("Pick(\"a\") error = %v, want %v", err, balancer.ErrNoSubConnAvailable)
	}
	var sc0 *testutils.TestSubConn
	select {
	case sc0 = <-cc.NewSubConnCh:
	case <-ctx.Done():
		t.Fatal("Timeout waiting for sc0 creation")
	}
	if got, want := sc0.Addresses[0].Addr, "0.0.0.0:0"; got != want {
		t.Fatalf("sc0.Addresses[0].Addr = %q, want %q", got, want)
	}
	select {
	case <-sc0.ConnectCh:
	case <-ctx.Done():
		t.Fatal("Timeout waiting for sc0.Connect()")
	}

	sc0.UpdateState(balancer.SubConnState{ConnectivityState: connectivity.Connecting})
	sc0.UpdateState(balancer.SubConnState{ConnectivityState: connectivity.Ready})
	p2 := <-cc.NewPickerCh

	resA, err := p2.Pick(balancer.PickInfo{Ctx: newContextWithShardingKey(ctx, "a")})
	if err != nil || resA.SubConn != sc0 {
		t.Fatalf("Pick(\"a\") = (%v, %v), want SubConn %v", resA, err, sc0)
	}
	resZ, err := p2.Pick(balancer.PickInfo{Ctx: newContextWithShardingKey(ctx, "z")})
	if err != nil || resZ.SubConn != sc1 {
		t.Fatalf("Pick(\"z\") = (%v, %v), want SubConn %v", resZ, err, sc1)
	}
}
*/
