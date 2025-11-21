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

package extauthz_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/experimental/extauthz"
	"google.golang.org/grpc/internal"
	"google.golang.org/grpc/internal/grpctest"
	"google.golang.org/grpc/internal/stubserver"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	v3corepb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	v3authgrpc "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	v3authpb "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	v3typepb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	testgrpc "google.golang.org/grpc/interop/grpc_testing"
	testpb "google.golang.org/grpc/interop/grpc_testing"
)

const (
	defaultTestTimeout      = 10 * time.Second
	defaultTestShortTimeout = 10 * time.Millisecond
)

type s struct {
	grpctest.Tester
}

func Test(t *testing.T) {
	grpctest.RunSubTests(t, s{})
}

// extAuthzStubServer is an implementation of the AuthorizationServer interface
// for testing purposes, where the Check method behavior can be customized by
// the test.
type extAuthzStubServer struct {
	v3authgrpc.UnimplementedAuthorizationServer

	listener net.Listener
	checkF   func(context.Context, *v3authpb.CheckRequest) (*v3authpb.CheckResponse, error)
}

// Check is the stub implementation of the Check method.
func (s *extAuthzStubServer) Check(ctx context.Context, req *v3authpb.CheckRequest) (*v3authpb.CheckResponse, error) {
	if s.checkF != nil {
		return s.checkF(ctx, req)
	}
	return nil, status.Errorf(codes.Unimplemented, "method Check not implemented")
}

func (s *extAuthzStubServer) startServer(t *testing.T, opts ...grpc.ServerOption) error {
	if s.listener == nil {
		lis, err := net.Listen("tcp", "localhost:0")
		if err != nil {
			t.Fatalf("Failed to listen: %v", err)
		}
		s.listener = lis
	}
	grpcServer := grpc.NewServer(opts...)
	v3authgrpc.RegisterAuthorizationServer(grpcServer, s)
	go grpcServer.Serve(s.listener)
	t.Logf("Started ext authz server at: %v", s.listener.Addr().String())
	t.Cleanup(func() { s.listener.Close() })
	return nil
}

// startTestServiceBackend starts a test service backend that implements a unary
// and streaming RPC.
//
// It returns the newly created test service backend and a couple of channels on
// which it writes incoming metadata received by the unary and streaming rpc
// handlers respectively, for the test to inspect.
func startTestServiceBackend(t *testing.T, opts ...grpc.ServerOption) (*stubserver.StubServer, chan metadata.MD, chan metadata.MD) {
	t.Helper()

	gotUnaryMD := make(chan metadata.MD, 1)
	gotStreamingMD := make(chan metadata.MD, 1)
	ss := &stubserver.StubServer{
		EmptyCallF: func(ctx context.Context, _ *testpb.Empty) (*testpb.Empty, error) {
			md, _ := metadata.FromIncomingContext(ctx)
			select {
			case gotUnaryMD <- md:
			case <-ctx.Done():
				t.Error("Timeout when attempting to write incoming metadata to channel")
			}

			return &testpb.Empty{}, nil
		},
		FullDuplexCallF: func(stream testgrpc.TestService_FullDuplexCallServer) error {
			ctx := stream.Context()
			md, _ := metadata.FromIncomingContext(ctx)
			select {
			case gotStreamingMD <- md:
			case <-ctx.Done():
				t.Error("Timeout when attempting to write incoming metadata to channel")
			}

			for {
				_, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return err
				}
			}
		},
	}
	backend := stubserver.StartTestService(t, ss, opts...)
	t.Cleanup(backend.Stop)

	return backend, gotUnaryMD, gotStreamingMD
}

// makeUnaryRPC is a helper function to make a unary RPC and verifiy the
// returned status code, trailers and headers (when headersCh is non-nil.)
func makeUnaryRPC(ctx context.Context, t *testing.T, cc *grpc.ClientConn, wantStatus codes.Code, wantTrailers metadata.MD, headersCh chan metadata.MD, wantHeaders metadata.MD) {
	t.Helper()
	t.Run("EmptyCall", func(t *testing.T) {
		client := testgrpc.NewTestServiceClient(cc)
		var gotTrailers metadata.MD
		_, err := client.EmptyCall(ctx, &testgrpc.Empty{}, grpc.Trailer(&gotTrailers))
		if status.Code(err) != wantStatus {
			t.Fatalf("EmptyCall RPC returned status code: %s, want: %s", status.Code(err), wantStatus)
		}

		if err := compareMetadata(gotTrailers, wantTrailers); err != nil {
			t.Fatalf("Unexpected trailer metadata received by the client: %v", err)
		}

		// Compare headers only when for RPCs with an OK status code.
		if status.Code(err) == codes.OK && headersCh != nil {
			select {
			case gotMD := <-headersCh:
				if err := compareMetadata(gotMD, wantHeaders); err != nil {
					t.Fatalf("Unexpected header metadata received by the server handler: %v", err)
				}
			case <-ctx.Done():
				t.Fatalf("Timeout when attempting to read incoming metadata received by the server handler")
			}
		}
	})
}

