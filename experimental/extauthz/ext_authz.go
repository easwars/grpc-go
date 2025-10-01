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

// Package extauthz is EXPERIMENTAL and contains functionality to communicate
// with an external server to make authorization decisions for incoming RPCs on
// a gRPC server.
//
// This package will be removed in a future release, once the external
// authorization server functionality is available as part of the xds package.
package extauthz

import (
	"context"
	rand "math/rand/v2"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/internal"
	"google.golang.org/grpc/internal/status"
	"google.golang.org/grpc/internal/transport"
	"google.golang.org/grpc/metadata"

	v3corepb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	v3authgrpc "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	v3authpb "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	timestamppb "google.golang.org/protobuf/types/known/timestamppb"
)

var joinServerOptions = internal.JoinServerOptions.(func(...grpc.ServerOption) grpc.ServerOption)
var randIntn = rand.IntN

// ServerOption returns a server option that enables external authorization
// for incoming RPCs on a gRPC server.
//
// Applications interested in using an external authorization server should pass
// the server option returned from this function to grpc.NewServer().
//
// Applications must ensure that the returned cancel function is called when the
// gRPC server is stopped, to clean up resources associated with the external
// authorization server client.
func ServerOption(cfg *InterceptorConfig) (o grpc.ServerOption, cancel func()) {
	client, err := newClient(cfg)
	if err != nil {
		return nil, func() {}
	}

	cancel = sync.OnceFunc(func() {
		client.cc.Close()
	})
	return joinServerOptions(grpc.ChainUnaryInterceptor(client.unaryInterceptor), grpc.ChainStreamInterceptor(client.streamInterceptor)), cancel
}

// client is a client for the external authorization server. It is used by the
// server interceptors to make authorization decisions for incoming RPCs.
type client struct {
	cfg    *InterceptorConfig
	cc     *grpc.ClientConn
	client v3authgrpc.AuthorizationClient
}

// newClient creates a new client for the external authorization server based
// on the provided configuration.
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
		cc:     cc,
		client: v3authgrpc.NewAuthorizationClient(cc),
		cfg:    cfg,
	}, nil
}

// unaryInterceptor is a server interceptor that makes authorization decisions
// for incoming unary RPCs by calling the external authorization server.
func (c *client) unaryInterceptor(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	ctx, err := c.processRPC(ctx)
	if err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

// streamInterceptor is a server interceptor that makes authorization decisions
// for incoming streaming RPCs by calling the external authorization server.
func (c *client) streamInterceptor(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if err := c.processRPC(ss.Context()); err != nil {
		return err
	}
	return handler(srv, ss)
}

// AllowRPC makes the authorization decision for an incoming RPC by calling the
// external authorization server.
//
// It implements the resolver.ServerInterceptor interface that is used for xDS
// HTTP filters on the server side.
func (c *client) AllowRPC(ctx context.Context) error {
	// TODO(easwars): The xDS server filter API needs to be changed to allow
	// filters to add headers to the RPC context and pass them to the next
	// filter. Once that is done, we can implement this function to make the
	// authorization decision by calling the external authorization server.
	panic("extauthz: xDS server interceptor interface is not yet supported")
}

func (c *client) handleIncomingUnaryRPC(incomingCtx context.Context) (context.Context, error) {
	enabled, err := c.isExtAuthEnabled()
	if !enabled {
		return incomingCtx, err
	}

	data, err := newRPCData(incomingCtx, c.cfg.AllowedHeaders, c.cfg.DisallowedHeaders)
	if err != nil {
		return incomingCtx, status.Errorf(codes.Internal, "extauthz: %v", err)
	}

	ctx, cancel := c.newContextWithTimeoutAndInitialMetadata(incomingCtx)
	defer cancel()
	resp, err := c.callExtAuthzService(ctx, data)
	if err != nil {
		if !c.cfg.FailureModeAllow {
			return incomingCtx, c.statusErrorf("extauthz: RPC denied due to error calling external authorization server: %v", err)
		}

		if c.cfg.FailureModeAllowHeaderAdd {
			// incomingCtx is guaranteed to have metadata because we already
			// checked that in newRPCData.
			md, _ := metadata.FromIncomingContext(incomingCtx)
			md.Append("x-envoy-auth-failure-mode-allowed", "true")
			return metadata.NewIncomingContext(ctx, md), nil
		}
		return incomingCtx, nil
	}

	deniedResp, ok := resp.GetHttpResponse().(*v3authpb.CheckResponse_DeniedResponse)
	if ok {
		trailers, err := c.handleDeniedResponse(deniedResp.DeniedResponse)
		if err != nil {
			return incomingCtx, err
		}

		// Set the trailers on the context so that they can be sent to the
		// caller.
		ss := grpc.ServerTransportStreamFromContext(incomingCtx)
		if err := ss.SetTrailer(trailers); err != nil {
			// If setting the trailers fails, return an internal error.
			return incomingCtx, err
		}
		return incomingCtx, err
	}

	allowedResp, ok := resp.GetHttpResponse().(*v3authpb.CheckResponse_OkResponse)
	if !ok {
		return incomingCtx, status.Errorf(codes.Internal, "extauthz: received unknown response type from external authorization server")
	}
	// TODO(easwars): Figure out how to handle the allowed response's headers.
	return incomingCtx, nil
}

// statusErrorf returns a grpc status error with a code derived from the
// StatusCodeOnError config field and a message constructed from the format
// specifier and args.
func (c *client) statusErrorf(format string, a ...any) error {
	// TODO(easwars): Consider caching the grpc status code derived from
	// StatusCodeOnError in the client struct to avoid computing it on every
	// error.
	return status.Errorf(c.grpcStatusCode(c.cfg.StatusCodeOnError), format, a...)
}

// newContextWithTimeoutAndInitialMetadata creates a new context for the call to
// the external authorization server. This context has a deadline that is the
// minimum of the incoming context's deadline and the timeout specified in the
// config. It also contains the incoming context's metadata, merged with the
// initial metadata specified in the config.
//
// TODO(easwars): Ensure that passing the incoming context to the ext_authz
// RPC propagates the trace context, so that the ext_authz RPC appears as a
// child span on the data plane RPC's trace.
func (c *client) newContextWithTimeoutAndInitialMetadata(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Server.Timeout)
	incomingMD, _ := metadata.FromIncomingContext(ctx)
	ctx = metadata.NewOutgoingContext(ctx, metadata.Join(c.cfg.Server.InitialMetadata, incomingMD))
	return ctx, cancel
}

