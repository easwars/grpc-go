/*
 *
 * Copyright 2024 gRPC authors.
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

package grpclb_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	lbpb "google.golang.org/grpc/balancer/grpclb/grpc_lb_v1"
	grpclbstate "google.golang.org/grpc/balancer/grpclb/state"
	"google.golang.org/grpc/internal/grpctest"
	"google.golang.org/grpc/internal/stubserver"
	"google.golang.org/grpc/internal/testutils/pickfirst"
	"google.golang.org/grpc/internal/testutils/roundrobin"
	testgrpc "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"
)

const (
	defaultTestTimeout      = 10 * time.Second
	defaultTestShortTimeout = 10 * time.Millisecond
	testUserAgent           = "test-user-agent"
	grpclbConfig            = `{"loadBalancingConfig": [{"grpclb": {}}]}`
)

type s struct {
	grpctest.Tester
}

func Test(t *testing.T) {
	grpctest.RunSubTests(t, s{})
}

func startBackendsAndRemoteLoadBalancer(t *testing.T, numberOfBackends int, customUserAgent string, statsChan chan *lbpb.ClientStats) (tss *testServers, cleanup func(), err error) {
	var (
		beListeners []net.Listener
		ls          *remoteBalancer
		lb          *grpc.Server
		beIPs       []net.IP
		bePorts     []int
	)

	lbLis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		err = fmt.Errorf("failed to create the listener for the load balancer %v", err)
		return
	}
	lbLis = testutils.NewRestartableListener(lbLis)
	lbCreds := &serverNameCheckCreds{
		sn: lbServerName,
	}
	lb = grpc.NewServer(grpc.Creds(lbCreds))
	ls = newRemoteBalancer(customUserAgent, beServerName, statsChan)
	lbgrpc.RegisterLoadBalancerServer(lb, ls)
	go func() {
		lb.Serve(lbLis)
	}()
	t.Logf("Started remote load balancer server listening on %s", lbLis.Addr().String())

	tss = &testServers{
		lbAddr:   net.JoinHostPort(fakeName, strconv.Itoa(lbLis.Addr().(*net.TCPAddr).Port)),
		ls:       ls,
		lb:       lb,
		backends: backends,
		beIPs:    beIPs,
		bePorts:  bePorts,

		lbListener:  lbLis,
		beListeners: beListeners,
	}
	cleanup = func() {
		defer stopBackends(backends)
		defer func() {
			ls.stop()
			lb.Stop()
		}()
	}
	return
}

// startTestServiceBackends starts num stub servers. It returns their addresses.
// Servers are closed when the test is stopped.
func startTestServiceBackends(t *testing.T, num int) []string {
	t.Helper()

	addrs := make([]string, 0, num)
	for i := 0; i < num; i++ {
		server := stubserver.StartTestService(t, nil)
		t.Cleanup(server.Stop)
		addrs = append(addrs, server.Address)
	}
	return addrs
}

// Test configures grpclb with pick_first as the child policy. The test changes
// the list of backend addresses returned by the remote balancer and verifies
// that RPCs are sent to the first address returned.
func (s) TestGRPCLB_PickFirst(t *testing.T) {
	const numBackends = 3
	tss, cleanup, err := startBackendsAndRemoteLoadBalancer(t, 3, "", nil)
	if err != nil {
		t.Fatalf("failed to create new load balancer: %v", err)
	}
	defer cleanup()

	beServers := []*lbpb.Server{{
		IpAddress:        tss.beIPs[0],
		Port:             int32(tss.bePorts[0]),
		LoadBalanceToken: lbToken,
	}, {
		IpAddress:        tss.beIPs[1],
		Port:             int32(tss.bePorts[1]),
		LoadBalanceToken: lbToken,
	}, {
		IpAddress:        tss.beIPs[2],
		Port:             int32(tss.bePorts[2]),
		LoadBalanceToken: lbToken,
	}}
	beServerAddrs := []resolver.Address{}
	for _, lis := range tss.beListeners {
		beServerAddrs = append(beServerAddrs, resolver.Address{Addr: lis.Addr().String()})
	}

	// Connect to the test backends.
	r := manual.NewBuilderWithScheme("whatever")
	dopts := []grpc.DialOption{
		grpc.WithResolvers(r),
		grpc.WithTransportCredentials(&serverNameCheckCreds{}),
		grpc.WithContextDialer(fakeNameDialer),
	}
	cc, err := grpc.Dial(r.Scheme()+":///"+beServerName, dopts...)
	if err != nil {
		t.Fatalf("Failed to dial to the backend %v", err)
	}
	defer cc.Close()

	// Push a service config with grpclb as the load balancing policy and
	// configure pick_first as its child policy.
	rs := resolver.State{ServiceConfig: r.CC.ParseServiceConfig(`{"loadBalancingConfig":[{"grpclb":{"childPolicy":[{"pick_first":{}}]}}]}`)}

	// Push a resolver update with the remote balancer address specified via
	// attributes.
	r.UpdateState(grpclbstate.Set(rs, &grpclbstate.State{BalancerAddresses: []resolver.Address{{Addr: tss.lbAddr, ServerName: lbServerName}}}))

	// Push all three backend addresses to the remote balancer, and verify that
	// RPCs are routed to the first backend.
	tss.ls.sls <- &lbpb.ServerList{Servers: beServers[0:3]}
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	if err := pickfirst.CheckRPCsToBackend(ctx, cc, beServerAddrs[0]); err != nil {
		t.Fatal(err)
	}

	// Update the address list with the remote balancer and verify pick_first
	// behavior based on the new backends.
	tss.ls.sls <- &lbpb.ServerList{Servers: beServers[2:]}
	if err := pickfirst.CheckRPCsToBackend(ctx, cc, beServerAddrs[2]); err != nil {
		t.Fatal(err)
	}

	// Update the address list with the remote balancer and verify pick_first
	// behavior based on the new backends. Since the currently connected backend
	// is in the new list (even though it is not the first one on the list),
	// pick_first will continue to use it.
	tss.ls.sls <- &lbpb.ServerList{Servers: beServers[1:]}
	if err := pickfirst.CheckRPCsToBackend(ctx, cc, beServerAddrs[2]); err != nil {
		t.Fatal(err)
	}

	// Switch child policy to roundrobin.
	s := &grpclbstate.State{
		BalancerAddresses: []resolver.Address{
			{
				Addr:       tss.lbAddr,
				ServerName: lbServerName,
			},
		},
	}
	rs = grpclbstate.Set(resolver.State{ServiceConfig: r.CC.ParseServiceConfig(grpclbConfig)}, s)
	r.UpdateState(rs)
	testC := testgrpc.NewTestServiceClient(cc)
	if err := roundrobin.CheckRoundRobinRPCs(ctx, testC, beServerAddrs[1:]); err != nil {
		t.Fatal(err)
	}

	tss.ls.sls <- &lbpb.ServerList{Servers: beServers[0:3]}
	if err := roundrobin.CheckRoundRobinRPCs(ctx, testC, beServerAddrs[0:3]); err != nil {
		t.Fatal(err)
	}
}