func makeStreamingRPC(ctx context.Context, t *testing.T, cc *grpc.ClientConn, wantStatus codes.Code, wantTrailers metadata.MD, headersCh chan metadata.MD, wantHeaders metadata.MD) {
	t.Helper()
	t.Run("FullDuplexCall", func(t *testing.T) {
		client := testgrpc.NewTestServiceClient(cc)
		var gotTrailers metadata.MD
		stream, err := client.FullDuplexCall(ctx, grpc.Trailer(&gotTrailers))
		if err != nil {
			t.Fatalf("Failed to start streaming RPC: %v", err)
		}

		// Send one empty message and close the sending side. Ignore the errors
		// from the send and close, and rely on errors returned from recv.
		stream.Send(&testpb.StreamingOutputCallRequest{})
		stream.CloseSend()
		_, err = stream.Recv()
		if errors.Is(err, io.EOF) {
			err = status.Error(codes.OK, "")
		}
		if status.Code(err) != wantStatus {
			t.Fatalf("FullDuplexCall RPC returned error: %v, want: %s", err, wantStatus)
		}

		if err := compareMetadata(gotTrailers, wantTrailers); err != nil {
			t.Errorf("Unexpected trailer metadata received by the client: %v", err)
		}

		// Compare headers only when for RPCs with an OK status code.
		if status.Code(err) == codes.OK && headersCh != nil {
			select {
			case gotMD := <-headersCh:
				if err := compareMetadata(gotMD, wantHeaders); err != nil {
					t.Fatalf("Unexpected header metadata received by the server handler: %v", err)
				}
			case <-ctx.Done():
				t.Fatalf("Timeout when attempting to read incoming metadata received by the server handler")
			}
		}
	})
}

// Tests cases where external authorization is not enabled.
func (s) TestExtAuthz_NotEnabled(t *testing.T) {
	tests := []struct {
		name       string
		config     *extauthz.InterceptorConfig
		wantStatus codes.Code
	}{
		{
			name: "DenyAtDisabled_False",
			config: &extauthz.InterceptorConfig{
				EnabledPercent: 0,     // External authorization not enabled.
				DenyAtDisabled: false, // RPCs should be allowed when not enabled.
			},
			wantStatus: codes.OK,
		},
		{
			name: "DenyAtDisabledTrue_StatusCodeOnError_Default",
			config: &extauthz.InterceptorConfig{
				EnabledPercent:    0,    // External authorization not enabled.
				DenyAtDisabled:    true, // RPCs should be denied when not enabled.
				StatusCodeOnError: 0,    // Use default status code (403 Forbidden).
			},
			wantStatus: codes.PermissionDenied,
		},
		{
			name: "DenyAtDisabledTrue_StatusCodeOnError_Unauthorized",
			config: &extauthz.InterceptorConfig{
				EnabledPercent:    0,                       // External authorization not enabled.
				DenyAtDisabled:    true,                    // RPCs should be denied when not enabled.
				StatusCodeOnError: http.StatusUnauthorized, // Unauthorized (401) translates to codes.Unauthenticated.
			},
			wantStatus: codes.Unauthenticated,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
			defer cancel()

			// Start a test ext_authz server and modify the test's interceptor
			// config to point to this ext_authz server.
			extAuthServer := &extAuthzStubServer{}
			extAuthServer.startServer(t)
			test.config.Server = extauthz.ServerConfig{
				TargetURI:          "dns:///" + extAuthServer.listener.Addr().String(),
				ChannelCredentials: insecure.NewCredentials(),
			}

			// Start a test backend with the ext_authz server option.
			extAuthzServerOption, done := extauthz.ServerOption(test.config)
			defer done()
			backend, _, _ := startTestServiceBackend(t, extAuthzServerOption)

			// Create a grpc client to the above backend server.
			cc, err := grpc.NewClient(backend.Address, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatalf("Failed to create client: %v", err)
			}
			defer cc.Close()
			makeUnaryRPC(ctx, t, cc, test.wantStatus, nil, nil, nil)
			makeStreamingRPC(ctx, t, cc, test.wantStatus, nil, nil, nil)
		})
	}
}