// handleIncomingRPC is the core logic for handling an incoming RPC and making
// the authorization decision by calling the external authorization server.
//
// For unary RPCs, the returned context is passed to the next interceptor in the
// chain or to the application handler. For streaming RPCs, the returned server
// stream is passed to the next interceptor in the chain or to the application
// handler. If an error is returned, it is sent to the caller of the RPC, and no
// further processing is done on the RPC by the gRPC.
func (c *client) handleIncomingRPC(ctx context.Context, ss grpc.ServerStream) (context.Context, grpc.ServerStream, error) {
	enabled, err := c.isExtAuthEnabled()
	if !enabled {
		return ctx, ss, err
	}

	data, err := newRPCData(ctx, c.cfg.AllowedHeaders, c.cfg.DisallowedHeaders)
	if err != nil {
		return ctx, ss, status.Errorf(codes.Internal, "extauthz: %v", err)
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.Server.Timeout)
	defer cancel()
	incomingMD, _ := metadata.FromIncomingContext(ctx)
	ctx = metadata.NewOutgoingContext(ctx, metadata.Join(c.cfg.Server.InitialMetadata, incomingMD))
	resp, err := c.callExtAuthzService(ctx, data)
	if err != nil {
		// If the RPC to the ext_authz service fails and the failure_mode_allow
		// config field is set to false, the data plane RPC will be failed with
		// the status derived from the StatusCodeOnError config field.
		if !c.cfg.FailureModeAllow {
			return ctx, ss, status.Errorf(c.grpcStatusCode(c.cfg.StatusCodeOnError), "extauthz: RPC denied due to error calling external authorization server: %v", err)
		}

		// Otherwise, the data plane RPC will be allowed. If the
		// failure_mode_allow_header_add config field is true, then the filter
		// will add a x-envoy-auth-failure-mode-allowed: true header to the data
		// plane RPC.
		if c.cfg.FailureModeAllowHeaderAdd {
			// TODO(easwars): This needs some stuff to be figured out. The
			// existing server filter API does not allow us to do this. For the
			// interceptors though, we could do what is mentioned in the
			// following example:
			// https://github.com/grpc/grpc-go/blob/master/examples/features/metadata_interceptor/README.md
		}
		return ctx, ss, nil
	}

	/*
		sts := grpc.ServerTransportStreamFromContext(ctx)
		ws := &wrappedStream{
			ServerTransportStream:  sts,
			metadataExchangeLabels: metadataExchangeLabels,
		}
		ctx = grpc.NewContextWithServerTransportStream(ctx, alts)
	*/
	if resp.Status.Code != int32(codes.OK) {
		return c.handleDeniedResponse(resp.GetDeniedResponse())
	}
	return c.handleOKResponse(resp)
}

