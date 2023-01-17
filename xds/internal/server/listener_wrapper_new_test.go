/*
 *
 * Copyright 2021 gRPC authors.
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

package server_test

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	"google.golang.org/grpc/internal/envconfig"
	"google.golang.org/grpc/internal/grpctest"
	"google.golang.org/grpc/internal/testutils"
	xdsbootstrap "google.golang.org/grpc/internal/testutils/xds/bootstrap"
	"google.golang.org/grpc/internal/testutils/xds/e2e"
	"google.golang.org/grpc/xds/internal/server"
	"google.golang.org/grpc/xds/internal/xdsclient"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	v3corepb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	v3listenerpb "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	v3routepb "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	v3httppb "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	v3discoverypb "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
)

const (
	defaultTestTimeout      = 5 * time.Second
	defaultTestShortTimeout = 100 * time.Millisecond
)

var (
	filterChainsWithRouteConfigNames = []*v3listenerpb.FilterChain{
		{
			FilterChainMatch: &v3listenerpb.FilterChainMatch{
				PrefixRanges: []*v3corepb.CidrRange{
					{
						AddressPrefix: "192.168.0.0",
						PrefixLen: &wrapperspb.UInt32Value{
							Value: uint32(16),
						},
					},
				},
				SourceType: v3listenerpb.FilterChainMatch_SAME_IP_OR_LOOPBACK,
				SourcePrefixRanges: []*v3corepb.CidrRange{
					{
						AddressPrefix: "192.168.0.0",
						PrefixLen: &wrapperspb.UInt32Value{
							Value: uint32(16),
						},
					},
				},
				SourcePorts: []uint32{80},
			},
			Filters: []*v3listenerpb.Filter{
				{
					Name: "filter-1",
					ConfigType: &v3listenerpb.Filter_TypedConfig{
						TypedConfig: testutils.MarshalAny(&v3httppb.HttpConnectionManager{
							RouteSpecifier: &v3httppb.HttpConnectionManager_Rds{
								Rds: &v3httppb.Rds{
									ConfigSource: &v3corepb.ConfigSource{
										ConfigSourceSpecifier: &v3corepb.ConfigSource_Ads{Ads: &v3corepb.AggregatedConfigSource{}},
									},
									RouteConfigName: "route-1",
								},
							},
							HttpFilters: []*v3httppb.HttpFilter{e2e.RouterHTTPFilter},
						}),
					},
				},
			},
		},
	}
)

type s struct {
	grpctest.Tester
}

func Test(t *testing.T) {
	grpctest.RunSubTests(t, s{})
}

func hostPortFromListener(t *testing.T, lis net.Listener) (string, uint32) {
	host, p, err := net.SplitHostPort(lis.Addr().String())
	if err != nil {
		t.Fatalf("net.SplitHostPort(%s) failed: %v", lis.Addr().String(), err)
	}
	port, err := strconv.ParseInt(p, 10, 32)
	if err != nil {
		t.Fatalf("strconv.ParseInt(%s, 10, 32) failed: %v", p, err)
	}
	return host, uint32(port)
}

func setupAndCreateListenerWraper(t *testing.T, opts e2e.ManagementServerOptions, nodeID string) (*e2e.ManagementServer, net.Listener, <-chan struct{}) {
	mgmtServer, err := e2e.StartManagementServer(opts)
	if err != nil {
		t.Fatalf("Starting xDS management server: %v", err)
	}
	t.Cleanup(func() { mgmtServer.Stop() })

	// Create a bootstrap configuration specifying the above management server,
	// and an xDS client that uses the bootstrap configuration.
	bootstrapContents, err := xdsbootstrap.Contents(xdsbootstrap.Options{
		NodeID:                             nodeID,
		ServerURI:                          mgmtServer.Address,
		ServerListenerResourceNameTemplate: "grpc/server?xds.resource.listening_address=%s",
		Version:                            xdsbootstrap.TransportV3,
	})
	if err != nil {
		t.Fatal(err)
	}
	xdsC, err := xdsclient.NewWithBootstrapContentsForTesting(bootstrapContents)
	if err != nil {
		t.Fatalf("Creating xDS client with bootstrap config %v: %v", string(bootstrapContents), err)
	}
	t.Cleanup(func() { xdsC.Close() })

	// Create a local TCP listener and a ListenerWrapper that uses it.
	lis, err := testutils.LocalTCPListener()
	if err != nil {
		t.Fatalf("Creating a local TCP listener: %v", err)
	}
	lw, readyCh := server.NewListenerWrapper(server.ListenerWrapperParams{
		Listener:             lis,
		ListenerResourceName: fmt.Sprintf("grpc/server?xds.resource.listening_address=%s", lis.Addr().String()),
		XDSClient:            xdsC,
	})
	t.Cleanup(func() { lw.Close() })

	return mgmtServer, lw, readyCh
}

func (s) TestNewListenerWrapper(t *testing.T) {
	// Configure a management server that pushes requested listener resource
	// names on to a channel.
	resourceNamesCh := make(chan []string, 1)
	opts := e2e.ManagementServerOptions{
		OnStreamRequest: func(id int64, req *v3discoverypb.DiscoveryRequest) error {
			if req.GetTypeUrl() != "type.googleapis.com/envoy.config.listener.v3.Listener" {
				return nil
			}

			select {
			case resourceNamesCh <- req.GetResourceNames():
			default:
			}
			return nil
		},
	}
	nodeID := uuid.New().String()
	mgmtServer, lw, readyCh := setupAndCreateListenerWraper(t, opts, nodeID)

	// Verify that the listener wrapper sends an LDS request with the expected
	// resource name.
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	wantNames := []string{fmt.Sprintf("grpc/server?xds.resource.listening_address=%s", lw.Addr().String())}
	select {
	case gotNames := <-resourceNamesCh:
		if !cmp.Equal(gotNames, wantNames) {
			t.Fatalf("Received LDS request for resource names %v, want %v", gotNames, wantNames)
		}
	case <-ctx.Done():
		t.Fatalf("Timeout when waiting for LDS request")
	}

	// Build a good listener resource corresponding to the requested name.
	host, port := hostPortFromListener(t, lw)
	goodListener := e2e.DefaultServerListener(host, port, e2e.SecurityLevelNone)

	// Configure a bad listener resource (`use_original_dst` field is
	// unsupported and is NACKed if set to true) on the management server, and
	// ensure that the ListenerWrapper does not report ready.
	badListener := proto.Clone(goodListener).(*v3listenerpb.Listener)
	badListener.UseOriginalDst = &wrapperspb.BoolValue{Value: true}
	resources := e2e.UpdateOptions{
		NodeID:         nodeID,
		Listeners:      []*v3listenerpb.Listener{badListener},
		SkipValidation: true,
	}
	if err := mgmtServer.Update(ctx, resources); err != nil {
		t.Fatal(err)
	}
	sCtx, sCancel := context.WithTimeout(ctx, defaultTestShortTimeout)
	defer sCancel()
	select {
	case <-readyCh:
		t.Fatalf("ListenerWrapper reports ready when expected not to")
	case <-sCtx.Done():
	}

	// Configure a listener resource on the management server whose address
	// field does not match the address to which our listener is bound to, and
	// ensure that the ListenerWrapper does not report ready.
	notMatchingListener := proto.Clone(goodListener).(*v3listenerpb.Listener)
	notMatchingListener.Address = &v3corepb.Address{
		Address: &v3corepb.Address_SocketAddress{
			SocketAddress: &v3corepb.SocketAddress{
				Address: host,
				PortSpecifier: &v3corepb.SocketAddress_PortValue{
					PortValue: port + 1,
				},
			},
		},
	}
	resources = e2e.UpdateOptions{
		NodeID:         nodeID,
		Listeners:      []*v3listenerpb.Listener{notMatchingListener},
		SkipValidation: true,
	}
	if err := mgmtServer.Update(ctx, resources); err != nil {
		t.Fatal(err)
	}
	sCtx, sCancel = context.WithTimeout(ctx, defaultTestShortTimeout)
	defer sCancel()
	select {
	case <-readyCh:
		t.Fatalf("ListenerWrapper reports ready when expected not to")
	case <-sCtx.Done():
	}

	// Configure a good listener resource on the management server, and ensure
	// that the ListenerWrapper reports ready.
	resources = e2e.UpdateOptions{
		NodeID:         nodeID,
		Listeners:      []*v3listenerpb.Listener{goodListener},
		SkipValidation: true,
	}
	if err := mgmtServer.Update(ctx, resources); err != nil {
		t.Fatal(err)
	}
	select {
	case <-readyCh:
	case <-ctx.Done():
		t.Fatalf("Timeout when waiting for ListenerWrapper to report ready")
	}
}

func (s) TestNewListenerWrapperWithRouteUpdate(t *testing.T) {
	oldRBAC := envconfig.XDSRBAC
	envconfig.XDSRBAC = true
	defer func() {
		envconfig.XDSRBAC = oldRBAC
	}()

	// Configure a management server that pushes requested route configuration
	// resource names on to a channel.
	resourceNamesCh := make(chan []string, 1)
	opts := e2e.ManagementServerOptions{
		OnStreamRequest: func(id int64, req *v3discoverypb.DiscoveryRequest) error {
			if req.GetTypeUrl() != "type.googleapis.com/envoy.config.route.v3.RouteConfiguration" {
				return nil
			}

			select {
			case resourceNamesCh <- req.GetResourceNames():
			default:
			}
			return nil
		},
	}
	nodeID := uuid.New().String()
	mgmtServer, lw, readyCh := setupAndCreateListenerWraper(t, opts, nodeID)

	// Build a good listener resource which contains route configuration
	// resource name, "route-1", to be further resolved using RDS.
	host, port := hostPortFromListener(t, lw)
	goodListener := e2e.DefaultServerListener(host, port, e2e.SecurityLevelNone)
	goodListener.FilterChains = filterChainsWithRouteConfigNames

	// Configure the management server with the listener resource.
	resources := e2e.UpdateOptions{
		NodeID:         nodeID,
		Listeners:      []*v3listenerpb.Listener{goodListener},
		SkipValidation: true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	if err := mgmtServer.Update(ctx, resources); err != nil {
		t.Fatal(err)
	}

	// Verify that the listener wrapper sends an RDS request with the expected
	// resource name.
	wantNames := []string{"route-1"}
	select {
	case gotNames := <-resourceNamesCh:
		if !cmp.Equal(gotNames, wantNames) {
			t.Fatalf("Received RDS request for resource names %v, want %v", gotNames, wantNames)
		}
	case <-ctx.Done():
		t.Fatalf("Timeout when waiting for RDS request")
	}

	// Verify that the listener wrapper is not reporting ready yet.
	sCtx, sCancel := context.WithTimeout(ctx, defaultTestShortTimeout)
	defer sCancel()
	select {
	case <-readyCh:
		t.Fatalf("ListenerWrapper reports ready when expected not to")
	case <-sCtx.Done():
	}

	// Configure the management server with listener and route configuration
	// resources, and expect the listener wrapper to report ready.
	resources = e2e.UpdateOptions{
		NodeID:         nodeID,
		Listeners:      []*v3listenerpb.Listener{goodListener},
		Routes:         []*v3routepb.RouteConfiguration{e2e.DefaultRouteConfig("route-1", "", "")},
		SkipValidation: true,
	}
	if err := mgmtServer.Update(ctx, resources); err != nil {
		t.Fatal(err)
	}
	select {
	case <-readyCh:
	case <-ctx.Done():
		t.Fatalf("Timeout when waiting for ListenerWrapper to report ready")
	}
}
