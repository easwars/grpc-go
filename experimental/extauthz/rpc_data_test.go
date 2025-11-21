/*
 *
 * Copyright 2025 gRPC authors.
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

package extauthz

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/url"
	"sort"
	"testing"

	v3corepb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/internal/xds/matcher"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/testing/protocmp"
)

type testServerTransportStreamWithMethod struct {
	grpc.ServerTransportStream
	method string
}

func (sts *testServerTransportStreamWithMethod) Method() string {
	return sts.method
}

func (s) TestNewRPCData_Failure(t *testing.T) {
	tests := []struct {
		name              string
		inputCtx          func(ctx context.Context) context.Context
		allowedHeaders    []matcher.StringMatcher
		disallowedHeaders []matcher.StringMatcher
		wantErr           string
	}{
		{
			name:     "MissingMetadata",
			inputCtx: func(ctx context.Context) context.Context { return ctx },
			wantErr:  "missing metadata in incoming context",
		},
		{
			name: "MissingServerTransportStream",
			inputCtx: func(ctx context.Context) context.Context {
				return metadata.NewIncomingContext(ctx, metadata.Pairs("a", "1"))
			},
			wantErr: "missing method in incoming context",
		},
		{
			name: "MissingPeer",
			inputCtx: func(ctx context.Context) context.Context {
				ctx = grpc.NewContextWithServerTransportStream(ctx, &testServerTransportStreamWithMethod{method: "foo"})
				return metadata.NewIncomingContext(ctx, metadata.Pairs("a", "1"))
			},
			wantErr: "missing peer info in incoming context",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
			defer cancel()

			if _, err := newRPCData(tt.inputCtx(ctx), tt.allowedHeaders, tt.disallowedHeaders); err == nil {
				t.Fatal("newRPCData() succeeded when expected to fail")
			} else if err.Error() != tt.wantErr {
				t.Fatalf("newRPCData() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func (s) TestNewRPCData_Success(t *testing.T) {
	tests := []struct {
		name              string
		inputCtx          func(ctx context.Context) context.Context
		allowedHeaders    []matcher.StringMatcher
		disallowedHeaders []matcher.StringMatcher
		wantRPCData       *rpcData
	}{
		{
			name: "NoTLSInfo",
			inputCtx: func(ctx context.Context) context.Context {
				ctx = peer.NewContext(ctx, &peer.Peer{
					Addr:      &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 666},
					LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 999},
				})
				ctx = grpc.NewContextWithServerTransportStream(ctx, &testServerTransportStreamWithMethod{method: "foo"})
				return metadata.NewIncomingContext(ctx, metadata.Pairs("a", "1", "b", "2"))
			},
			wantRPCData: &rpcData{
				localAddr:  "127.0.0.1:999",
				remoteAddr: "127.0.0.1:666",
				methodName: "foo",
				principal:  "",
				peerCert:   "",
				headerMap: &v3corepb.HeaderMap{
					Headers: []*v3corepb.HeaderValue{
						{Key: "a", RawValue: []byte("1")},
						{Key: "b", RawValue: []byte("2")},
					},
				},
			},
		},
		{
			name: "WithTLSInfo_NoPeerCert",
			inputCtx: func(ctx context.Context) context.Context {
				ctx = peer.NewContext(ctx, &peer.Peer{
					Addr:      &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 666},
					LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 999},
					AuthInfo: credentials.TLSInfo{
						State: tls.ConnectionState{
							PeerCertificates: []*x509.Certificate{},
						},
					},
				})
				ctx = grpc.NewContextWithServerTransportStream(ctx, &testServerTransportStreamWithMethod{method: "foo"})
				return metadata.NewIncomingContext(ctx, metadata.Pairs("a", "1", "b", "2"))
			},
			wantRPCData: &rpcData{
				localAddr:  "127.0.0.1:999",
				remoteAddr: "127.0.0.1:666",
				methodName: "foo",
				principal:  "",
				peerCert:   "",
				headerMap: &v3corepb.HeaderMap{
					Headers: []*v3corepb.HeaderValue{
						{Key: "a", RawValue: []byte("1")},
						{Key: "b", RawValue: []byte("2")},
					},
				},
			},
		},
		{
			name: "WithTLSInfo_WithPeerCert",
			inputCtx: func(ctx context.Context) context.Context {
				ctx = peer.NewContext(ctx, &peer.Peer{
					Addr:      &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 666},
					LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 999},
					AuthInfo: credentials.TLSInfo{
						State: tls.ConnectionState{
							PeerCertificates: []*x509.Certificate{{
								URIs: []*url.URL{{
									Host: "localhost:666",
								}},
							}},
						},
					},
				})
				ctx = grpc.NewContextWithServerTransportStream(ctx, &testServerTransportStreamWithMethod{method: "foo"})
				return metadata.NewIncomingContext(ctx, metadata.Pairs("a", "1"))
			},
			wantRPCData: &rpcData{
				localAddr:  "127.0.0.1:999",
				remoteAddr: "127.0.0.1:666",
				methodName: "foo",
				principal:  "//localhost:666",
				peerCert:   "-----BEGIN+CERTIFICATE-----%0A-----END+CERTIFICATE-----%0A",
				headerMap: &v3corepb.HeaderMap{
					Headers: []*v3corepb.HeaderValue{
						{Key: "a", RawValue: []byte("1")},
					},
				},
			},
		},
		{
			name: "WithHeaderFilters",
			inputCtx: func(ctx context.Context) context.Context {
				ctx = peer.NewContext(ctx, &peer.Peer{
					Addr:      &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 666},
					LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 999},
				})
				ctx = grpc.NewContextWithServerTransportStream(ctx, &testServerTransportStreamWithMethod{method: "foo"})
				return metadata.NewIncomingContext(ctx, metadata.Pairs("a", "1", "b", "2", "c", "3"))
			},
			allowedHeaders: []matcher.StringMatcher{
				newStringMatcher(t, matcher.StringMatcherOptions{ExactMatch: newStringP("a")}),
				newStringMatcher(t, matcher.StringMatcherOptions{PrefixMatch: newStringP("c")}),
			},
			disallowedHeaders: []matcher.StringMatcher{
				newStringMatcher(t, matcher.StringMatcherOptions{ExactMatch: newStringP("b")}),
			},
			wantRPCData: &rpcData{
				localAddr:  "127.0.0.1:999",
				remoteAddr: "127.0.0.1:666",
				methodName: "foo",
				principal:  "",
				peerCert:   "",
				headerMap: &v3corepb.HeaderMap{
					Headers: []*v3corepb.HeaderValue{
						{Key: "a", RawValue: []byte("1")},
						{Key: "c", RawValue: []byte("3")},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
			defer cancel()

			gotRPCData, err := newRPCData(tt.inputCtx(ctx), tt.allowedHeaders, tt.disallowedHeaders)
			if err != nil {
				t.Fatalf("newRPCData() failed: %v", err)
			}
			if gotRPCData.headerMap != nil {
				if headers := gotRPCData.headerMap.Headers; headers != nil {
					sort.Slice(headers, func(i, j int) bool {
						return headers[i].Key < headers[j].Key
					})
				}
			}
			if !cmp.Equal(tt.wantRPCData, gotRPCData, cmp.AllowUnexported(rpcData{}), protocmp.Transform()) {
				t.Fatalf("newRPCData() mismatch:\ngot: %+v\nwant: %+v", gotRPCData, tt.wantRPCData)
			}
		})
	}
}

func newStringMatcher(t *testing.T, opts matcher.StringMatcherOptions) matcher.StringMatcher {
	matcher, err := matcher.NewStringMatcher(opts)
	if err != nil {
		t.Fatalf("Failed to create StringMatcher: %v", err)
	}
	return matcher
}

func newStringP(s string) *string {
	return &s
}

func (s) TestConstructHeaderMap(t *testing.T) {
	tests := []struct {
		name              string
		md                metadata.MD
		allowedHeaders    []matcher.StringMatcher
		disallowedHeaders []matcher.StringMatcher
		wantHeaderMap     *v3corepb.HeaderMap
	}{
		{
			name:          "NoHeaders",
			md:            metadata.MD{},
			wantHeaderMap: nil,
		},
		{
			name: "NoAllowedHeaders",
			md: metadata.MD{
				"a": {"1"},
				"b": {"2"},
			},
			wantHeaderMap: &v3corepb.HeaderMap{
				Headers: []*v3corepb.HeaderValue{
					{Key: "a", RawValue: []byte("1")},
					{Key: "b", RawValue: []byte("2")},
				},
			},
		},
		{
			name: "WithMultipleValues_ForSingleHeader",
			md: metadata.MD{
				"a": {"1", "11"},
				"b": {"2"},
			},
			wantHeaderMap: &v3corepb.HeaderMap{
				Headers: []*v3corepb.HeaderValue{
					{Key: "a", RawValue: []byte("1")},
					{Key: "a", RawValue: []byte("11")},
					{Key: "b", RawValue: []byte("2")},
				},
			},
		},
		{
			name: "WithAllowedHeaders",
			md: metadata.MD{
				"a": {"1"},
				"b": {"2"},
				"c": {"3"},
			},
			allowedHeaders: []matcher.StringMatcher{
				newStringMatcher(t, matcher.StringMatcherOptions{ExactMatch: newStringP("a")}),
				newStringMatcher(t, matcher.StringMatcherOptions{PrefixMatch: newStringP("c")}),
			},
			wantHeaderMap: &v3corepb.HeaderMap{
				Headers: []*v3corepb.HeaderValue{
					{Key: "a", RawValue: []byte("1")},
					{Key: "c", RawValue: []byte("3")},
				},
			},
		},
		{
			name: "WithDisallowedHeaders",
			md: metadata.MD{
				"a": {"1"},
				"b": {"2"},
				"c": {"3"},
			},
			disallowedHeaders: []matcher.StringMatcher{
				newStringMatcher(t, matcher.StringMatcherOptions{ExactMatch: newStringP("b")}),
				newStringMatcher(t, matcher.StringMatcherOptions{PrefixMatch: newStringP("c")}),
			},
			wantHeaderMap: &v3corepb.HeaderMap{
				Headers: []*v3corepb.HeaderValue{
					{Key: "a", RawValue: []byte("1")},
				},
			},
		},
		{
			name: "WithAllowedAndDisallowedHeaders",
			md: metadata.MD{
				"a": {"1"},
				"b": {"2"},
				"c": {"3"},
				"d": {"4"},
			},
			allowedHeaders: []matcher.StringMatcher{
				newStringMatcher(t, matcher.StringMatcherOptions{ExactMatch: newStringP("a")}),
				newStringMatcher(t, matcher.StringMatcherOptions{PrefixMatch: newStringP("b")}),
				newStringMatcher(t, matcher.StringMatcherOptions{PrefixMatch: newStringP("c")}),
			},
			disallowedHeaders: []matcher.StringMatcher{
				newStringMatcher(t, matcher.StringMatcherOptions{ExactMatch: newStringP("b")}),
				newStringMatcher(t, matcher.StringMatcherOptions{PrefixMatch: newStringP("c")}),
			},
			wantHeaderMap: &v3corepb.HeaderMap{
				Headers: []*v3corepb.HeaderValue{
					{Key: "a", RawValue: []byte("1")},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotHeaderMap := constructHeaderMap(tt.md, tt.allowedHeaders, tt.disallowedHeaders)
			if gotHeaderMap != nil {
				sort.Slice(gotHeaderMap.Headers, func(i, j int) bool {
					return gotHeaderMap.Headers[i].Key < gotHeaderMap.Headers[j].Key
				})
			}
			if diff := cmp.Diff(tt.wantHeaderMap, gotHeaderMap, protocmp.Transform()); diff != "" {
				t.Fatalf("constructHeaderMap() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
