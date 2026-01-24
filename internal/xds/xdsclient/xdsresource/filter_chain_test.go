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
 */

package xdsresource

import (
	"testing"

	"google.golang.org/grpc/internal/grpctest"
	"google.golang.org/grpc/internal/pretty"
	"google.golang.org/grpc/internal/xds/xdsclient/xdsresource/version"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	v3listenerpb "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
)

const (
	topLevel = "top level"
	vhLevel  = "virtual host level"
	rLevel   = "route level"
)

// Tests cases where we have a single filter chain with match criteria that
// contains unsupported fields.
func (s) TestUnmarshalListener_ServerSide_FilterChains_Failure(t *testing.T) {
	const listenerName = "lds.target.good:3333"

	tests := []struct {
		desc     string
		resource *anypb.Any
		wantErr  string
	}{
		{
			desc: "unsupported_destination_port_field",
			resource: &anypb.Any{
				TypeUrl: version.V3ListenerURL,
				Value: func() []byte {
					lis := &v3listenerpb.Listener{
						Name:    listenerName,
						Address: localSocketAddress,
						FilterChains: []*v3listenerpb.FilterChain{
							{
								FilterChainMatch: &v3listenerpb.FilterChainMatch{DestinationPort: &wrapperspb.UInt32Value{Value: 666}},
								Name:             "test-filter-chain",
							},
						},
					}
					mLis, _ := proto.Marshal(lis)
					return mLis
				}(),
			},
			wantErr: `Dropping filter chain "test-filter-chain" since it contains unsupported destination_port match field`,
		},
		{
			desc: "unsupported_server_names_field",
			resource: &anypb.Any{
				TypeUrl: version.V3ListenerURL,
				Value: func() []byte {
					lis := &v3listenerpb.Listener{
						Name:    listenerName,
						Address: localSocketAddress,
						FilterChains: []*v3listenerpb.FilterChain{
							{
								FilterChainMatch: &v3listenerpb.FilterChainMatch{ServerNames: []string{"example-server"}},
								Name:             "test-filter-chain",
							},
						},
					}
					mLis, _ := proto.Marshal(lis)
					return mLis
				}(),
			},
			wantErr: `Dropping filter chain "test-filter-chain" since it contains unsupported server_names match field`,
		},
		{
			desc: "unsupported_transport_protocol_field",
			resource: &anypb.Any{
				TypeUrl: version.V3ListenerURL,
				Value: func() []byte {
					lis := &v3listenerpb.Listener{
						Name:    listenerName,
						Address: localSocketAddress,
						FilterChains: []*v3listenerpb.FilterChain{
							{
								FilterChainMatch: &v3listenerpb.FilterChainMatch{TransportProtocol: "tls"},
								Name:             "test-filter-chain",
							},
						},
					}
					mLis, _ := proto.Marshal(lis)
					return mLis
				}(),
			},
			wantErr: `Dropping filter chain "test-filter-chain" since it contains unsupported value for transport_protocol match field`,
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			grpctest.ExpectWarning(test.wantErr)
			_, _, err := unmarshalListenerResource(test.resource, nil)
			if err == nil {
				t.Errorf("unmarshalListenerResource(%s) succeeded when expected to fail", pretty.ToJSON(test.resource))
			}
		})
	}
}

/*
// TestNewFilterChainImpl_Failure_BadMatchFields verifies cases where we have a
// single filter chain with match criteria that contains unsupported fields.
func (s) TestNewFilterChainImpl_Failure_BadMatchFields(t *testing.T) {
	tests := []struct {
		desc string
		lis  *v3listenerpb.Listener
	}{
		{
			desc: "unsupported application protocol field",
			lis: &v3listenerpb.Listener{
				FilterChains: []*v3listenerpb.FilterChain{
					{
						FilterChainMatch: &v3listenerpb.FilterChainMatch{ApplicationProtocols: []string{"h2"}},
					},
				},
			},
		},
		{
			desc: "bad dest address prefix",
			lis: &v3listenerpb.Listener{
				FilterChains: []*v3listenerpb.FilterChain{
					{
						FilterChainMatch: &v3listenerpb.FilterChainMatch{PrefixRanges: []*v3corepb.CidrRange{{AddressPrefix: "a.b.c.d"}}},
					},
				},
			},
		},
		{
			desc: "bad dest prefix length",
			lis: &v3listenerpb.Listener{
				FilterChains: []*v3listenerpb.FilterChain{
					{
						FilterChainMatch: &v3listenerpb.FilterChainMatch{PrefixRanges: []*v3corepb.CidrRange{cidrRangeFromAddressAndPrefixLen("10.1.1.0", 50)}},
					},
				},
			},
		},
		{
			desc: "bad source address prefix",
			lis: &v3listenerpb.Listener{
				FilterChains: []*v3listenerpb.FilterChain{
					{
						FilterChainMatch: &v3listenerpb.FilterChainMatch{SourcePrefixRanges: []*v3corepb.CidrRange{{AddressPrefix: "a.b.c.d"}}},
					},
				},
			},
		},
		{
			desc: "bad source prefix length",
			lis: &v3listenerpb.Listener{
				FilterChains: []*v3listenerpb.FilterChain{
					{
						FilterChainMatch: &v3listenerpb.FilterChainMatch{SourcePrefixRanges: []*v3corepb.CidrRange{cidrRangeFromAddressAndPrefixLen("10.1.1.0", 50)}},
					},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			if fci, err := NewFilterChainManager(test.lis); err == nil {
				t.Fatalf("NewFilterChainManager() returned %v when expected to fail", fci)
			}
		})
	}
}
*/
