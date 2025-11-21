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
	"fmt"
	rand "math/rand/v2"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/internal"
	internalgrpclog "google.golang.org/grpc/internal/grpclog"
	"google.golang.org/grpc/internal/status"
	"google.golang.org/grpc/internal/transport"
	"google.golang.org/grpc/metadata"

	v3corepb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	v3authgrpc "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	v3authpb "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	timestamppb "google.golang.org/protobuf/types/known/timestamppb"
)

var (
	logger            = grpclog.Component("xds")
	joinServerOptions = internal.JoinServerOptions.(func(...grpc.ServerOption) grpc.ServerOption)
	randFunc          = rand.Uint32N
)

const prefix = "[extauthz-client %p] "

func prefixLogger(p *client) *internalgrpclog.PrefixLogger {
	return internalgrpclog.NewPrefixLogger(logger, fmt.Sprintf(prefix, p))
}

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
	return joinServerOptions(grpc.ChainUnaryInterceptor(client.unaryInterceptorForServer), grpc.ChainStreamInterceptor(client.streamInterceptorForServer)), cancel
}

// client is a client for the external authorization server. It is used by the
// server interceptors to make authorization decisions for incoming RPCs.
type client struct {
	cfg    *InterceptorConfig
	cc     *grpc.ClientConn
	client v3authgrpc.AuthorizationClient
	logger *internalgrpclog.PrefixLogger
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
	c := &client{
		cc:     cc,
		client: v3authgrpc.NewAuthorizationClient(cc),
		cfg:    cfg,
	}
	c.logger = prefixLogger(c)
	return c, nil
}

// unaryInterceptorForServer makes authorization decisions for incoming unary
// RPCs by calling out to an external authorization server.
func (c *client) unaryInterceptorForServer(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	incomingStream := &commonServerStream{
		unaryStream: grpc.ServerTransportStreamFromContext(ctx),
		ctx:         ctx,
	}
	ctx, trailerHVOs, err := c.handleIncomingRPC(incomingStream)
	if err != nil {
		return nil, err
	}

	// Create a wrapped stream to handle trailers set by subsequent filters in
	// the chain before applying trailer mutations specified by the external
	// authorization server.
	ss := grpc.ServerTransportStreamFromContext(ctx)
	wss := &wrappedUnaryServerStream{ServerTransportStream: ss}
	ctx = grpc.NewContextWithServerTransportStream(ctx, wss)
	resp, err := handler(ctx, req)

	// Collect all trailers added by subsequent filters and apply mutations
	// specified by the external authorization server.
	mds := metadata.Join(wss.trailerMDs...)
	trailers, err2 := c.cfg.DecoderHeaderMutationRules.ApplyAdditions(trailerHVOs, mds)
	if err2 != nil {
		return nil, status.Errorf(codes.Unknown, "extauthz: error applying header mutation rules to trailers: %v", err2)
	}
	if err2 := ss.SetTrailer(trailers); err2 != nil {
		// If setting the trailers fails, return an INTERNAL error.
		return nil, fmt.Errorf("%v: %w", err, err2)
	}
	return resp, err
}

// minimalServerStream wraps the functionality required by the incoming RPC
// handling code on the server side.
type minimalServerStream interface {
	incomingContext() context.Context
	setTrailer(metadata.MD) error
}

// commonServerStream makes it possible to share the incoming RPC handling code
// between unary and streaming RPCs on the server side.
type commonServerStream struct {
	ctx context.Context

	// Only one of the two streams must be set.
	unaryStream     grpc.ServerTransportStream
	streamingStream grpc.ServerStream
}

func (css *commonServerStream) incomingContext() context.Context {
	return css.ctx
}

func (css *commonServerStream) setTrailer(md metadata.MD) error {
	if css.unaryStream != nil {
		return css.unaryStream.SetTrailer(md)
	}
	css.streamingStream.SetTrailer(md)
	return nil
}