// Tests cases where the external authorization RPC times out because the
// configured server timeout is very low.
func (s) TestExtAuthz_AuthzRPC_Timeout(t *testing.T) {
	tests := []struct {
		name        string
		config      *extauthz.InterceptorConfig
		wantHeaders metadata.MD // Expected incoming metadata at the server handler.
		wantStatus  codes.Code  // Expected status code for the data plane RPC.
	}{
		{
			name: "FailureModeAllow_False_StatusCodeOnError_Default",
			config: &extauthz.InterceptorConfig{
				EnabledPercent:    100,   // External authorization is always enabled.
				FailureModeAllow:  false, // Data plane RPC to be denied.
				StatusCodeOnError: 0,     // Use default status code (403 Forbidden).
			},
			wantStatus: codes.PermissionDenied,
		},
		{
			name: "FailureModeAllow_False_StatusCodeOnError_Unauthorized",
			config: &extauthz.InterceptorConfig{
				EnabledPercent:    100,                     // External authorization is always enabled.
				FailureModeAllow:  false,                   // Data plane RPC to be denied.
				StatusCodeOnError: http.StatusUnauthorized, // Unauthorized (401) translates to codes.Unauthenticated.
			},
			wantStatus: codes.Unauthenticated,
		},
		{
			name: "FailureModeAllow_True",
			config: &extauthz.InterceptorConfig{
				EnabledPercent:   100,  // External authorization is always enabled.
				FailureModeAllow: true, // Data plane RPC to be allowed.
			},
			wantHeaders: metadata.MD{},
			wantStatus:  codes.OK,
		},
		{
			name: "FailureModeAllow_True_HeaderAdd",
			config: &extauthz.InterceptorConfig{
				EnabledPercent:            100,  // External authorization is always enabled.
				FailureModeAllow:          true, // Data plane RPC to be allowed.
				FailureModeAllowHeaderAdd: true, // Add the "x-envoy-auth-failure-mode-allowed:true" header.
			},
			wantHeaders: metadata.Pairs("x-envoy-auth-failure-mode-allowed", "true"),
			wantStatus:  codes.OK,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
			defer cancel()

			// Start a test ext_authz server and modify the test's interceptor
			// config to point to this ext_authz server.
			extAuthServer := &extAuthzStubServer{
				checkF: func(ctx context.Context, _ *v3authpb.CheckRequest) (*v3authpb.CheckResponse, error) {
					// Wait until the test context is done to simulate a timeout on the
					// external authorization RPC.
					<-ctx.Done()

					return &v3authpb.CheckResponse{Status: &spb.Status{Code: int32(codes.OK)}}, nil
				},
			}
			extAuthServer.startServer(t)
			test.config.Server = extauthz.ServerConfig{
				TargetURI:          "dns:///" + extAuthServer.listener.Addr().String(),
				ChannelCredentials: insecure.NewCredentials(),
				Timeout:            defaultTestShortTimeout, // A very short timeout for the ext_authz RPC
			}

			// Start a test backend with the ext_authz server option.
			extAuthzServerOption, done := extauthz.ServerOption(test.config)
			defer done()
			backend, unaryCh, streamingCh := startTestServiceBackend(t, extAuthzServerOption)

			// Set the :authority header expected at the server handler. This is
			// added by gRPC and not by the ext_authz interceptor.
			if test.wantHeaders != nil {
				test.wantHeaders.Append(":authority", backend.Address)
			}

			// Create a grpc client to the above backend server.
			cc, err := grpc.NewClient(backend.Address, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatalf("Failed to create client: %v", err)
			}
			defer cc.Close()

			makeUnaryRPC(ctx, t, cc, test.wantStatus, nil, unaryCh, test.wantHeaders)
			makeStreamingRPC(ctx, t, cc, test.wantStatus, nil, streamingCh, test.wantHeaders)
		})
	}
}

// Tests cases where the external authorization RPC fails.
func (s) TestExtAuthz_AuthzRPC_Failure(t *testing.T) {
	tests := []struct {
		name        string
		config      *extauthz.InterceptorConfig
		wantHeaders metadata.MD // Expected incoming metadata at the server handler.
		wantStatus  codes.Code  // Expected status code for the data plane RPC.
	}{
		{
			name: "FailureModeAllow_False_StatusCodeOnError_Default",
			config: &extauthz.InterceptorConfig{
				EnabledPercent:    100,   // External authorization is always enabled.
				FailureModeAllow:  false, // Data plane RPC to be denied.
				StatusCodeOnError: 0,     // Use default status code (403 Forbidden).
			},
			wantStatus: codes.PermissionDenied,
		},
		{
			name: "FailureModeAllow_False_StatusCodeOnError_Unauthorized",
			config: &extauthz.InterceptorConfig{
				EnabledPercent:    100,                     // External authorization is always enabled.
				FailureModeAllow:  false,                   // Data plane RPC to be denied.
				StatusCodeOnError: http.StatusUnauthorized, // Unauthorized (401) translates to codes.Unauthenticated.
			},
			wantStatus: codes.Unauthenticated,
		},
		{
			name: "FailureModeAllow_True",
			config: &extauthz.InterceptorConfig{
				EnabledPercent:   100,  // External authorization is always enabled.
				FailureModeAllow: true, // Data plane RPC to be allowed.
			},
			wantHeaders: metadata.MD{},
			wantStatus:  codes.OK,
		},
		{
			name: "FailureModeAllow_True_HeaderAdd",
			config: &extauthz.InterceptorConfig{
				EnabledPercent:            100,  // External authorization is always enabled.
				FailureModeAllow:          true, // Data plane RPC to be allowed.
				FailureModeAllowHeaderAdd: true, // Add the "x-envoy-auth-failure-mode-allowed:true" header.
			},
			wantHeaders: metadata.Pairs("x-envoy-auth-failure-mode-allowed", "true"),
			wantStatus:  codes.OK,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
			defer cancel()

			// Start a test ext_authz server and modify the test's interceptor
			// config to point to this ext_authz server.
			extAuthServer := &extAuthzStubServer{
				checkF: func(context.Context, *v3authpb.CheckRequest) (*v3authpb.CheckResponse, error) {
					return nil, status.Error(codes.Internal, "internal server error")
				},
			}
			extAuthServer.startServer(t)
			test.config.Server = extauthz.ServerConfig{
				TargetURI:          "dns:///" + extAuthServer.listener.Addr().String(),
				ChannelCredentials: insecure.NewCredentials(),
			}

			// Start a test backend with the ext_authz server option.
			extAuthzServerOption, done := extauthz.ServerOption(test.config)
			defer done()
			backend, unaryCh, streamingCh := startTestServiceBackend(t, extAuthzServerOption)

			// Set the :authority header expected at the server handler. This is
			// added by gRPC and not by the ext_authz interceptor.
			if test.wantHeaders != nil {
				test.wantHeaders.Append(":authority", backend.Address)
			}

			// Create a grpc client to the above backend server.
			cc, err := grpc.NewClient(backend.Address, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatalf("Failed to create client: %v", err)
			}
			defer cc.Close()

			makeUnaryRPC(ctx, t, cc, test.wantStatus, nil, unaryCh, test.wantHeaders)
			makeStreamingRPC(ctx, t, cc, test.wantStatus, nil, streamingCh, test.wantHeaders)
		})
	}
}

