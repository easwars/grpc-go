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

package xds_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v3clusterpb "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	v3endpointpb "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	v3listenerpb "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	v3routepb "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/internal/grpctest"
	"google.golang.org/grpc/internal/stubserver"
	"google.golang.org/grpc/internal/testutils"
	"google.golang.org/grpc/internal/testutils/xds/e2e"
	"google.golang.org/grpc/internal/testutils/xds/e2e/setup"
	"google.golang.org/grpc/internal/xds/bootstrap"

	testgrpc "google.golang.org/grpc/interop/grpc_testing"
	testpb "google.golang.org/grpc/interop/grpc_testing"
)

type s struct {
	grpctest.Tester
}

func Test(t *testing.T) {
	grpctest.RunSubTests(t, s{})
}

const (
	defaultTestTimeout      = 10 * time.Second
	defaultTestShortTimeout = 10 * time.Millisecond // For events expected to *not* happen.
)

func (s) TestClientSideXDS(t *testing.T) {
	managementServer, nodeID, _, xdsResolver := setup.ManagementServerAndResolver(t)

	server := stubserver.StartTestService(t, nil)
	defer server.Stop()

	const serviceName = "my-service-client-side-xds"
	resources := e2e.DefaultClientResources(e2e.ResourceParams{
		DialTarget: serviceName,
		NodeID:     nodeID,
		Host:       "localhost",
		Port:       testutils.ParsePort(t, server.Address),
		SecLevel:   e2e.SecurityLevelNone,
	})
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	if err := managementServer.Update(ctx, resources); err != nil {
		t.Fatal(err)
	}

	// Create a ClientConn and make a successful RPC.
	cc, err := grpc.NewClient(fmt.Sprintf("xds:///%s", serviceName), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithResolvers(xdsResolver))
	if err != nil {
		t.Fatalf("failed to dial local test server: %v", err)
	}
	defer cc.Close()

	client := testgrpc.NewTestServiceClient(cc)
	if _, err := client.EmptyCall(ctx, &testpb.Empty{}, grpc.WaitForReady(true)); err != nil {
		t.Fatalf("rpc EmptyCall() failed: %v", err)
	}
}

// Test a scenario where a gRPC client utilizes xDS to communicate with
// its backend services.  However, there's a twist: the management server
// responsible for providing the client with the necessary configuration to
// locate these backend services is itself discovered using xDS.
func (s) TestClientNew_RaceWithClose(t *testing.T) {
	// Create two management servers. The first one provides configuration
	// required to connect to service backends, while the second one provides
	// configuration required to connect to the first.
	mgmtServerForBackends := e2e.StartManagementServer(t, e2e.ManagementServerOptions{})
	mgmtServerForManagement := e2e.StartManagementServer(t, e2e.ManagementServerOptions{})

	// Create a bootstrap configuration with a top-level server config pointing
	// to the first management server created above, i.e. used to discover
	// configuration required to talk to service backends. The server URI for
	// this server config also has an xds:/// scheme.
	//
	// In the authorities map, we have a server config for the management server
	// used to discover the top-level management server.
	const mgmtServerAuthority = "management-server"
	const mgmtServiceName = "my-management-service"
	nodeID := uuid.New().String()
	bootstrapContents, err := bootstrap.NewContentsForTesting(bootstrap.ConfigOptionsForTesting{
		Servers: []byte(fmt.Sprintf(`
			[{
				"server_uri": %q,
				"channel_creds": [{"type": "insecure"}]
			}]`, fmt.Sprintf("xds://%s/%s", mgmtServerAuthority, mgmtServiceName))),
		Node: []byte(fmt.Sprintf(`{"id": "%s"}`, nodeID)),
		Authorities: map[string]json.RawMessage{
			mgmtServerAuthority: []byte(fmt.Sprintf(`
			{
				"xds_servers":
				[{
					"server_uri": %q,
					"channel_creds": [{"type": "insecure"}]
				}]
			}`, mgmtServerForManagement.Address)),
		},
	})
	if err != nil {
		t.Fatalf("Failed to create bootstrap configuration: %v", err)
	}
	testutils.CreateBootstrapFileForTesting(t, bootstrapContents)

	// Create the test service backend.
	server := stubserver.StartTestService(t, nil)
	defer server.Stop()

	// Create configuration for the test service backends.
	const serviceName = "my-service-client-side-xds"
	resources := e2e.DefaultClientResources(e2e.ResourceParams{
		DialTarget: serviceName,
		NodeID:     nodeID,
		Host:       "localhost",
		Port:       testutils.ParsePort(t, server.Address),
		SecLevel:   e2e.SecurityLevelNone,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()
	if err := mgmtServerForBackends.Update(ctx, resources); err != nil {
		t.Fatal(err)
	}

	// Create configuration returned by the second management server. This is
	// used when resolving the first management server.
	dialTarget := fmt.Sprintf("xdstp://%s/envoy.config.listener.v3.Listener/%s", mgmtServerAuthority, mgmtServiceName)
	routeConfigName := "route-" + dialTarget
	clusterName := "cluster-" + dialTarget
	endpointsName := "endpoints-" + dialTarget
	resources = e2e.UpdateOptions{
		NodeID:    nodeID,
		Listeners: []*v3listenerpb.Listener{e2e.DefaultClientListener(dialTarget, routeConfigName)},
		Routes:    []*v3routepb.RouteConfiguration{e2e.DefaultRouteConfig(routeConfigName, mgmtServiceName, clusterName)},
		Clusters:  []*v3clusterpb.Cluster{e2e.DefaultCluster(clusterName, endpointsName, e2e.SecurityLevelNone)},
		Endpoints: []*v3endpointpb.ClusterLoadAssignment{e2e.DefaultEndpoint(endpointsName, "localhost", []uint32{testutils.ParsePort(t, mgmtServerForBackends.Address)})},
	}
	if err := mgmtServerForManagement.Update(ctx, resources); err != nil {
		t.Fatal(err)
	}

	// To exercise any possible race between creating and closing xDS clients
	// (as part of creating and closing gRPC channels), we spawn a bunch of
	// goroutines here, each of which simply creates a gRPC channel, makes an
	// RPC and closes the channel.
	var wg sync.WaitGroup
	const numGoroutines = 5
	wg.Add(numGoroutines)
	var failed atomic.Bool
	for i := 0; i < numGoroutines; i++ {
		go func(idx int) {
			t.Logf("Starting goroutine %d to perform channel creation and close", idx)
			defer wg.Done()
			const numChannels = 3
			for j := 0; j < numChannels; j++ {
				t.Logf("Goroutine %d: Creating channel %d", idx, j)
				if err := createChannelAndMakeRPC(ctx, serviceName); err != nil {
					t.Logf("Goroutine %d, channel %d: %v", idx, j, err)
					failed.Store(true)
					return
				}
				//<-time.After(defaultTestShortTimeout)
			}
		}(i)
	}
	wg.Wait()
	if failed.Load() == true {
		t.Fatalf("Test failed")
	}
}

func createChannelAndMakeRPC(ctx context.Context, serviceName string) error {
	cc, err := grpc.NewClient(fmt.Sprintf("xds:///%s", serviceName), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("failed to dial local test server: %v", err)
	}
	defer cc.Close()

	client := testgrpc.NewTestServiceClient(cc)
	if _, err := client.EmptyCall(ctx, &testpb.Empty{}, grpc.WaitForReady(true)); err != nil {
		return fmt.Errorf("EmptyCall() failed: %v", err)
	}
	return nil
}