// handleIncomingRPC makes an authorization decision for incoming RPCs by
// calling out to an external authorization server.
//
// It performs the following steps:
//   - Determines if external authorization is enabled for this RPC based on the
//     `EnabledPercent` configuration.
//   - If enabled, it extracts relevant data from the incoming RPC and sends it
//     to the external authorization server.
//   - It processes the authorization server's response, allowing or denying the
//     RPC.
//
// It returns the following:
//   - a new context to be used for the rest of the RPC,
//   - a list of trailer mutations to be performed after invoking subsequent
//     interceptors in the chain.
//   - a non-nil error if the RPC needs to be denied, in which case, subsequent
//     interceptors in the chain are not invoked.
func (c *client) handleIncomingRPC(ss minimalServerStream) (context.Context, []*v3corepb.HeaderValueOption, error) {
	// Check if external authorization is enabled for this RPC.
	var enabled bool
	ep := c.cfg.EnabledPercent
	switch {
	case ep == 0:
		enabled = false
	case ep >= 100:
		enabled = true
	default:
		if randFunc(101) < ep {
			enabled = true
		}
	}

	// If external authorization is disabled for this RPC, the next step depends
	// on the value of the DenyAtDisabled config field. If this field is true,
	// the RPC will be failed with the status derived from the StatusCodeOnError
	// config field. Else, the RPC will be passed to the next interceptor
	// without modification.
	incomingCtx := ss.incomingContext()
	if !enabled {
		if c.cfg.DenyAtDisabled {
			return nil, nil, c.statusErrorf("extauthz: RPC denied due to external authorization being disabled")
		}

		if c.logger.V(2) {
			c.logger.Infof("RPC not enabled for external authorization allowed to proceed because DenyAtDisabled is false")
		}
		return incomingCtx, nil, nil
	}

	// Extract the relevant data from the incoming RPC context.
	data, err := newRPCData(incomingCtx, c.cfg.AllowedHeaders, c.cfg.DisallowedHeaders)
	if err != nil {
		return nil, nil, status.Errorf(codes.Internal, "extauthz: %v", err)
	}

	// Create a new context for the RPC to the external authorization server.
	// This context has a deadline that is the minimum of the incoming context's
	// deadline and the timeout specified in the config. It also contains the
	// incoming context's metadata, merged with the initial metadata specified
	// in the config.
	//
	// TODO(easwars): Ensure that passing the incoming context to the ext_authz
	// RPC propagates the trace context, so that the ext_authz RPC appears as a
	// child span on the data plane RPC's trace.
	var extAuthzCtx context.Context
	if c.cfg.Server.Timeout != 0 {
		var cancel context.CancelFunc
		extAuthzCtx, cancel = context.WithTimeout(incomingCtx, c.cfg.Server.Timeout)
		defer cancel()
	} else {
		extAuthzCtx = incomingCtx
	}
	incomingMD, _ := metadata.FromIncomingContext(incomingCtx)
	extAuthzCtx = metadata.NewOutgoingContext(extAuthzCtx, metadata.Join(c.cfg.Server.InitialMetadata, incomingMD))

	// Make the external authorization request.
	resp, err := c.callExtAuthzService(extAuthzCtx, data)
	if err != nil {
		if c.logger.V(2) {
			c.logger.Infof("RPC to external authorization server failed: %v", err)
		}

		// If the RPC to the ext_authz service fails and the failure_mode_allow
		// config field is set to false, the data plane RPC will be failed with
		// the status derived from the StatusCodeOnError config field.
		if !c.cfg.FailureModeAllow {
			return nil, nil, c.statusErrorf("extauthz: RPC denied due to error calling external authorization server: %v", err)
		}

		// Otherwise, the data plane RPC will be allowed. If the
		// failure_mode_allow_header_add config field is true, then the filter
		// will add a x-envoy-auth-failure-mode-allowed: true header to the data
		// plane RPC.
		if c.cfg.FailureModeAllowHeaderAdd {
			// incomingCtx is guaranteed to have metadata because we already
			// checked that in newRPCData.
			md, _ := metadata.FromIncomingContext(incomingCtx)
			md.Append("x-envoy-auth-failure-mode-allowed", "true")
			return metadata.NewIncomingContext(incomingCtx, md), nil, nil
		}
		return incomingCtx, nil, nil
	}

	// When the external authorization server denies the RPC, set the trailers
	// on the context so that they can be sent to the caller, and return a status
	// error based on the denied response.
	if st := status.FromProto(resp.GetStatus()); st.Code() != codes.OK {
		deniedResp, ok := resp.GetHttpResponse().(*v3authpb.CheckResponse_DeniedResponse)
		if !ok {
			// If the status in the respose is not OK, and the response does not
			// contain a DeniedResponse message, we just fail the RPC with
			// PERMISSION_DENIED.
			return nil, nil, status.Errorf(codes.PermissionDenied, "extauthz: RPC denied by external authorization server")
		}

		// Always returns a non-nil error.
		trailers, err := c.handleDeniedResponse(deniedResp.DeniedResponse)
		if trailers != nil {
			if err2 := ss.setTrailer(trailers); err2 != nil {
				// If setting the trailers fails, return an INTERNAL error.
				return nil, nil, fmt.Errorf("%v: %w", err, err2)
			}
		}
		return nil, nil, err
	}

	// We get here only if the external authorization server allowed the RPC.
	if _, ok := resp.GetHttpResponse().(*v3authpb.CheckResponse_OkResponse); !ok {
		// If the status in the respose is OK, and the response does not contain
		// an OkResponse message, we return without making any header or trailer
		// mutations.
		return incomingCtx, nil, nil
	}

	// Update incoming metadata with headers specified by the external
	// authorization server.
	allowedResp := resp.GetOkResponse()
	var updatedMD metadata.MD
	if headers := allowedResp.GetHeaders(); headers != nil {
		var err error
		updatedMD, err = c.cfg.DecoderHeaderMutationRules.ApplyAdditions(headers, incomingMD)
		if err != nil {
			return nil, nil, status.Errorf(codes.Unknown, "extauthz: error applying header mutation rules to OK response: %v", err)
		}
	} else {
		updatedMD = incomingMD
	}

	// Update incoming metadata with headers_to_remove specified by the external
	// authorization server.
	if headers := allowedResp.GetHeadersToRemove(); headers != nil {
		var err error
		updatedMD, err = c.cfg.DecoderHeaderMutationRules.ApplyRemovals(headers, updatedMD)
		if err != nil {
			return nil, nil, status.Errorf(codes.Unknown, "extauthz: error applying header mutation rules to OK response: %v", err)
		}
	}

	hvos := allowedResp.GetResponseHeadersToAdd()
	if hvos == nil {
		// No response headers (i.e., trailers) to be added, return the input
		// stream with the updated header metadata.
		return metadata.NewIncomingContext(incomingCtx, updatedMD), nil, nil
	}

	// Response headers were specified by the external authorization server.
	// Returning them from here will ensure that they get applied after running
	// subsequent interceptors in the chain.
	return metadata.NewIncomingContext(incomingCtx, updatedMD), hvos, nil
}