// Tests cases where the ext_authz server denies the data plane RPC.
func (s) TestExtAuthz_Denied(t *testing.T) {
	tests := []struct {
		name                string
		headerMutationRules extauthz.HeaderMutationRules                                                   // Header mutation rules to be included as part of the config.
		checkFunc           func(context.Context, *v3authpb.CheckRequest) (*v3authpb.CheckResponse, error) // ext_authz server functionality.
		wantTrailers        metadata.MD                                                                    // Expected trailers received by the client.
		wantStatus          codes.Code                                                                     // Expected status code for the data plane RPC.
	}{
		{
			name: "NoDeniedResponse",
			checkFunc: func(context.Context, *v3authpb.CheckRequest) (*v3authpb.CheckResponse, error) {
				st := &spb.Status{Code: int32(codes.PermissionDenied)}
				return &v3authpb.CheckResponse{Status: st}, nil
			},
			wantStatus: codes.PermissionDenied,
		},
		{
			name: "WithDeniedResponse",
			checkFunc: func(context.Context, *v3authpb.CheckRequest) (*v3authpb.CheckResponse, error) {
				return &v3authpb.CheckResponse{
					Status: &spb.Status{Code: int32(codes.PermissionDenied)},
					HttpResponse: &v3authpb.CheckResponse_DeniedResponse{
						DeniedResponse: &v3authpb.DeniedHttpResponse{
							// Unauthorized (401) translates to codes.Unauthenticated.
							Status: &v3typepb.HttpStatus{Code: v3typepb.StatusCode_Unauthorized},
						},
					},
				}, nil
			},
			wantStatus: codes.Unauthenticated,
		},
		{
			name: "WithDeniedResponse_AndTrailers",
			headerMutationRules: extauthz.HeaderMutationRules{
				AllowExpr:       regexp.MustCompile("^k.*"),
				DisallowExpr:    regexp.MustCompile("^a1$"),
				DisallowIsError: false, // Disllowed header mutations are silently ignored.
			},
			checkFunc: func(context.Context, *v3authpb.CheckRequest) (*v3authpb.CheckResponse, error) {
				return &v3authpb.CheckResponse{
					Status: &spb.Status{Code: int32(codes.PermissionDenied)},
					HttpResponse: &v3authpb.CheckResponse_DeniedResponse{
						DeniedResponse: &v3authpb.DeniedHttpResponse{
							Headers: []*v3corepb.HeaderValueOption{
								{Header: &v3corepb.HeaderValue{Key: "k1", Value: "v1"}},                                   // Allowed.
								{Header: &v3corepb.HeaderValue{Key: "k2-bin", Value: "v2", RawValue: []byte{0, 1, 2, 3}}}, // Allowed binary header.
								{Header: &v3corepb.HeaderValue{Key: "a1", Value: "v1"}},                                   // Disallowed.
							},
						},
					},
				}, nil
			},
			wantTrailers: metadata.Pairs("k1", "v1", "k2-bin", "\x00\x01\x02\x03"),
			wantStatus:   codes.PermissionDenied,
		},
		{
			name: "WithDeniedResponse_AndTrailers_MutationFails",
			headerMutationRules: extauthz.HeaderMutationRules{
				AllowExpr:       regexp.MustCompile("^k.*"),
				DisallowExpr:    regexp.MustCompile("^a1$"),
				DisallowIsError: true, // Disallowed header mutations result in RPC failures.
			},
			checkFunc: func(context.Context, *v3authpb.CheckRequest) (*v3authpb.CheckResponse, error) {
				return &v3authpb.CheckResponse{
					Status: &spb.Status{Code: int32(codes.PermissionDenied)},
					HttpResponse: &v3authpb.CheckResponse_DeniedResponse{
						DeniedResponse: &v3authpb.DeniedHttpResponse{
							Headers: []*v3corepb.HeaderValueOption{
								{Header: &v3corepb.HeaderValue{Key: "k1", Value: "v1"}},                                   // Allowed.
								{Header: &v3corepb.HeaderValue{Key: "k2-bin", Value: "v2", RawValue: []byte{0, 1, 2, 3}}}, // Allowed binary header.
								{Header: &v3corepb.HeaderValue{Key: "a1", Value: "v1"}},                                   // Disallowed.
							},
						},
					},
				}, nil
			},
			wantStatus: codes.Unknown,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
			defer cancel()

			// Start a test ext_authz server whose functionality is specified by
			// the test table.
			extAuthServer := &extAuthzStubServer{checkF: test.checkFunc}
			extAuthServer.startServer(t)

			// Start a test backend with the ext_authz server option. Only the
			// header mutation rules are specified by the test table. The rest
			// of the config is the same for all subtests.
			config := &extauthz.InterceptorConfig{
				Server: extauthz.ServerConfig{
					TargetURI:          "dns:///" + extAuthServer.listener.Addr().String(),
					ChannelCredentials: insecure.NewCredentials(),
				},
				EnabledPercent:             100, // External authorization is always enabled.
				DecoderHeaderMutationRules: test.headerMutationRules,
			}
			extAuthzServerOption, done := extauthz.ServerOption(config)
			defer done()

			// Also include a test interceptor in the chain to ensure that
			// subsequent interceptors in the chain are not invoked when the
			// ext_authz server denies the RPC.
			testInterceptor := newTestInterceptor()
			backend, _, _ := startTestServiceBackend(t, extAuthzServerOption, testInterceptor.serverOption())

			// Create a grpc client to the above backend server.
			cc, err := grpc.NewClient(backend.Address, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatalf("Failed to create client: %v", err)
			}
			defer cc.Close()

			makeUnaryRPC(ctx, t, cc, test.wantStatus, test.wantTrailers, nil, nil)
			sCtx, sCancel := context.WithTimeout(ctx, defaultTestShortTimeout)
			defer sCancel()
			select {
			case <-testInterceptor.unaryCalled:
				t.Fatal("Subsequent interceptor in chain called when it should not have been called")
			case <-sCtx.Done():
			}

			makeStreamingRPC(ctx, t, cc, test.wantStatus, test.wantTrailers, nil, nil)
			sCtx, sCancel = context.WithTimeout(ctx, defaultTestShortTimeout)
			defer sCancel()
			select {
			case <-testInterceptor.streamingCalled:
				t.Fatal("Subsequent interceptor in chain called when it should not have been called")
			case <-sCtx.Done():
			}
		})
	}
}

