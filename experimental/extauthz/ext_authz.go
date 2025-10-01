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
	ctx, trailerAddFn, err := c.handleIncomingUnaryRPC(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := handler(ctx, req)

	// Add trailers specified by the external authorization server.
	if trailerAddFn != nil {
		err2 := trailerAddFn()
		if err2 != nil {
			return resp, fmt.Errorf("%v: %w", err2, err)
		}
	}
	return resp, err
}

// handleIncomingUnaryRPC makes an authorization decision for the incoming RPC
// by calling out to an external authorization server.
//
// It returns a new context to be used for the rest of the RPC, a function to
// be deferred to add trailers to the RPC response, and an error.
//
// If the authorization decision is to allow the RPC, the returned error is
// nil. The returned context may contain mutated headers as dictated by the
// authorization server's response. The returned function may be non-nil if the
// authorization server's response dictates trailer mutations.
//
// If the authorization decision is to deny the RPC, a non-nil error with the
// appropriate status code is returned.
//
// It performs the following steps:
//  1. Determines if external authorization is enabled for this RPC based on the
//     `EnabledPercent` configuration.
//  2. If enabled, it extracts relevant data from the incoming RPC and sends it
//     to the external authorization server.
//  3. It processes the authorization server's response, allowing or denying the
//     RPC, and applying any specified header or trailer mutations.
func (c *client) handleIncomingUnaryRPC(incomingCtx context.Context) (context.Context, func() error, error) {
	// Check if external authorization is enabled for this RPC.
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
			return incomingCtx, nil, c.statusErrorf("extauthz: RPC denied due to external authorization being disabled")
		}
		return incomingCtx, nil, nil
	}

	// Extract the relevant data from the incoming RPC context.
	data, err := newRPCData(incomingCtx, c.cfg.AllowedHeaders, c.cfg.DisallowedHeaders)
	if err != nil {
		return incomingCtx, nil, status.Errorf(codes.Internal, "extauthz: %v", err)
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
	ctx, cancel := context.WithTimeout(incomingCtx, c.cfg.Server.Timeout)
	defer cancel()
	incomingMD, _ := metadata.FromIncomingContext(ctx)
	ctx = metadata.NewOutgoingContext(ctx, metadata.Join(c.cfg.Server.InitialMetadata, incomingMD))

	// Make the external authorization request.
	resp, err := c.callExtAuthzService(ctx, data)
	if err != nil {
		// If the RPC to the ext_authz service fails and the failure_mode_allow
		// config field is set to false, the data plane RPC will be failed with
		// the status derived from the StatusCodeOnError config field.
		if !c.cfg.FailureModeAllow {
			return incomingCtx, nil, c.statusErrorf("extauthz: RPC denied due to error calling external authorization server: %v", err)
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
	// on the context so that they can be sent to the caller.
	if st := status.FromProto(resp.GetStatus()); st.Code() != codes.OK {
		deniedResp, ok := resp.GetHttpResponse().(*v3authpb.CheckResponse_DeniedResponse)
		if !ok {
			// If the status in the respose is not OK, and the response does not
			// contain a DeniedResponse message, we just fail the RPC with
			// PERMISSION_DENIED.
			return incomingCtx, nil, status.Errorf(codes.PermissionDenied, "extauthz: RPC denied by external authorization server")
		}
		trailers, err := c.handleDeniedResponse(deniedResp.DeniedResponse)
		ss := grpc.ServerTransportStreamFromContext(incomingCtx)
		if err2 := ss.SetTrailer(trailers); err2 != nil {
			// If setting the trailers fails, return an INTERNAL error.
			return incomingCtx, nil, fmt.Errorf("%v: %w", err, err2)
		}
		return incomingCtx, nil, err
	}

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
			return incomingCtx, nil, status.Errorf(codes.Internal, "extauthz: error applying header mutation rules to OK response: %v", err)
		}
	}

	// Update incoming metadata with headers_to_remove specified by the external
	// authorization server.
	if headers := allowedResp.GetHeadersToRemove(); headers != nil {
		var err error
		updatedMD, err = c.cfg.DecoderHeaderMutationRules.ApplyRemovals(headers, incomingMD)
		if err != nil {
			return incomingCtx, nil, status.Errorf(codes.Internal, "extauthz: error applying header mutation rules to OK response: %v", err)
		}
	}

	hvos := allowedResp.GetResponseHeadersToAdd()
	if hvos == nil {
		return metadata.NewIncomingContext(incomingCtx, updatedMD), nil, nil
	}
	ss := grpc.ServerTransportStreamFromContext(incomingCtx)
	ws := &wrappedUnaryServerStream{ServerTransportStream: ss}
	ctx = grpc.NewContextWithServerTransportStream(incomingCtx, ws)
	ctx = metadata.NewIncomingContext(ctx, updatedMD)
	addTrailer := func() error {
		mds := metadata.Join(ws.mds...)
		trailers, err := c.cfg.DecoderHeaderMutationRules.ApplyAdditions(hvos, mds)
		if err != nil {
			return status.Errorf(codes.Internal, "extauthz: error applying header mutation rules to trailers: %v", err)
		}
		if err2 := ss.SetTrailer(trailers); err2 != nil {
			// If setting the trailers fails, return an INTERNAL error.
			return err
		}
		return nil
	}
	return ctx, addTrailer, nil
}

// wrappedUnaryServerStream is a wrapped stream for unary RPCs to support
// addition of trailer metadata specified by the external authorization server.
type wrappedUnaryServerStream struct {
	grpc.ServerTransportStream
	mds []metadata.MD
}

// SetTrailer is overridden to capture any trailers set by filters after us in
// the chain and the server handler. This is eventually combined with trailers
// specified by the external authorization server.
func (s *wrappedUnaryServerStream) SetTrailer(md metadata.MD) error {
	s.mds = append(s.mds, md)
	return nil
}

// streamInterceptor is a server interceptor that makes authorization decisions
// for incoming streaming RPCs by calling the external authorization server.
func (c *client) streamInterceptor(any, grpc.ServerStream, *grpc.StreamServerInfo, grpc.StreamHandler) error {
	// TODO(easwars): Implement this.
	return nil
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
		return nil, status.Errorf(code, "extauthz: error applying header mutation rules to denied response: %v", err)
	}
	return trailers, status.Errorf(code, "extauthz: RPC denied by external authorization server")
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
