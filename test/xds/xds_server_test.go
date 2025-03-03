/*
 *
 * Copyright 2023 gRPC authors.
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

package xds_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	xdscreds "google.golang.org/grpc/credentials/xds"
	"google.golang.org/grpc/internal/stubserver"
	"google.golang.org/grpc/internal/testutils"
	"google.golang.org/grpc/internal/testutils/xds/e2e"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/xds"
	"google.golang.org/protobuf/types/known/wrapperspb"

	v3corepb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	v3listenerpb "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	v3routepb "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	v3routerpb "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	v3httppb "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	v3discoverypb "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	testgrpc "google.golang.org/grpc/interop/grpc_testing"
	testpb "google.golang.org/grpc/interop/grpc_testing"
)

var (
	errAcceptAndClose = status.New(codes.Unavailable, "")
	routerHTTPFilter  = e2e.HTTPFilter("router", &v3routerpb.Router{})
)

// Tests the basic scenario for an xDS enabled gRPC server.
//
//   - Verifies that the xDS enabled gRPC server requests for the expected
//     listener resource.
//   - Once the listener resource is received from the management server, it
//     verifies that the xDS enabled gRPC server requests for the appropriate
//     route configuration name. Also verifies that at this point, the server has
//     not yet started serving RPCs
//   - Once the route configuration is received from the management server, it
//     verifies that the server can serve RPCs successfully.
func (s) TestServer_Basic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	// Start an xDS management server.
	listenerNamesCh := make(chan []string, 1)
	routeNamesCh := make(chan []string, 1)
	managementServer := e2e.StartManagementServer(t, e2e.ManagementServerOptions{
		OnStreamRequest: func(_ int64, req *v3discoverypb.DiscoveryRequest) error {
			switch req.GetTypeUrl() {
			case "type.googleapis.com/envoy.config.listener.v3.Listener":
				select {
				case listenerNamesCh <- req.GetResourceNames():
				case <-ctx.Done():
				}
			case "type.googleapis.com/envoy.config.route.v3.RouteConfiguration":
				select {
				case routeNamesCh <- req.GetResourceNames():
				case <-ctx.Done():
				}
			}
			return nil
		},
		AllowResourceSubset: true,
	})

	// Create bootstrap configuration pointing to the above management server.
	nodeID := uuid.New().String()
	bootstrapContents := e2e.DefaultBootstrapContents(t, nodeID, managementServer.Address)

	// Create a listener on a local port to act as the xDS enabled gRPC server.
	lis, err := testutils.LocalTCPListener()
	if err != nil {
		t.Fatalf("Failed to listen to local port: %v", err)
	}
	host, port, err := hostPortFromListener(lis)
	if err != nil {
		t.Fatalf("Failed to retrieve host and port of server: %v", err)
	}

	// Configure the managegement server with a listener resource for the above
	// xDS enabled gRPC server.
	const routeConfigName = "routeName"
	resources := e2e.UpdateOptions{
		NodeID:         nodeID,
		Listeners:      []*v3listenerpb.Listener{e2e.DefaultServerListenerWithRouteConfigName(host, port, e2e.SecurityLevelNone, "routeName")},
		SkipValidation: true,
	}
	if err := managementServer.Update(ctx, resources); err != nil {
		t.Fatal(err)
	}

	// Create the xDS enabled gRPC server with the following:
	// - xDS credentials with an insecure fallback
	// - a callback that notifies the test when the server goes SERVING
	creds, err := xdscreds.NewServerCredentials(xdscreds.ServerOptions{FallbackCreds: insecure.NewCredentials()})
	if err != nil {
		t.Fatalf("Failed to create server credentials: %v", err)
	}
	servingCh := make(chan struct{})
	modeChangeOpt := xds.ServingModeCallback(func(addr net.Addr, args xds.ServingModeChangeArgs) {
		t.Logf("Serving mode for listener %q changed to %q, err: %v", addr.String(), args.Mode, args.Err)
		if args.Mode == connectivity.ServingModeServing {
			close(servingCh)
		}
	})
	stub := createStubServer(t, lis, creds, modeChangeOpt, bootstrapContents)
	defer stub.Stop()

	// Wait for the expected listener resource to be requested.
	wantLisResourceNames := []string{fmt.Sprintf(e2e.ServerListenerResourceNameTemplate, net.JoinHostPort(host, strconv.Itoa(int(port))))}
	select {
	case <-ctx.Done():
		t.Fatal("Timeout waiting for the expected listener resource to be requested")
	case gotLisResourceName := <-listenerNamesCh:
		if !cmp.Equal(gotLisResourceName, wantLisResourceNames) {
			t.Fatalf("Got unexpected listener resource names: %v, want %v", gotLisResourceName, wantLisResourceNames)
		}
	}

	// Wait for the expected route config resource to be requested.
	select {
	case <-ctx.Done():
		t.Fatal("Timeout waiting for the expected route config resource to be requested")
	case gotRouteNames := <-routeNamesCh:
		if !cmp.Equal(gotRouteNames, []string{routeConfigName}) {
			t.Fatalf("Got unexpected route config resource names: %v, want %v", gotRouteNames, []string{routeConfigName})
		}
	}

	// Ensure that the server is not serving RPCs yet.
	sCtx, sCancel := context.WithTimeout(ctx, defaultTestShortTimeout)
	defer sCancel()
	select {
	case <-sCtx.Done():
	case <-servingCh:
		t.Fatal("Server started serving RPCs before the route config was received")
	}

	// Create a gRPC channel to the xDS enabled server.
	cc, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient(%q) failed: %v", lis.Addr(), err)
	}
	defer cc.Close()

	// Ensure that the server isnt't serving RPCs successfully.
	client := testgrpc.NewTestServiceClient(cc)
	if _, err := client.EmptyCall(ctx, &testpb.Empty{}); err == nil || status.Code(err) != codes.Unavailable {
		t.Fatalf("EmptyCall() returned %v, want %v", err, codes.Unavailable)
	}

	// Configure the management server with the expected route config resource,
	// and expext RPCs to succeed.
	resources.Routes = []*v3routepb.RouteConfiguration{e2e.RouteConfigNonForwardingAction(routeConfigName)}
	if err := managementServer.Update(ctx, resources); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
		t.Fatal("Timeout waiting for the server to start serving RPCs")
	case <-servingCh:
	}
	waitForSuccessfulRPC(ctx, t, cc)
}

// Tests the case where the route configuration contains an unsupported route
// action.  Verifies that RPCs fail with UNAVAILABLE.
func (s) TestServer_FailWithRouteActionRoute(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	// Start an xDS management server.
	managementServer := e2e.StartManagementServer(t, e2e.ManagementServerOptions{})

	// Create bootstrap configuration pointing to the above management server.
	nodeID := uuid.New().String()
	bootstrapContents := e2e.DefaultBootstrapContents(t, nodeID, managementServer.Address)

	// Create a listener on a local port to act as the xDS enabled gRPC server.
	lis, err := testutils.LocalTCPListener()
	if err != nil {
		t.Fatalf("Failed to listen to local port: %v", err)
	}
	host, port, err := hostPortFromListener(lis)
	if err != nil {
		t.Fatalf("Failed to retrieve host and port of server: %v", err)
	}

	// Configure the managegement server with a listener and route configuration
	// resource for the above xDS enabled gRPC server.
	const routeConfigName = "routeName"
	resources := e2e.UpdateOptions{
		NodeID:    nodeID,
		Listeners: []*v3listenerpb.Listener{e2e.DefaultServerListenerWithRouteConfigName(host, port, e2e.SecurityLevelNone, "routeName")},
		Routes:    []*v3routepb.RouteConfiguration{e2e.RouteConfigNonForwardingAction(routeConfigName)},
	}
	if err := managementServer.Update(ctx, resources); err != nil {
		t.Fatal(err)
	}

	// Create the xDS enabled gRPC server with the following:
	// - xDS credentials with an insecure fallback
	// - a callback that logs serving mode changes
	creds, err := xdscreds.NewServerCredentials(xdscreds.ServerOptions{FallbackCreds: insecure.NewCredentials()})
	if err != nil {
		t.Fatalf("Failed to create server credentials: %v", err)
	}
	modeChangeOpt := xds.ServingModeCallback(func(addr net.Addr, args xds.ServingModeChangeArgs) {
		t.Logf("Serving mode for listener %q changed to %q, err: %v", addr.String(), args.Mode, args.Err)
	})
	stub := createStubServer(t, lis, creds, modeChangeOpt, bootstrapContents)
	defer stub.Stop()

	// Create a gRPC channel and verify that RPCs succeed.
	cc, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient(%q) failed: %v", lis.Addr(), err)
	}
	defer cc.Close()
	waitForSuccessfulRPC(ctx, t, cc)

	// Update the route config resource to contain an unsupported action.
	//
	// "NonForwardingAction is expected for all Routes used on server-side; a
	// route with an inappropriate action causes RPCs matching that route to
	// fail with UNAVAILABLE." - A36
	resources.Routes = []*v3routepb.RouteConfiguration{e2e.RouteConfigFilterAction("routeName")}
	if err := managementServer.Update(ctx, resources); err != nil {
		t.Fatal(err)
	}
	waitForFailedRPCWithStatus(ctx, t, cc, status.New(codes.Unavailable, "the incoming RPC matched to a route that was not of action type non forwarding"))
}

// tests the case where the listener resource is removed from the management
// server. This should cause the xDS server to transition to NOT_SERVING mode,
// and the error message should contain the xDS node ID.
func (s) TestServer_ListenerResourceRemoved(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	// Start an xDS management server.
	managementServer := e2e.StartManagementServer(t, e2e.ManagementServerOptions{})

	// Create bootstrap configuration pointing to the above management server.
	nodeID := uuid.New().String()
	bootstrapContents := e2e.DefaultBootstrapContents(t, nodeID, managementServer.Address)

	// Create a listener on a local port to act as the xDS enabled gRPC server.
	lis, err := testutils.LocalTCPListener()
	if err != nil {
		t.Fatalf("Failed to listen to local port: %v", err)
	}
	host, port, err := hostPortFromListener(lis)
	if err != nil {
		t.Fatalf("Failed to retrieve host and port of server: %v", err)
	}

	// Configure the managegement server with a listener and route configuration
	// resource for the above xDS enabled gRPC server.
	const routeConfigName = "routeName"
	resources := e2e.UpdateOptions{
		NodeID:         nodeID,
		Listeners:      []*v3listenerpb.Listener{e2e.DefaultServerListenerWithRouteConfigName(host, port, e2e.SecurityLevelNone, "routeName")},
		Routes:         []*v3routepb.RouteConfiguration{e2e.RouteConfigNonForwardingAction(routeConfigName)},
		SkipValidation: true,
	}
	if err := managementServer.Update(ctx, resources); err != nil {
		t.Fatal(err)
	}

	// Create the xDS enabled gRPC server with the following:
	// - xDS credentials with an insecure fallback
	// - a callback that notifies the test when the server goes NOT_SERVING
	servingCh := make(chan error, 1)
	creds, err := xdscreds.NewServerCredentials(xdscreds.ServerOptions{FallbackCreds: insecure.NewCredentials()})
	if err != nil {
		t.Fatalf("Failed to create server credentials: %v", err)
	}
	modeChangeOpt := xds.ServingModeCallback(func(addr net.Addr, args xds.ServingModeChangeArgs) {
		t.Logf("Serving mode for listener %q changed to %q, err: %v", addr.String(), args.Mode, args.Err)
		if args.Mode == connectivity.ServingModeNotServing {
			servingCh <- args.Err
		}
	})
	stub := createStubServer(t, lis, creds, modeChangeOpt, bootstrapContents)
	defer stub.Stop()

	// Create a gRPC channel and verify that RPCs succeed.
	cc, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient(%q) failed: %v", lis.Addr(), err)
	}
	defer cc.Close()
	waitForSuccessfulRPC(ctx, t, cc)

	// Remove the listener resource from the management server. This should
	// cause the server to go NOT_SERVING, and the error message should contain
	// the xDS node ID.
	resources.Listeners = nil
	if err := managementServer.Update(ctx, resources); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-servingCh:
		if err == nil || !strings.Contains(err.Error(), nodeID) {
			t.Fatalf("When moving to NOT_SERVING, got error message: %v, want xDS node ID: %q", err, nodeID)
		}
	case <-ctx.Done():
		t.Fatalf("Timed out waiting for server to go NOT_SERVING")
	}
}

// Tests the case where the listener resource points to a route configuration
// name that is NACKed. This should trigger the server to move to SERVING,
// successfully accept connections, and fail at RPCs with an expected error
// message.
func (s) TestServer_RDS_ResourceNACK(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	// Start an xDS management server.
	managementServer := e2e.StartManagementServer(t, e2e.ManagementServerOptions{})

	// Create bootstrap configuration pointing to the above management server.
	nodeID := uuid.New().String()
	bootstrapContents := e2e.DefaultBootstrapContents(t, nodeID, managementServer.Address)

	// Create a listener on a local port to act as the xDS enabled gRPC server.
	lis, err := testutils.LocalTCPListener()
	if err != nil {
		t.Fatalf("Failed to listen to local port: %v", err)
	}
	host, port, err := hostPortFromListener(lis)
	if err != nil {
		t.Fatalf("Failed to retrieve host and port of server: %v", err)
	}

	// Configure the managegement server with a listener and route configuration
	// resource (that will be NACKed), for the above xDS enabled gRPC server.
	const routeConfigName = "routeName"
	resources := e2e.UpdateOptions{
		NodeID:         nodeID,
		Listeners:      []*v3listenerpb.Listener{e2e.DefaultServerListenerWithRouteConfigName(host, port, e2e.SecurityLevelNone, "routeName")},
		Routes:         []*v3routepb.RouteConfiguration{e2e.RouteConfigNoRouteMatch(routeConfigName)},
		SkipValidation: true,
	}
	if err := managementServer.Update(ctx, resources); err != nil {
		t.Fatal(err)
	}

	// Create the xDS enabled gRPC server with the following:
	// - xDS credentials with an insecure fallback
	// - a callback that notifies the test when the server goes SERVING
	creds, err := xdscreds.NewServerCredentials(xdscreds.ServerOptions{FallbackCreds: insecure.NewCredentials()})
	if err != nil {
		t.Fatalf("Failed to create server credentials: %v", err)
	}
	servingCh := make(chan struct{})
	modeChangeOpt := xds.ServingModeCallback(func(addr net.Addr, args xds.ServingModeChangeArgs) {
		t.Logf("Serving mode for listener %q changed to %q, err: %v", addr.String(), args.Mode, args.Err)
		if args.Mode == connectivity.ServingModeServing {
			close(servingCh)
		}
	})
	stub := createStubServer(t, lis, creds, modeChangeOpt, bootstrapContents)
	defer stub.Stop()

	// Create a gRPC channel and verify that RPCs succeed.
	cc, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient(%q) failed: %v", lis.Addr(), err)
	}
	defer cc.Close()

	select {
	case <-ctx.Done():
		t.Fatal("Timeout waiting for the server to start serving RPCs")
	case <-servingCh:
	}
	waitForFailedRPCWithStatus(ctx, t, cc, status.New(codes.Unavailable, "error from xDS configuration for matched route configuration"))
}

// Tests the case where the listener resource points to multiple route
// configuration resources.
//
//   - Initially the listener resource points to three route configuration
//     resources (A, B and C). The filter chain in the listener matches incoming
//     connections to route A, and RPCs are expected to succeed.
//   - A streaming RPC is also kept open at this point.
//   - The listener resource is then updated to point to two route configuration
//     resources (A and B). The filter chain in the listener resource does not
//     match to any of the configured routes. The default filter chain though
//     matches to route B, which contains a route action of type "Route", and this
//     is not supported on the server side. New RPCs are expected to fail, while
//     any ongoing RPCs should be allowed to complete.
//   - The listener resource is then updated to point to a single route
//     configuration (A), and the filter chain in the listener matches to route A.
//     New RPCs are expected to succeed at this point.
func (s) TestServer_MultipleRouteConfigurations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	// Start an xDS management server.
	managementServer := e2e.StartManagementServer(t, e2e.ManagementServerOptions{})

	// Create bootstrap configuration pointing to the above management server.
	nodeID := uuid.New().String()
	bootstrapContents := e2e.DefaultBootstrapContents(t, nodeID, managementServer.Address)

	// Create a listener on a local port to act as the xDS enabled gRPC server.
	lis, err := testutils.LocalTCPListener()
	if err != nil {
		t.Fatalf("Failed to listen to local port: %v", err)
	}
	host, port, err := hostPortFromListener(lis)
	if err != nil {
		t.Fatalf("Failed to retrieve host and port of server: %v", err)
	}

	// Setup the management server to respond with a listener resource that
	// specifies three route names to watch, and the corresponding route
	// configuration resources.
	const routeConfigNameA = "routeName-A"
	const routeConfigNameB = "routeName-B"
	const routeConfigNameC = "routeName-C"
	ldsResource := e2e.DefaultServerListenerWithRouteConfigName(host, port, e2e.SecurityLevelNone, routeConfigNameA)
	ldsResource.FilterChains = append(ldsResource.FilterChains,
		filterChainWontMatch(t, routeConfigNameB, "1.1.1.1", []uint32{1}),
		filterChainWontMatch(t, routeConfigNameC, "2.2.2.2", []uint32{2}),
	)
	routeConfigA := e2e.RouteConfigNonForwardingAction(routeConfigNameA)
	routeConfigB := e2e.RouteConfigFilterAction(routeConfigNameB) // Unsupported route action on server.
	routeConfigC := e2e.RouteConfigFilterAction(routeConfigNameC) // Unsupported route action on server.
	resources := e2e.UpdateOptions{
		NodeID:         nodeID,
		Listeners:      []*v3listenerpb.Listener{ldsResource},
		Routes:         []*v3routepb.RouteConfiguration{routeConfigA, routeConfigB, routeConfigC},
		SkipValidation: true,
	}
	if err := managementServer.Update(ctx, resources); err != nil {
		t.Fatal(err)
	}

	// Create the xDS enabled gRPC server with the following:
	// - xDS credentials with an insecure fallback
	// - a callback that logs serving mode changes
	creds, err := xdscreds.NewServerCredentials(xdscreds.ServerOptions{FallbackCreds: insecure.NewCredentials()})
	if err != nil {
		t.Fatalf("Failed to create server credentials: %v", err)
	}
	modeChangeOpt := xds.ServingModeCallback(func(addr net.Addr, args xds.ServingModeChangeArgs) {
		t.Logf("Serving mode for listener %q changed to %q, err: %v", addr.String(), args.Mode, args.Err)
	})
	stub := &stubserver.StubServer{
		Listener: lis,
		EmptyCallF: func(ctx context.Context, in *testpb.Empty) (*testpb.Empty, error) {
			return &testpb.Empty{}, nil
		},
		FullDuplexCallF: func(stream testgrpc.TestService_FullDuplexCallServer) error {
			for {
				_, err := stream.Recv()
				if err == io.EOF {
					return nil
				}
			}
		},
	}
	server, err := xds.NewGRPCServer(grpc.Creds(creds), modeChangeOpt, xds.BootstrapContentsForTesting(bootstrapContents))
	if err != nil {
		t.Fatalf("Failed to create an xDS enabled gRPC server: %v", err)
	}
	stub.S = server
	stubserver.StartTestService(t, stub)
	defer stub.Stop()

	// Create a gRPC channel and verify that RPCs succeed.
	cc, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient(%q) failed: %v", lis.Addr(), err)
	}
	defer cc.Close()
	waitForSuccessfulRPC(ctx, t, cc)

	// Start a streaming RPC and keep the stream open.
	client := testgrpc.NewTestServiceClient(cc)
	stream, err := client.FullDuplexCall(ctx)
	if err != nil {
		t.Fatalf("FullDuplexCall failed: %v", err)
	}
	if err = stream.Send(&testpb.StreamingOutputCallRequest{}); err != nil {
		t.Fatalf("stream.Send() failed: %v, should continue to work due to graceful stop", err)
	}

	// Update the listener resource such that the filter chain does not match
	// incoming connections to route A. Instead a default filter chain matches
	// to route B, which contains an unsupported route action.
	ldsResource = e2e.DefaultServerListenerWithRouteConfigName(host, port, e2e.SecurityLevelNone, routeConfigNameA)
	ldsResource.FilterChains = []*v3listenerpb.FilterChain{filterChainWontMatch(t, routeConfigNameA, "1.1.1.1", []uint32{1})}
	ldsResource.DefaultFilterChain = filterChainWontMatch(t, routeConfigNameB, "2.2.2.2", []uint32{2})
	resources.Listeners = []*v3listenerpb.Listener{ldsResource}
	if err := managementServer.Update(ctx, resources); err != nil {
		t.Fatal(err)
	}

	// xDS is eventually consistent. So simply poll for the new change to be
	// reflected.
	// "NonForwardingAction is expected for all Routes used on server-side; a
	// route with an inappropriate action causes RPCs matching that route to
	// fail with UNAVAILABLE." - A36
	waitForFailedRPCWithStatus(ctx, t, cc, status.New(codes.Unavailable, "the incoming RPC matched to a route that was not of action type non forwarding"))

	// Stream should be allowed to continue on the old working configuration -
	// as it on a connection that is gracefully closed (old FCM/LDS
	// Configuration which is allowed to continue).
	if err = stream.CloseSend(); err != nil {
		t.Fatalf("stream.CloseSend() failed: %v, should continue to work due to graceful stop", err)
	}
	if _, err = stream.Recv(); err != io.EOF {
		t.Fatalf("unexpected error: %v, expected an EOF error", err)
	}

	// Update the listener resource to point to a single route configuration
	// that is expected to match and verify that RPCs succeed.
	resources.Listeners = []*v3listenerpb.Listener{e2e.DefaultServerListener(host, port, e2e.SecurityLevelNone, routeConfigNameA)}
	if err := managementServer.Update(ctx, resources); err != nil {
		t.Fatal(err)
	}
	pollForSuccessfulRPC(ctx, t, cc)
}

func pollForSuccessfulRPC(ctx context.Context, t *testing.T, cc *grpc.ClientConn) {
	t.Helper()
	c := testgrpc.NewTestServiceClient(cc)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("timeout waiting for RPCs to succeed")
		case <-ticker.C:
			if _, err := c.EmptyCall(ctx, &testpb.Empty{}); err == nil {
				return
			}
		}
	}
}

// waitForFailedRPCWithStatus makes unary RPC's until it receives the expected
// status in a polling manner. Fails if the RPC made does not return the
// expected status before the context expires.
func waitForFailedRPCWithStatus(ctx context.Context, t *testing.T, cc *grpc.ClientConn, st *status.Status) {
	t.Helper()

	c := testgrpc.NewTestServiceClient(cc)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var err error
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("failure when waiting for RPCs to fail with certain status %v: %v. most recent error received from RPC: %v", st, ctx.Err(), err)
		case <-ticker.C:
			_, err = c.EmptyCall(ctx, &testpb.Empty{})
			if status.Code(err) == st.Code() && strings.Contains(err.Error(), st.Message()) {
				t.Logf("most recent error happy case: %v", err.Error())
				return
			}
		}
	}
}

// filterChainWontMatch returns a filter chain that won't match if running the
// test locally.
func filterChainWontMatch(t *testing.T, routeName string, addressPrefix string, srcPorts []uint32) *v3listenerpb.FilterChain {
	hcm := &v3httppb.HttpConnectionManager{
		RouteSpecifier: &v3httppb.HttpConnectionManager_Rds{
			Rds: &v3httppb.Rds{
				ConfigSource: &v3corepb.ConfigSource{
					ConfigSourceSpecifier: &v3corepb.ConfigSource_Ads{Ads: &v3corepb.AggregatedConfigSource{}},
				},
				RouteConfigName: routeName,
			},
		},
		HttpFilters: []*v3httppb.HttpFilter{routerHTTPFilter},
	}
	return &v3listenerpb.FilterChain{
		Name: routeName + "-wont-match",
		FilterChainMatch: &v3listenerpb.FilterChainMatch{
			PrefixRanges: []*v3corepb.CidrRange{
				{
					AddressPrefix: addressPrefix,
					PrefixLen: &wrapperspb.UInt32Value{
						Value: uint32(0),
					},
				},
			},
			SourceType:  v3listenerpb.FilterChainMatch_SAME_IP_OR_LOOPBACK,
			SourcePorts: srcPorts,
			SourcePrefixRanges: []*v3corepb.CidrRange{
				{
					AddressPrefix: addressPrefix,
					PrefixLen: &wrapperspb.UInt32Value{
						Value: uint32(0),
					},
				},
			},
		},
		Filters: []*v3listenerpb.Filter{
			{
				Name:       "filter-1",
				ConfigType: &v3listenerpb.Filter_TypedConfig{TypedConfig: testutils.MarshalAny(t, hcm)},
			},
		},
	}
}

func (s) TestServer_ConnectionCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	// Start an xDS management server.
	managementServer := e2e.StartManagementServer(t, e2e.ManagementServerOptions{})

	// Create bootstrap configuration pointing to the above management server.
	nodeID := uuid.New().String()
	bootstrapContents := e2e.DefaultBootstrapContents(t, nodeID, managementServer.Address)

	// Create a listener on a local port to act as the xDS enabled gRPC server.
	lis, err := testutils.LocalTCPListener()
	if err != nil {
		t.Fatalf("Failed to listen to local port: %v", err)
	}
	host, port, err := hostPortFromListener(lis)
	if err != nil {
		t.Fatalf("Failed to retrieve host and port of server: %v", err)
	}

	// Configure the managegement server with a listener and route configuration
	// resource for the above xDS enabled gRPC server.
	const routeConfigName = "routeName"
	resources := e2e.UpdateOptions{
		NodeID:         nodeID,
		Listeners:      []*v3listenerpb.Listener{e2e.DefaultServerListenerWithRouteConfigName(host, port, e2e.SecurityLevelNone, "routeName")},
		Routes:         []*v3routepb.RouteConfiguration{e2e.RouteConfigNonForwardingAction(routeConfigName)},
		SkipValidation: true,
	}
	if err := managementServer.Update(ctx, resources); err != nil {
		t.Fatal(err)
	}

	// Create the xDS enabled gRPC server with the following:
	// - xDS credentials with an insecure fallback
	// - a callback that logs serving mode changes
	creds, err := xdscreds.NewServerCredentials(xdscreds.ServerOptions{FallbackCreds: insecure.NewCredentials()})
	if err != nil {
		t.Fatalf("Failed to create server credentials: %v", err)
	}
	modeChangeOpt := xds.ServingModeCallback(func(addr net.Addr, args xds.ServingModeChangeArgs) {
		t.Logf("Serving mode for listener %q changed to %q, err: %v", addr.String(), args.Mode, args.Err)
	})
	stub := createStubServer(t, lis, creds, modeChangeOpt, bootstrapContents)
	defer stub.Stop()

	// Create a gRPC channel and verify that RPCs succeed.
	cc, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient(%q) failed: %v", lis.Addr(), err)
	}
	defer cc.Close()
	waitForSuccessfulRPC(ctx, t, cc)

	// Create multiple channels to the server, and make an RPC on each one. When
	// everything is closed, the server should have cleaned up all its resources
	// as well (and this will be verified by the leakchecker).
	const numConns = 100
	var wg sync.WaitGroup
	wg.Add(numConns)
	for range numConns {
		go func() {
			defer wg.Done()
			cc, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Errorf("grpc.NewClient failed with err: %v", err)
			}
			client := testgrpc.NewTestServiceClient(cc)
			if _, err := client.EmptyCall(ctx, &testpb.Empty{}); err != nil {
				t.Errorf("EmptyCall() failed: %v", err)
			}
			cc.Close()
		}()
	}
	wg.Wait()
}