// Tests the case where the ext_authz server allows the data plane RPC, but does
// not specify any headers or trailers to add. Verifies that the RPC is sent to
// the next interceptor in the chain without modification and that it eventually
// succeeds.
func (s) TestExtAuthz_Allowed_NoHTTPResponse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	extAuthServer := &extAuthzStubServer{
		checkF: func(context.Context, *v3authpb.CheckRequest) (*v3authpb.CheckResponse, error) {
			st := &spb.Status{Code: int32(codes.OK)}
			return &v3authpb.CheckResponse{Status: st}, nil
		},
	}
	extAuthServer.startServer(t)

	// Start a test backend with the ext_authz server option.
	config := &extauthz.InterceptorConfig{
		Server: extauthz.ServerConfig{
			TargetURI:          "dns:///" + extAuthServer.listener.Addr().String(),
			ChannelCredentials: insecure.NewCredentials(),
		},
		EnabledPercent: 100, // External authorization is always enabled.
	}
	extAuthzServerOption, done := extauthz.ServerOption(config)
	defer done()

	// Also include a test interceptor in the chain to verify if
	// subsequent interceptors in the chain are invoked.
	testInterceptor := newTestInterceptor()
	backend, unaryCh, streamingCh := startTestServiceBackend(t, extAuthzServerOption, testInterceptor.serverOption())

	// Create a grpc client to the above backend server.
	cc, err := grpc.NewClient(backend.Address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	defer cc.Close()

	// Verify that the next interceptor sees the expected headers. The
	// :authority header is added by gRPC and not by the ext_authz interceptor.
	wantHeaders := metadata.Pairs(":authority", backend.Address)
	makeUnaryRPC(ctx, t, cc, codes.OK, nil, unaryCh, wantHeaders)
	makeStreamingRPC(ctx, t, cc, codes.OK, nil, streamingCh, wantHeaders)
}

// Tests the case where the ext_authz server allows the data plane RPC, and
// specifies headers to be added and removed from the data plane RPC. Verifies
// that the RPC is sent to the next interceptor in the chain with the expected
// headers and that it eventually succeeds.
func (s) TestExtAuthz_Allowed_WithHeaders(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	// Start a test ext_authz server that allows the data plane RPC.
	extAuthServer := &extAuthzStubServer{
		checkF: func(context.Context, *v3authpb.CheckRequest) (*v3authpb.CheckResponse, error) {
			st := &spb.Status{Code: int32(codes.OK)}
			return &v3authpb.CheckResponse{
				Status: st,
				HttpResponse: &v3authpb.CheckResponse_OkResponse{
					OkResponse: &v3authpb.OkHttpResponse{
						Headers: []*v3corepb.HeaderValueOption{
							{Header: &v3corepb.HeaderValue{Key: "k1", Value: "v1"}},                                   // Allowed.
							{Header: &v3corepb.HeaderValue{Key: "k2-bin", Value: "v2", RawValue: []byte{0, 1, 2, 3}}}, // Allowed binary header.
							{Header: &v3corepb.HeaderValue{Key: "a1", Value: "v1"}},                                   // Disallowed.
						},
						HeadersToRemove: []string{":authority", "k-test-header-to-be-removed"},
					},
				},
			}, nil
		},
	}
	extAuthServer.startServer(t)

	// Start a test backend with the ext_authz server option.
	config := &extauthz.InterceptorConfig{
		Server: extauthz.ServerConfig{
			TargetURI:          "dns:///" + extAuthServer.listener.Addr().String(),
			ChannelCredentials: insecure.NewCredentials(),
		},
		EnabledPercent: 100, // External authorization is always enabled.
		DecoderHeaderMutationRules: extauthz.HeaderMutationRules{
			AllowExpr:       regexp.MustCompile("^k.*"),
			DisallowExpr:    regexp.MustCompile("^a1$"),
			DisallowIsError: false, // Disllowed header mutations are silently ignored.
		},
	}
	extAuthzServerOption, done := extauthz.ServerOption(config)
	defer done()

	// Also include a test interceptor in the chain to verify if
	// subsequent interceptors in the chain are invoked.
	testInterceptor := newTestInterceptor()
	backend, unaryCh, streamingCh := startTestServiceBackend(t, extAuthzServerOption, testInterceptor.serverOption())

	// Create a grpc client to the above backend server.
	cc, err := grpc.NewClient(backend.Address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	defer cc.Close()

	// Verify that the next interceptor sees the expected headers. The
	// :authority header is added by gRPC and not by the ext_authz interceptor.
	wantHeaders := metadata.Pairs(":authority", backend.Address, "k1", "v1", "k2-bin", "\x00\x01\x02\x03")
	outgoingCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("k-test-header-to-be-removed", "true"))
	makeUnaryRPC(outgoingCtx, t, cc, codes.OK, nil, unaryCh, wantHeaders)
	makeStreamingRPC(outgoingCtx, t, cc, codes.OK, nil, streamingCh, wantHeaders)
}