// wrappedUnaryServerStream is a wrapped stream for unary RPCs to support
// trailer mutations specified by the external authorization server.
type wrappedUnaryServerStream struct {
	grpc.ServerTransportStream
	trailerMDs []metadata.MD
}

// SetTrailer is overridden to capture any trailers set by subsequent
// interceptors in the chain and the server handler. This is eventually combined
// with trailers specified by the external authorization server.
func (s *wrappedUnaryServerStream) SetTrailer(md metadata.MD) error {
	s.trailerMDs = append(s.trailerMDs, md)
	return nil
}

// streamInterceptorForServer makes authorization decisions for incoming
// streaming RPCs by calling out to an external authorization server.
func (c *client) streamInterceptorForServer(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	incomingStream := &commonServerStream{
		streamingStream: ss,
		ctx:             ss.Context(),
	}
	ctx, trailerHVOs, err := c.handleIncomingRPC(incomingStream)
	if err != nil {
		return err
	}

	// Create a wrapped stream to handle trailers set by subsequent filters in
	// the chain before applying trailer mutations specified by the external
	// authorization server.
	wss := &wrappedStreamingServerStream{ServerStream: ss, ctx: ctx}
	err = handler(srv, wss)

	// Collect all trailers added by subsequent filters and apply mutations
	// specified by the external authorization server.
	mds := metadata.Join(wss.trailerMDs...)
	trailers, err2 := c.cfg.DecoderHeaderMutationRules.ApplyAdditions(trailerHVOs, mds)
	if err2 != nil {
		return status.Errorf(codes.Unknown, "extauthz: error applying header mutation rules to trailers: %v", err2)
	}
	ss.SetTrailer(trailers)
	return err
}

// wrappedStreamingServerStream is a wrapped stream for streaming RPCs to
// support trailer mutations specified by the external authorization server.
type wrappedStreamingServerStream struct {
	grpc.ServerStream
	ctx        context.Context
	trailerMDs []metadata.MD
}

// Context is overridden to ensure that any header mutations specified by the
// external authorization server are propagated to subsequent interceptors in
// the chain and the server handler.
func (s *wrappedStreamingServerStream) Context() context.Context {
	return s.ctx
}

// SetTrailer is overridden to capture any trailers set by subsequent
// interceptors in the chain and the server handler. This is eventually combined
// with trailers specified by the external authorization server.
func (s *wrappedStreamingServerStream) SetTrailer(md metadata.MD) {
	s.trailerMDs = append(s.trailerMDs, md)
}

// callExtAuthzService makes the RPC to the external authorization server.
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

// handleDeniedResponse processes a DeniedHttpResponse from the authorization
// server.
//
// It determines the gRPC status code to return to the client, which defaults
// to PERMISSION_DENIED but can be overridden by the authorization server's
// response. It also applies any header mutations specified in the response to
// generate trailers. It returns the trailers and a gRPC status error.
func (c *client) handleDeniedResponse(deniedResponse *v3authpb.DeniedHttpResponse) (trailers metadata.MD, err error) {
	// Compute the status to return to the caller based on the status returned
	// by the external authorization server.
	code := codes.PermissionDenied
	if st := deniedResponse.GetStatus(); st != nil {
		code = c.grpcStatusCode(int(st.GetCode()))
	}

	trailers, err = c.cfg.DecoderHeaderMutationRules.ApplyAdditions(deniedResponse.GetHeaders(), nil)
	if err != nil {
		return nil, status.Errorf(codes.Unknown, "extauthz: error applying header mutation rules to denied response: %v", err)
	}
	return trailers, status.Errorf(code, "extauthz: RPC denied by external authorization server")
}

// statusErrorf returns a grpc status error with a code derived from the
// StatusCodeOnError config field and a message constructed from the format
// specifier and args.
func (c *client) statusErrorf(format string, a ...any) error {
	return status.Errorf(c.grpcStatusCode(c.cfg.StatusCodeOnError), format, a...)
}

// grpcStatusCode converts an HTTP status code to a gRPC status code using the
// mapping defined in
// https://github.com/grpc/grpc/blob/master/doc/http-grpc-status-mapping.md
func (c *client) grpcStatusCode(httpStatus int) codes.Code {
	if code, ok := transport.HTTPStatusConvTab[httpStatus]; ok {
		return code
	}
	return codes.Unknown
}