// isExtAuthEnabled checks if the external authorization is enabled for the
// incoming RPC based on the EnabledPercent configuration.
//
// The first return value indicates if external authorization is enabled for the
// RPC. If it is false, the second return value indicates if the RPC should be
// failed or if it should be passed to the next interceptor or the application
// handler without modification.
//
// TODO(easwars): Consider making this a method on the InterceptorConfig struct.
func (c *client) isExtAuthEnabled() (bool, error) {
	enabled := true
	if ep := c.cfg.EnabledPercent; ep.Denominator != 0 {
		if randIntn(ep.Denominator) > ep.Numerator {
			enabled = false
		}
	}

	if enabled {
		return true, nil
	}

	// If external authorization is disabled for this RPC, the next step depends
	// on the value of the DenyAtDisabled config field. If this field is true,
	// the RPC will be failed with the status derived from the StatusCodeOnError
	// config field. Else, the RPC will be passed to the next interceptor
	// without modification.
	if c.cfg.DenyAtDisabled {
		return false, c.statusErrorf("extauthz: RPC denied due to external authorization being disabled")
	}
	return false, nil
}

// grpcStatusCode converts an HTTP status code to a gRPC status code using the
// mapping defined in
// https://github.com/grpc/grpc/blob/master/doc/http-grpc-status-mapping.md
//
// TODO(easwars): Consider moving this to the internal/transport package if it
// is useful elsewhere, or moving it to the status or codes package.
func (c *client) grpcStatusCode(httpStatus int) codes.Code {
	if code, ok := transport.HTTPStatusConvTab[httpStatus]; ok {
		return code
	}
	return codes.Unknown
}

func (c *client) callExtAuthzService(ctx context.Context, data *rpcData) (*v3authpb.CheckResponse, error) {
	req := &v3authpb.CheckRequest{
		Attributes: &v3authpb.AttributeContext{
			Source: &v3authpb.AttributeContext_Peer{
				Address: &v3corepb.Address{
					Address: &v3corepb.Address_SocketAddress{
						SocketAddress: &v3corepb.SocketAddress{
							Address: data.remoteAddr,
						},
					},
				},
				Principal:   data.principal,
				Certificate: data.peerCert,
			},
			Destination: &v3authpb.AttributeContext_Peer{
				Address: &v3corepb.Address{
					Address: &v3corepb.Address_SocketAddress{
						SocketAddress: &v3corepb.SocketAddress{
							Address: data.localAddr,
						},
					},
				},
				// TODO(easwars): Currently, the tls package does not provide a way
				// to get the local principal (i.e. the server certificate's
				// identity). See: https://github.com/golang/go/issues/24673
				Principal: "",
			},
			Request: &v3authpb.AttributeContext_Request{
				// TODO(easwars): Record the RPC's incoming timestamp in the
				// server and make it available as a value in the context.
				Time: &timestamppb.Timestamp{
					Seconds: time.Now().Unix(),
					Nanos:   int32(time.Now().UnixNano() % 1e9),
				},
				Http: &v3authpb.AttributeContext_HttpRequest{
					Method:    "POST",
					HeaderMap: data.headerMap,
					Path:      data.methodName,
					Size:      -1,
					Protocol:  "HTTP/2",
				},
			},
		},
	}
	return c.client.Check(ctx, req)
}

func (c *client) handleOKResponse(resp *v3authpb.CheckResponse) error {
	// gRPC will not support rewriting the :scheme, :method, :path, :authority,
	// or host headers, regardless of what settings are present in the filter's
	// config. If the server specifies a rewrite for one of these headers, that
	// rewrite will be ignored. Otherwise, header rewriting will be allowed
	// based on the decoder_header_mutation_rules config field.
	return nil
}

func (c *client) handleDeniedResponse(deniedResponse *v3authpb.DeniedHttpResponse) (trailers metadata.MD, err error) {
	// Compute the status to return to the caller based on the status returned by
	// the external authorization server.
	var code codes.Code
	if deniedResponse.GetStatus() == nil {
		code = codes.PermissionDenied
	} else {
		code = c.grpcStatusCode(int(deniedResponse.GetStatus().GetCode()))
	}

	trailers, err = c.cfg.DecoderHeaderMutationRules.Apply(deniedResponse.GetHeaders(), nil)
	if err != nil {
		return nil, status.Errorf(code, "extauthz: error applying header mutation rules to denied response: %v", err)
	}
	return trailers, status.Errorf(code, "extauthz: RPC denied by external authorization server")
}

type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *wrappedStream) Context() context.Context {
	return s.ctx
}

func (s *wrappedStream) SetTrailer(metadata.MD) {
	// TODO(easwars): Maintain a map of trailer metadata and accumulate the
	// trailers set by interceptors that come after us in the chain and by the
	// application. Make mutations to this map based on the metadata returned by
	// the external authorization server and using the
	// decoder_header_mutation_rules config field. Set the trailers on the
	// stream before returning from the interceptor.
	panic("extauthz: setting trailer metadata is not yet supported")
}