// Tests the case where the ext_authz server allows the data plane RPC, and
// specifies headers to be added and removed from the data plane RPC. One of the
// specified header mutations is not allowed by the configuration. Verifies that
// the RPC is failed with code Unknown. Also verifies that the next interceptor
// in the chain is not called.
func (s) TestExtAuthz_Allowed_WithHeaders_MutationFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	// Start a test ext_authz server that allows the data plane RPC.
	extAuthServer := &extAuthzStubServer{
		checkF: func(context.Context, *v3authpb.CheckRequest) (*v3authpb.CheckResponse, error) {
			st := &spb.Status{Code: int32(codes.OK)}
			return &v3authpb.CheckResponse{
				Status: st,
				HttpResponse: &v3authpb.CheckResponse_OkResponse{
					OkResponse: &v3authpb.OkHttpResponse{
						Headers: []*v3corepb.HeaderValueOption{
							{Header: &v3corepb.HeaderValue{Key: "k1", Value: "v1"}},                                   // Allowed.
							{Header: &v3corepb.HeaderValue{Key: "k2-bin", Value: "v2", RawValue: []byte{0, 1, 2, 3}}}, // Allowed binary header.
							{Header: &v3corepb.HeaderValue{Key: "a1", Value: "v1"}},                                   // Disallowed.
						},
						HeadersToRemove: []string{":authority", "k-test-header-to-be-removed"},
					},
				},
			}, nil
		},
	}
	extAuthServer.startServer(t)

	// Start a test backend with the ext_authz server option.
	config := &extauthz.InterceptorConfig{
		Server: extauthz.ServerConfig{
			TargetURI:          "dns:///" + extAuthServer.listener.Addr().String(),
			ChannelCredentials: insecure.NewCredentials(),
		},
		EnabledPercent: 100, // External authorization is always enabled.
		DecoderHeaderMutationRules: extauthz.HeaderMutationRules{
			AllowExpr:       regexp.MustCompile("^k.*"),
			DisallowExpr:    regexp.MustCompile("^a1$"),
			DisallowIsError: true, // Disallowed header mutations result in RPC failures.
		},
	}
	extAuthzServerOption, done := extauthz.ServerOption(config)
	defer done()

	// Also include a test interceptor in the chain to verify if
	// subsequent interceptors in the chain are invoked.
	testInterceptor := newTestInterceptor()
	backend, _, _ := startTestServiceBackend(t, extAuthzServerOption, testInterceptor.serverOption())

	// Create a grpc client to the above backend server.
	cc, err := grpc.NewClient(backend.Address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	defer cc.Close()

	outgoingCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("k-test-header-to-be-removed", "true"))
	makeUnaryRPC(outgoingCtx, t, cc, codes.Unknown, nil, nil, nil)
	sCtx, sCancel := context.WithTimeout(ctx, defaultTestShortTimeout)
	defer sCancel()
	select {
	case <-testInterceptor.unaryCalled:
		t.Fatal("Subsequent interceptor in chain called when it should not have been called")
	case <-sCtx.Done():
	}

	makeStreamingRPC(outgoingCtx, t, cc, codes.Unknown, nil, nil, nil)
	sCtx, sCancel = context.WithTimeout(ctx, defaultTestShortTimeout)
	defer sCancel()
	select {
	case <-testInterceptor.streamingCalled:
		t.Fatal("Subsequent interceptor in chain called when it should not have been called")
	case <-sCtx.Done():
	}
}

