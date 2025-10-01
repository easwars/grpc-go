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
	"errors"
	"fmt"
	rand "math/rand/v2"
	"net/http"
	"regexp"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/internal"
	"google.golang.org/grpc/internal/status"
	"google.golang.org/grpc/internal/transport"
	"google.golang.org/grpc/metadata"
)

// InterceptorConfig contains the configuration for the external authorization
// server interceptor.
type InterceptorConfig struct {
	// Server is the configuration for the external authorization server.
	Server ServerConfig
	// EnabledPercent specifies the percentage of requests to be authorized by
	// the external authorization server. If unset, all requests will be
	// authorized by the external authorization server.
	EnabledPercent FractionalPercent
	// DenyAtDisabled specifies whether to deny requests when external
	// authorization is disabled via the EnabledPercent configuration. If true,
	// requests will be denied with a status based on StatusCodeOnError.
	DenyAtDisabled bool
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

var joinServerOptions = internal.JoinServerOptions.(func(...grpc.ServerOption) grpc.ServerOption)
var randIntn = rand.IntN

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
	if err := validateInterceptorConfig(cfg); err != nil {
		return nil, err
	}

	dOpts := []grpc.DialOption{grpc.WithTransportCredentials(cfg.Server.ChannelCredentials)}
	for _, creds := range cfg.Server.CallCredentials {
		dOpts = append(dOpts, grpc.WithPerRPCCredentials(creds))
	}
	cc, err := grpc.NewClient(cfg.Server.TargetURI, dOpts...)
	if err != nil {
		return nil, err
	}
	return &client{
		cc:  cc,
		cfg: cfg,
	}, nil
}

func validateInterceptorConfig(cfg *InterceptorConfig) error {
	if cfg == nil {
		return errors.New("extauthz: InterceptorConfig must be non-nil")
	}
	if err := validateServerConfig(&cfg.Server); err != nil {
		return err
	}
	if err := validateFractionalPercent(cfg.EnabledPercent); err != nil {
		return err
	}
	if cfg.StatusCodeOnError != 0 && http.StatusText(cfg.StatusCodeOnError) == "" {
		return fmt.Errorf("extauthz: StatusCodeOnError (%d) is not a valid HTTP status code", cfg.StatusCodeOnError)
	}
	return nil
}

func validateServerConfig(cfg *ServerConfig) error {
	if cfg.TargetURI == "" {
		return errors.New("extauthz: ServerConfig.TargetURI must be a non-empty string")
	}
	if cfg.ChannelCredentials == nil {
		return errors.New("extauthz: ServerConfig.ChannelCredentials must be non-nil")
	}
	if cfg.Timeout < 0 {
		return errors.New("extauthz: ServerConfig.Timeout must be non-negative")
	}
	for k, vals := range cfg.InitialMetadata {
		if len(k) == 0 || len(k) >= 16384 {
			return fmt.Errorf("extauthz: ServerConfig.InitialMetadata key %q has invalid length %d; must be in range [1, 16384)", k, len(k))
		}
		for _, v := range vals {
			if len(v) >= 16384 {
				return fmt.Errorf("extauthz: ServerConfig.InitialMetadata value for key %q has invalid length %d; must be less than 16384", k, len(v))
			}
		}
	}
	return nil
}

func validateFractionalPercent(fp FractionalPercent) error {
	if fp.Numerator < 0 || fp.Denominator < 0 {
		return fmt.Errorf("extauthz: FractionalPercent.Numerator (%d) and Denominator (%d) must be non-negative", fp.Numerator, fp.Denominator)
	}
	if fp.Numerator > fp.Denominator {
		return fmt.Errorf("extauthz: FractionalPercent.Numerator (%d) must be in range [0, %d]", fp.Numerator, fp.Denominator)
	}
	return nil
}

func (c *client) unaryInterceptor(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if err := c.processRPC(ctx); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

func (c *client) streamInterceptor(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if err := c.processRPC(ss.Context()); err != nil {
		return err
	}
	return handler(srv, ss)
}

func (c *client) AllowRPC(ctx context.Context) error {
	return c.processRPC(ctx)
}

// processRPC is the core logic for processing an RPC and making the
// authorization decision by calling the external authorization server.
func (c *client) processRPC(ctx context.Context) error {
	// Check if the external authorization is enabled for this RPC based on the
	// EnabledPercent configuration.
	enabled := true
	if ep := c.cfg.EnabledPercent; ep.Denominator != 0 {
		if randIntn(ep.Denominator) > ep.Numerator {
			enabled = false
		}
	}

	// If external authorization is disabled for this RPC, the next step depends
	// on the value of the DenyAtDisabled config field. If this field is true,
	// the RPC will be failed with the status derived from the StatusCodeOnError
	// config field. Else, the RPC will be passed to the next interceptor
	// without modification.
	if !enabled {
		if c.cfg.DenyAtDisabled {
			code, ok := transport.HTTPStatusConvTab[c.cfg.StatusCodeOnError]
			if !ok {
				code = codes.Unknown
			}
			return status.Err(code, "RPC denied due to external authorization being disabled")
		}
		return nil
	}

	// TODO(easwars): Make the call to the external authorization server
	return nil
}
