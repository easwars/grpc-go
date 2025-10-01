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

// Package extauthz contains functionality to communicate with an external
// server to make authorization decisions for incoming RPCs on a gRPC server.
package extauthz

import (
	"context"
	"regexp"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/internal"
	"google.golang.org/grpc/metadata"
)

// ServerConfig contains the configuration for the external authorization
// server.
type ServerConfig struct {
	// TargetURI is the name of the external authorization server.
	TargetURI string
	// ChannelCredentials specifies the transport credentials to use to connect
	// to the external authorization server. Must not be nil.
	ChannelCredentials credentials.TransportCredentials
	// CallCredentials specifies the per-RPC credentials to use when making
	// calls to the external authorization server.
	CallCredentials []credentials.PerRPCCredentials
	// Timeout is the timeout for the external authorization check request.
	// If unset, no timeout is set.
	// TODO(easwars): If unset, do we use the background context's deadline?
	Timeout time.Duration
	// InitialMetadata is the initial metadata to be sent to the external
	// authorization server.
	InitialMetadata metadata.MD
}

// FractionalPercent specifies a percentage as a fraction of two integers.
type FractionalPercent struct {
	// Numerator is the numerator of the fractional percentage. Must be in the
	// range [0, Denominator].
	Numerator int
	// Denominator is the denominator of the fractional percentage.
	Denominator int
}

// StringMatcher specifies a match criteria for matching a string.
type StringMatcher struct {
	// We already have a string matcher implementation in
	// internal/xds/matcher/string_matcher.go. Figure out a way to reuse it
	// or copy it here without causing import cycles.
}

// HeaderMutationRules specifies the rules for what modifications an external
// authorization server may make to headers sent on the data plane RPC.
type HeaderMutationRules struct {
	// AllowExpr specifies a regular expression that matches the headers that
	// can be mutated.
	AllowExpr *regexp.Regexp
	// DisallowExpr specifies a regular expression that matches the headers that
	// cannot be mutated. This overrides the above AllowExpr if a header matches
	// both.
	DisallowExpr *regexp.Regexp
	// DisallowAll specifies that no header mutations are allowed. This
	// overrides all other settings.
	// TODO(easwars): The order of precedence is a little unclear.
	DisallowAll bool
	// DisallowIsError specifies whether to return an error if a header mutation
	// is disallowed.
	DisallowIsError bool
}

// InterceptorConfig contains the configuration for the external authorization
// server interceptor.
type InterceptorConfig struct {
	// Server is the configuration for the external authorization server.
	Server ServerConfig
	// EnabledPercent specifies the percentage of requests to be authorized by
	// the external authorization server. If unset, all requests will be
	// authorized by the external authorization server.
	EnabledPercent FractionalPercent
	// DenyIfDisabled specifies whether to deny requests when external
	// authorization is disabled via the EnabledPercent configuration. If true,
	// requests will be denied with a status based on StatusCodeOnError.
	DenyIfDisabled bool
	// StatusCodeOnError is a HTTP status code to use when the external
	// authorization is disabled or when the call to the external authorization
	// server fails. The HTTP status code will be converted to a gRPC status
	// code using the mapping defined in
	// https://github.com/grpc/grpc/blob/master/doc/http-grpc-status-mapping.md
	StatusCodeOnError int
	// FailureModeAllow specifies the behavior when the call to the external
	// authorization server fails. If true, the request will be allowed in such
	// cases. If false, the request will be denied with a status based on
	// StatusCodeOnError.
	FailureModeAllow bool
	// FailureModeAllowHeaderAdd specifies whether to add the
	// `x-envoy-auth-failure-mode-allowed: true` header to the data plane RPC
	// when the call to the external authorization server fails.
	FailureModeAllowHeaderAdd bool
	// AllowedHeaders specifies the headers that are allowed to be sent to the
	// external authorization server.
	AllowedHeaders []StringMatcher
	// DisallowedHeaders specifies the headers that will not be sent to the
	// external authorization server. This overrides the above AllowedHeaders if
	// a header matches both.
	DisallowedHeaders []StringMatcher
	// DecoderHeaderMutationRules specifies the rules for what modifications an
	// external authorization server may make to headers sent on the data plane
	// RPC.
	DecoderHeaderMutationRules HeaderMutationRules
	// IncludePeerCertificate specifies whether to include the peer certificate
	// in the request sent to the external authorization server.
	IncludePeerCertificate bool
}

var joinServerOptions = internal.JoinServerOptions.(func(...grpc.ServerOption) grpc.ServerOption)

// ServerOption returns a server option which enables external authorization
// for incoming RPCs on a gRPC server.
//
// Applications interested in using an external authorization server should pass
// the server option returned from this function to grpc.NewServer().
func ServerOption(cfg *InterceptorConfig) grpc.ServerOption {
	client, err := newClient(cfg)
	if err != nil {
		return nil
	}
	return joinServerOptions(grpc.ChainUnaryInterceptor(client.unaryInterceptor), grpc.ChainStreamInterceptor(client.streamInterceptor))
}

type client struct {
	cc  *grpc.ClientConn
	cfg *InterceptorConfig
}

func newClient(cfg *InterceptorConfig) (*client, error) {
	// TODO(easwars): Validate the config.
	// - Ensure target URI is non-empty and is a valid URI.
	// - Ensure channel credentials is non-nil.
	// - Ensure timeout is non-negative.
	// - Ensure InitialMetadata has valid keys. Inside of each entry:
	// key: Value length must be in the range [1, 16384). Must be a valid HTTP/2 header name.
	// value: Specifies the header value. Must be shorter than 16384 bytes. Must be a valid HTTP/2 header value. Not used if key ends in -bin and raw_value is set.
	// raw_value: Used only if key ends in -bin. Must be shorter than 16384 bytes. Will be base64-encoded on the wire, unless the pure binary metadata extension from gRFC G1: True Binary Metadata is used.
	// - Ensure numerator and denominator in EnabledPercent are valid.
	// - Ensure StatusCodeOnError is a valid HTTP status code.

	// TODO(easwars): Create the gRPC channel to the external auth server.

	return &client{cfg: cfg}, nil
}

func (c *client) unaryInterceptor(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	return nil, nil
}

func (c *client) streamInterceptor(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	return nil
}

// TODO(easwars): Implement the signature for the methods that will be used to
// implement the HTTP filter interceptor. This will ensure that we can share
// code between the two interceptors.