// Tests the case where the ext_authz server allows the data plane RPC, and
// specifies trailers to be added and removed from the data plane RPC. Verifies
// that data plane RPC succeeds with expected trailers.
func (s) TestExtAuthz_Allowed_WithTrailers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	// Start a test ext_authz server that allows the data plane RPC.
	extAuthServer := &extAuthzStubServer{
		checkF: func(context.Context, *v3authpb.CheckRequest) (*v3authpb.CheckResponse, error) {
			st := &spb.Status{Code: int32(codes.OK)}
			return &v3authpb.CheckResponse{
				Status: st,
				HttpResponse: &v3authpb.CheckResponse_OkResponse{
					OkResponse: &v3authpb.OkHttpResponse{
						ResponseHeadersToAdd: []*v3corepb.HeaderValueOption{
							{Header: &v3corepb.HeaderValue{Key: "k1", Value: "v1"}},                                   // Allowed.
							{Header: &v3corepb.HeaderValue{Key: "k2-bin", Value: "v2", RawValue: []byte{0, 1, 2, 3}}}, // Allowed binary header.
							{Header: &v3corepb.HeaderValue{Key: "a1", Value: "v1"}},                                   // Disallowed.
						},
						HeadersToRemove: []string{":authority", "k-test-header-to-be-removed"},
					},
				},
			}, nil
		},
	}
	extAuthServer.startServer(t)

	// Start a test backend with the ext_authz server option.
	config := &extauthz.InterceptorConfig{
		Server: extauthz.ServerConfig{
			TargetURI:          "dns:///" + extAuthServer.listener.Addr().String(),
			ChannelCredentials: insecure.NewCredentials(),
		},
		EnabledPercent: 100, // External authorization is always enabled.
		DecoderHeaderMutationRules: extauthz.HeaderMutationRules{
			AllowExpr:       regexp.MustCompile("^k.*"),
			DisallowExpr:    regexp.MustCompile("^a1$"),
			DisallowIsError: false, // Disllowed header mutations are silently ignored.
		},
	}
	extAuthzServerOption, done := extauthz.ServerOption(config)
	defer done()

	// Also include a test interceptor in the chain to verify if subsequent
	// interceptors in the chain are invoked. This interceptor also adds a
	// trailer.
	testInterceptor := newTestInterceptor("test-trailer-key", "test-trailer-value", "k1", "test-trailer-v1")
	backend, unaryCh, streamingCh := startTestServiceBackend(t, extAuthzServerOption, testInterceptor.serverOption())

	// Create a grpc client to the above backend server.
	cc, err := grpc.NewClient(backend.Address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	defer cc.Close()

	wantTrailers := metadata.Pairs(
		"test-trailer-key", "test-trailer-value",
		"k1", "test-trailer-v1",
		"k1", "v1",
		"k2-bin", "\x00\x01\x02\x03",
	)
	// Verify that the next interceptor sees the expected headers. The
	// :authority header is added by gRPC and not by the ext_authz interceptor.
	wantHeaders := metadata.Pairs(":authority", backend.Address)
	outgoingCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("k-test-header-to-be-removed", "true"))
	makeUnaryRPC(outgoingCtx, t, cc, codes.OK, wantTrailers, unaryCh, wantHeaders)
	makeStreamingRPC(outgoingCtx, t, cc, codes.OK, wantTrailers, streamingCh, wantHeaders)
}

// Tests the case where the ext_authz server allows the data plane RPC, and
// specifies trailers to be added and removed from the data plane RPC. One of
// the trailer mutations is not allowed by the configuration. Verifies that
// data plane RPC fails with code Unknown.
func (s) TestExtAuthz_Allowed_WithTrailers_MutationFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	// Start a test ext_authz server that allows the data plane RPC.
	extAuthServer := &extAuthzStubServer{
		checkF: func(context.Context, *v3authpb.CheckRequest) (*v3authpb.CheckResponse, error) {
			st := &spb.Status{Code: int32(codes.OK)}
			return &v3authpb.CheckResponse{
				Status: st,
				HttpResponse: &v3authpb.CheckResponse_OkResponse{
					OkResponse: &v3authpb.OkHttpResponse{
						ResponseHeadersToAdd: []*v3corepb.HeaderValueOption{
							{Header: &v3corepb.HeaderValue{Key: "k1", Value: "v1"}},                                   // Allowed.
							{Header: &v3corepb.HeaderValue{Key: "k2-bin", Value: "v2", RawValue: []byte{0, 1, 2, 3}}}, // Allowed binary header.
							{Header: &v3corepb.HeaderValue{Key: "a1", Value: "v1"}},                                   // Disallowed.
						},
						HeadersToRemove: []string{":authority", "k-test-header-to-be-removed"},
					},
				},
			}, nil
		},
	}
	extAuthServer.startServer(t)

	// Start a test backend with the ext_authz server option.
	config := &extauthz.InterceptorConfig{
		Server: extauthz.ServerConfig{
			TargetURI:          "dns:///" + extAuthServer.listener.Addr().String(),
			ChannelCredentials: insecure.NewCredentials(),
		},
		EnabledPercent: 100, // External authorization is always enabled.
		DecoderHeaderMutationRules: extauthz.HeaderMutationRules{
			AllowExpr:       regexp.MustCompile("^k.*"),
			DisallowExpr:    regexp.MustCompile("^a1$"),
			DisallowIsError: true, // Disallowed header mutations result in RPC failures.
		},
	}
	extAuthzServerOption, done := extauthz.ServerOption(config)
	defer done()

	// Also include a test interceptor in the chain to verify if subsequent
	// interceptors in the chain are invoked. This interceptor also adds a
	// trailer.
	testInterceptor := newTestInterceptor()
	backend, unaryCh, streamingCh := startTestServiceBackend(t, extAuthzServerOption, testInterceptor.serverOption())

	// Create a grpc client to the above backend server.
	cc, err := grpc.NewClient(backend.Address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	defer cc.Close()

	// Verify that the next interceptor sees the expected headers. The
	// :authority header is added by gRPC and not by the ext_authz interceptor.
	wantHeaders := metadata.Pairs(":authority", backend.Address)
	outgoingCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("k-test-header-to-be-removed", "true"))
	makeUnaryRPC(outgoingCtx, t, cc, codes.Unknown, nil, unaryCh, wantHeaders)
	makeStreamingRPC(outgoingCtx, t, cc, codes.Unknown, nil, streamingCh, wantHeaders)
}

