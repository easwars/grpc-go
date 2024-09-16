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

// Package testutils provides utilities for testing grpclb.
package testutils

import (
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	lbgrpc "google.golang.org/grpc/balancer/grpclb/grpc_lb_v1"
	lbpb "google.golang.org/grpc/balancer/grpclb/grpc_lb_v1"
)

type RemoteLB struct {
	lbgrpc.UnimplementedLoadBalancerServer

	ServerListChan chan *lbpb.ServerList
	FallbackChan   chan struct{}

	statsDura time.Duration
	stats     *rpcStats
	statsChan chan *lbpb.ClientStats

	UserAgentChan  chan string // Received user-agent in metadata of BalancerLoad.
	ServerNameChan chan string // Received server name in InitialLoadBalanceRequest.
}

func newRemoteBalancer(wantUserAgent, wantServerName string, statsChan chan *lbpb.ClientStats) *RemoteLB {
	return &RemoteLB{
		ServerListChan: make(chan *lbpb.ServerList, 1),
		stats:          newRPCStats(),
		statsChan:      statsChan,
		FallbackChan:   make(chan struct{}),
		UserAgentChan:  wantUserAgent,
		ServerNameChan: wantServerName,
	}
}

func (b *RemoteLB) stop() {
	close(b.ServerListChan)
	close(b.done)
}

func (b *RemoteLB) fallbackNow() {
	b.FallbackChan <- struct{}{}
}

func (b *RemoteLB) updateServerName(name string) {
	b.ServerNameChan = name
}

func (b *RemoteLB) BalanceLoad(stream lbgrpc.LoadBalancer_BalanceLoadServer) error {
	md, ok := metadata.FromIncomingContext(stream.Context())
	if !ok {
		return status.Error(codes.Internal, "failed to receive metadata")
	}
	if b.UserAgentChan != "" {
		if ua := md["user-agent"]; len(ua) == 0 || !strings.HasPrefix(ua[0], b.UserAgentChan) {
			return status.Errorf(codes.InvalidArgument, "received unexpected user-agent: %v, want prefix %q", ua, b.UserAgentChan)
		}
	}

	req, err := stream.Recv()
	if err != nil {
		return err
	}
	initReq := req.GetInitialRequest()
	if initReq.Name != b.ServerNameChan {
		return status.Errorf(codes.InvalidArgument, "invalid service name: %q, want: %q", initReq.Name, b.ServerNameChan)
	}
	resp := &lbpb.LoadBalanceResponse{
		LoadBalanceResponseType: &lbpb.LoadBalanceResponse_InitialResponse{
			InitialResponse: &lbpb.InitialLoadBalanceResponse{
				ClientStatsReportInterval: &durationpb.Duration{
					Seconds: int64(b.statsDura.Seconds()),
					Nanos:   int32(b.statsDura.Nanoseconds() - int64(b.statsDura.Seconds())*1e9),
				},
			},
		},
	}
	if err := stream.Send(resp); err != nil {
		return err
	}
	go func() {
		for {
			req, err := stream.Recv()
			if err != nil {
				return
			}
			b.stats.merge(req.GetClientStats())
			if b.statsChan != nil && req.GetClientStats() != nil {
				b.statsChan <- req.GetClientStats()
			}
		}
	}()
	for {
		select {
		case v := <-b.ServerListChan:
			resp = &lbpb.LoadBalanceResponse{
				LoadBalanceResponseType: &lbpb.LoadBalanceResponse_ServerList{
					ServerList: v,
				},
			}
		case <-b.FallbackChan:
			resp = &lbpb.LoadBalanceResponse{
				LoadBalanceResponseType: &lbpb.LoadBalanceResponse_FallbackResponse{
					FallbackResponse: &lbpb.FallbackResponse{},
				},
			}
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}