// Tests the case where the ext_authz server allows the data plane RPC, and
// specified both header and trailer mutations that are expected to succeed.
// Verifies that the data plane RPC succeeds.
func (s) TestExtAuthz_Allowed_WithHeadersAndTrailers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	// Start a test ext_authz server that allows the data plane RPC.
	extAuthServer := &extAuthzStubServer{
		checkF: func(context.Context, *v3authpb.CheckRequest) (*v3authpb.CheckResponse, error) {
			st := &spb.Status{Code: int32(codes.OK)}
			return &v3authpb.CheckResponse{
				Status: st,
				HttpResponse: &v3authpb.CheckResponse_OkResponse{
					OkResponse: &v3authpb.OkHttpResponse{
						Headers: []*v3corepb.HeaderValueOption{
							{Header: &v3corepb.HeaderValue{Key: "k1", Value: "v1"}}, // Allowed.
						},
						ResponseHeadersToAdd: []*v3corepb.HeaderValueOption{
							{Header: &v3corepb.HeaderValue{Key: "k2-bin", Value: "v2", RawValue: []byte{0, 1, 2, 3}}}, // Allowed binary header.
						},
						HeadersToRemove: []string{":authority", "k-test-header-to-be-removed"},
					},
				},
			}, nil
		},
	}
	extAuthServer.startServer(t)

	// Start a test backend with the ext_authz server option.
	config := &extauthz.InterceptorConfig{
		Server: extauthz.ServerConfig{
			TargetURI:          "dns:///" + extAuthServer.listener.Addr().String(),
			ChannelCredentials: insecure.NewCredentials(),
		},
		EnabledPercent: 100, // External authorization is always enabled.
		DecoderHeaderMutationRules: extauthz.HeaderMutationRules{
			AllowExpr:       regexp.MustCompile("^k.*"),
			DisallowExpr:    regexp.MustCompile("^a1$"),
			DisallowIsError: true, // Disallowed header mutations result in RPC failures.
		},
	}
	extAuthzServerOption, done := extauthz.ServerOption(config)
	defer done()

	// Also include a test interceptor in the chain to verify if subsequent
	// interceptors in the chain are invoked. This interceptor also adds a
	// trailer.
	testInterceptor := newTestInterceptor("test-trailer-key", "test-trailer-value")
	backend, unaryCh, streamingCh := startTestServiceBackend(t, extAuthzServerOption, testInterceptor.serverOption())

	// Create a grpc client to the above backend server.
	cc, err := grpc.NewClient(backend.Address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	defer cc.Close()

	wantTrailers := metadata.Pairs(
		"test-trailer-key", "test-trailer-value",
		"k2-bin", "\x00\x01\x02\x03",
	)
	// Verify that the next interceptor sees the expected headers. The
	// :authority header is added by gRPC and not by the ext_authz interceptor.
	wantHeaders := metadata.Pairs(":authority", backend.Address, "k1", "v1")
	outgoingCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("k-test-header-to-be-removed", "true"))
	makeUnaryRPC(outgoingCtx, t, cc, codes.OK, wantTrailers, unaryCh, wantHeaders)
	makeStreamingRPC(outgoingCtx, t, cc, codes.OK, wantTrailers, streamingCh, wantHeaders)
}

// Compare metadata removes metadata entries that are not pertinent to tests in
// this package before comparing them. This ensures that the expected metadata
// defined in tests will be shorter.
func compareMetadata(got, want metadata.MD) error {
	if got.Get("content-type") != nil {
		got.Delete("content-type")
	}
	if got.Get("user-agent") != nil {
		got.Delete("user-agent")
	}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		return fmt.Errorf("diff in metadata (-want +got):\n%s", diff)
	}
	return nil
}

// A test interceptor that can be added to the interceptor chain to verify if
// subsequent interceptors in the chain are being invoked. This interceptor also
// supports adding trailers to enable testing the trailer merging functionality
// in the ext_authz interceptor.
type testInterceptor struct {
	unaryCalled     chan struct{}
	streamingCalled chan struct{}
	trailers        metadata.MD
}

func newTestInterceptor(trialersPairsToAdd ...string) *testInterceptor {
	return &testInterceptor{
		unaryCalled:     make(chan struct{}, 1),
		streamingCalled: make(chan struct{}, 1),
		trailers:        metadata.Pairs(trialersPairsToAdd...),
	}
}

func (t *testInterceptor) serverOption() grpc.ServerOption {
	joinServerOptions := internal.JoinServerOptions.(func(...grpc.ServerOption) grpc.ServerOption)
	return joinServerOptions(grpc.ChainUnaryInterceptor(t.unaryInterceptor), grpc.ChainStreamInterceptor(t.streamInterceptor))
}

func (t *testInterceptor) unaryInterceptor(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	select {
	case t.unaryCalled <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	resp, err := handler(ctx, req)
	ss := grpc.ServerTransportStreamFromContext(ctx)
	if t.trailers != nil {
		ss.SetTrailer(t.trailers)
	}
	return resp, err
}

func (t *testInterceptor) streamInterceptor(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	select {
	case t.streamingCalled <- struct{}{}:
	case <-ss.Context().Done():
		return ss.Context().Err()
	}

	err := handler(srv, ss)
	if t.trailers != nil {
		ss.SetTrailer(t.trailers)
	}
	return err
}
