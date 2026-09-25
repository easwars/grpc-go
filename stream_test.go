/*
 *
 * Copyright 2023 gRPC authors.
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

package grpc_test

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/internal/grpctest"
	iresolver "google.golang.org/grpc/internal/resolver"
	"google.golang.org/grpc/internal/stubserver"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"
	"google.golang.org/grpc/status"

	testgrpc "google.golang.org/grpc/interop/grpc_testing"
	testpb "google.golang.org/grpc/interop/grpc_testing"
)

const defaultTestTimeout = 10 * time.Second

type s struct {
	grpctest.Tester
}

func Test(t *testing.T) {
	grpctest.RunSubTests(t, s{})
}

func (s) TestStream_Header_TrailersOnly(t *testing.T) {
	ss := stubserver.StubServer{
		FullDuplexCallF: func(testgrpc.TestService_FullDuplexCallServer) error {
			return status.Errorf(codes.NotFound, "a test error")
		},
	}
	if err := ss.Start(nil); err != nil {
		t.Fatal("Error starting server:", err)
	}
	defer ss.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	s, err := ss.Client.FullDuplexCall(ctx)
	if err != nil {
		t.Fatal("Error staring call", err)
	}
	if md, err := s.Header(); md != nil || err != nil {
		t.Fatalf("s.Header() = %v, %v; want nil, nil", md, err)
	}
	if _, err := s.Recv(); status.Code(err) != codes.NotFound {
		t.Fatalf("s.Recv() = _, %v; want _, err.Code()=codes.NotFound", err)
	}
}

// TestUnaryClient_ServerStreamingMismatch ensures that the client's
// non-streaming RecvMsg() logic correctly handles various error scenarios
// from the server.
//
// The Client initiates a Unary RPC (Invoke), forcing it to use the
// non-server-streaming `recvMsg` code path (where the bug was).
// The Server handles it as a Streaming RPC (FullDuplexCall), allowing us to
// send arbitrary sequences of messages and errors.
func (s) TestUnaryClient_ServerStreamingMismatch(t *testing.T) {
	tests := []struct {
		name              string
		fullDuplexCallF   func(testgrpc.TestService_FullDuplexCallServer) error
		wantErrorContains string
		wantCode          codes.Code
		clientCallOptions []grpc.CallOption
	}{
		{
			name: "server_sends_error_after_message",
			fullDuplexCallF: func(stream testgrpc.TestService_FullDuplexCallServer) error {
				if err := stream.Send(&testpb.StreamingOutputCallResponse{}); err != nil {
					return err
				}
				return status.Error(codes.Internal, "server error after message")
			},
			wantErrorContains: "server error after message",
			wantCode:          codes.Internal,
		},
		{
			name: "server_sends_second_message_exceeding_limit",
			fullDuplexCallF: func(stream testgrpc.TestService_FullDuplexCallServer) error {
				if err := stream.Send(&testpb.StreamingOutputCallResponse{
					Payload: &testpb.Payload{Body: make([]byte, 1)},
				}); err != nil {
					return err
				}
				return stream.Send(&testpb.StreamingOutputCallResponse{
					Payload: &testpb.Payload{Body: make([]byte, 10)},
				})
			},
			clientCallOptions: []grpc.CallOption{grpc.MaxCallRecvMsgSize(5)},
			wantErrorContains: "received message larger than max",
			wantCode:          codes.ResourceExhausted,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ss := &stubserver.StubServer{
				FullDuplexCallF: test.fullDuplexCallF,
			}
			if err := ss.Start(nil); err != nil {
				t.Fatal("Error starting server:", err)
			}
			defer ss.Stop()

			ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
			defer cancel()

			// Invoke the streaming RPC method as a Unary RPC. This forces the client
			// to use the non-streaming RecvMsg path, while the server handles it as
			// a stream (allowing it to send messages and errors in ways a standard
			// Unary server cannot).
			err := ss.CC.Invoke(ctx, "/grpc.testing.TestService/FullDuplexCall", &testpb.StreamingOutputCallRequest{}, &testpb.StreamingOutputCallResponse{}, test.clientCallOptions...)
			if err == nil {
				t.Fatal("Client.Invoke returned nil, want error")
			}
			if status.Code(err) != test.wantCode {
				t.Errorf("Unexpected error code: got %v, want %v", status.Code(err), test.wantCode)
			}
			if !strings.Contains(err.Error(), test.wantErrorContains) {
				t.Errorf("Unexpected error message: got %v, want %v", err.Error(), test.wantErrorContains)
			}
		})
	}
}

// interceptorStream wraps a ClientStream to record invocations of RecvMsg and
// CloseSend hooks across downstream interceptors.
type interceptorStream struct {
	grpc.ClientStream
	recvMsgCount   int
	closeSendCount int
}

func (s *interceptorStream) RecvMsg(m any) error {
	s.recvMsgCount++
	return s.ClientStream.RecvMsg(m)
}

func (s *interceptorStream) CloseSend() error {
	s.closeSendCount++
	return s.ClientStream.CloseSend()
}

// TestDefaultStreamInterceptor verifies that defaultStreamInterceptor's
// behavior of automatically triggering CloseSend on non-client-streaming RPCs
// right after SendMsg, and calling RecvMsg a second time on
// non-server-streaming RPCs to consume trailers and io.EOF, are not visible to
// user-defined interceptors.
func (s) TestDefaultStreamInterceptor(t *testing.T) {
	var iStream *interceptorStream
	// Define a client-side stream interceptor that wraps the ClientStream to
	// monitor hook invocations across RPC calls.
	clientInt := func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		cs, err := streamer(ctx, desc, cc, method, opts...)
		if err != nil {
			return nil, err
		}
		iStream = &interceptorStream{ClientStream: cs}
		return iStream, nil
	}

	// Setup a service implementing both a client-streaming RPC and a
	// server-streaming RPC.
	ss := &stubserver.StubServer{
		StreamingOutputCallF: func(_ *testpb.StreamingOutputCallRequest, stream testgrpc.TestService_StreamingOutputCallServer) error {
			return stream.Send(&testpb.StreamingOutputCallResponse{})
		},
		StreamingInputCallF: func(stream testgrpc.TestService_StreamingInputCallServer) error {
			for {
				if _, err := stream.Recv(); err != nil {
					if err == io.EOF {
						return stream.SendAndClose(&testpb.StreamingInputCallResponse{})
					}
					return err
				}
			}
		},
	}
	if err := ss.Start(nil, grpc.WithStreamInterceptor(clientInt)); err != nil {
		t.Fatal("Error starting server:", err)
	}
	defer ss.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	// Make a client-streaming RPC. When CloseAndRecv invokes RecvMsg once to
	// get the single reply message on a non-server-streaming RPC,
	// defaultStreamInterceptor automatically calls a second RecvMsg on the
	// underlying client stream to consume io.EOF and receive trailers. But this
	// should not be visible to user interceptors.
	stream, err := ss.Client.StreamingInputCall(ctx)
	if err != nil {
		t.Fatal("Error calling StreamingInputCall:", err)
	}
	if err := stream.Send(&testpb.StreamingInputCallRequest{}); err != nil {
		t.Fatal("Error sending request:", err)
	}
	if _, err := stream.CloseAndRecv(); err != nil {
		t.Fatal("Error running CloseAndRecv:", err)
	}
	if iStream.recvMsgCount != 1 {
		t.Fatalf("RecvMsg was called %v times on user interceptor stream, want 1 time", iStream.recvMsgCount)
	}

	// Make a server-streaming RPC. Since StreamingOutputCall is not
	// client-streaming, the proto generated code invokes CloseSend after
	// sending the single message. defaultStreamInterceptor also invokes
	// CloseSend right after sending the request message (to handle cases where
	// the user is using the ClientStream API instead of the proto generated
	// code) to signal downstream interceptors, which in this case are xDS
	// filters. The user interceptor should not see this CloseSend call.
	if _, err := ss.Client.StreamingOutputCall(ctx, &testpb.StreamingOutputCallRequest{}); err != nil {
		t.Fatal("Error calling StreamingOutputCall:", err)
	}
	if iStream.closeSendCount != 1 {
		t.Fatalf("CloseSend called %v times on user interceptor stream, want 1 times", iStream.closeSendCount)
	}
}

type funcConfigSelector struct {
	f func(iresolver.RPCInfo) (*iresolver.RPCConfig, error)
}

func (f funcConfigSelector) SelectConfig(i iresolver.RPCInfo) (*iresolver.RPCConfig, error) {
	return f.f(i)
}

type testClientInterceptor struct {
	newStream func(ctx context.Context, ri iresolver.RPCInfo, newStream func(ctx context.Context, opts ...grpc.CallOption) (grpc.ClientStream, error), opts ...grpc.CallOption) (grpc.ClientStream, error)
}

func (i *testClientInterceptor) NewStream(ctx context.Context, ri iresolver.RPCInfo, newStream func(ctx context.Context, opts ...grpc.CallOption) (grpc.ClientStream, error), opts ...grpc.CallOption) (grpc.ClientStream, error) {
	return i.newStream(ctx, ri, newStream, opts...)
}

func (i *testClientInterceptor) Close() {}

type mutatingClientStream struct {
	grpc.ClientStream
	extraHeader  metadata.MD
	extraTrailer metadata.MD
	overridePeer *peer.Peer
	sendMsgErr   error
	closeSendErr error
	recvMsgErr   error
}

func (s *mutatingClientStream) Header() (metadata.MD, error) {
	md, err := s.ClientStream.Header()
	if err != nil {
		return md, err
	}
	return metadata.Join(md, s.extraHeader), nil
}

func (s *mutatingClientStream) Trailer() metadata.MD {
	md := s.ClientStream.Trailer()
	return metadata.Join(md, s.extraTrailer)
}

func (s *mutatingClientStream) Context() context.Context {
	ctx := s.ClientStream.Context()
	if s.overridePeer != nil {
		return peer.NewContext(ctx, s.overridePeer)
	}
	return ctx
}

func (s *mutatingClientStream) SendMsg(m any) error {
	if s.sendMsgErr != nil {
		return s.sendMsgErr
	}
	return s.ClientStream.SendMsg(m)
}

func (s *mutatingClientStream) CloseSend() error {
	if s.closeSendErr != nil {
		return s.closeSendErr
	}
	return s.ClientStream.CloseSend()
}

func (s *mutatingClientStream) RecvMsg(m any) error {
	if s.recvMsgErr != nil {
		return s.recvMsgErr
	}
	return s.ClientStream.RecvMsg(m)
}

// TestClientStream_CallOptionAfterWithInterceptor verifies that Header,
// Trailer, and Peer CallOptions observe values returned by the ClientStream
// wrapped by an RPCConfig.Interceptor (e.g., xDS HTTP filters).
func (s) TestClientStream_CallOptionAfterWithInterceptor(t *testing.T) {
	ss := &stubserver.StubServer{
		EmptyCallF: func(ctx context.Context, _ *testpb.Empty) (*testpb.Empty, error) {
			grpc.SetHeader(ctx, metadata.Pairs("server-header", "server-header-val"))
			grpc.SetTrailer(ctx, metadata.Pairs("server-trailer", "server-trailer-val"))
			return &testpb.Empty{}, nil
		},
	}
	ss.R = manual.NewBuilderWithScheme("callopt-after")
	if err := ss.Start(nil); err != nil {
		t.Fatalf("Error starting stub server: %v", err)
	}
	defer ss.Stop()

	wantPeer := &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 12345},
	}
	interceptor := &testClientInterceptor{
		newStream: func(ctx context.Context, _ iresolver.RPCInfo, newStream func(ctx context.Context, opts ...grpc.CallOption) (grpc.ClientStream, error), opts ...grpc.CallOption) (grpc.ClientStream, error) {
			cs, err := newStream(ctx, opts...)
			if err != nil {
				return nil, err
			}
			return &mutatingClientStream{
				ClientStream: cs,
				extraHeader:  metadata.Pairs("filter-header", "filter-header-val"),
				extraTrailer: metadata.Pairs("filter-trailer", "filter-trailer-val"),
				overridePeer: wantPeer,
			}, nil
		},
	}

	state := iresolver.SetConfigSelector(resolver.State{
		Addresses:     []resolver.Address{{Addr: ss.Address}},
		ServiceConfig: ss.R.CC().ParseServiceConfig("{}"),
	}, funcConfigSelector{
		f: func(iresolver.RPCInfo) (*iresolver.RPCConfig, error) {
			return &iresolver.RPCConfig{Interceptor: interceptor}, nil
		},
	})
	ss.R.UpdateState(state)

	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	var gotHeader, gotTrailer metadata.MD
	var gotPeer peer.Peer
	if _, err := ss.Client.EmptyCall(ctx, &testpb.Empty{}, grpc.Header(&gotHeader), grpc.Trailer(&gotTrailer), grpc.Peer(&gotPeer)); err != nil {
		t.Fatalf("EmptyCall() failed: %v", err)
	}

	if got := gotHeader.Get("server-header"); !cmp.Equal(got, []string{"server-header-val"}) {
		t.Errorf("gotHeader[server-header] = %v, want [server-header-val]", got)
	}
	if got := gotHeader.Get("filter-header"); !cmp.Equal(got, []string{"filter-header-val"}) {
		t.Errorf("gotHeader[filter-header] = %v, want [filter-header-val]", got)
	}
	if got := gotTrailer.Get("server-trailer"); !cmp.Equal(got, []string{"server-trailer-val"}) {
		t.Errorf("gotTrailer[server-trailer] = %v, want [server-trailer-val]", got)
	}
	if got := gotTrailer.Get("filter-trailer"); !cmp.Equal(got, []string{"filter-trailer-val"}) {
		t.Errorf("gotTrailer[filter-trailer] = %v, want [filter-trailer-val]", got)
	}
	if !cmp.Equal(gotPeer.Addr, wantPeer.Addr) {
		t.Errorf("gotPeer.Addr = %v, want %v", gotPeer.Addr, wantPeer.Addr)
	}
}

// TestClientStream_OnFinishWhenInterceptorAborts verifies that when an
// RPCConfig.Interceptor creates the underlying innermost ClientStream and then
// aborts early (in NewStream, SendMsg, CloseSend, or RecvMsg) without
// delegating to the innermost stream, the innermost stream is still finished
// and OnFinish callbacks are executed exactly once.
func (s) TestClientStream_OnFinishWhenInterceptorAborts(t *testing.T) {
	wantErr := status.Error(codes.PermissionDenied, "aborted by filter")

	tests := []struct {
		name        string
		interceptor *testClientInterceptor
		makeRPC     func(ctx context.Context, client testgrpc.TestServiceClient, opt grpc.CallOption) error
	}{
		{
			name: "abort_in_NewStream_after_creating_innermost",
			interceptor: &testClientInterceptor{
				newStream: func(ctx context.Context, _ iresolver.RPCInfo, newStream func(ctx context.Context, opts ...grpc.CallOption) (grpc.ClientStream, error), opts ...grpc.CallOption) (grpc.ClientStream, error) {
					if _, err := newStream(ctx, opts...); err != nil {
						return nil, err
					}
					return nil, wantErr
				},
			},
			makeRPC: func(ctx context.Context, client testgrpc.TestServiceClient, opt grpc.CallOption) error {
				_, err := client.EmptyCall(ctx, &testpb.Empty{}, opt)
				return err
			},
		},
		{
			name: "abort_in_SendMsg_unary",
			interceptor: &testClientInterceptor{
				newStream: func(ctx context.Context, _ iresolver.RPCInfo, newStream func(ctx context.Context, opts ...grpc.CallOption) (grpc.ClientStream, error), opts ...grpc.CallOption) (grpc.ClientStream, error) {
					cs, err := newStream(ctx, opts...)
					if err != nil {
						return nil, err
					}
					return &mutatingClientStream{ClientStream: cs, sendMsgErr: wantErr}, nil
				},
			},
			makeRPC: func(ctx context.Context, client testgrpc.TestServiceClient, opt grpc.CallOption) error {
				_, err := client.EmptyCall(ctx, &testpb.Empty{}, opt)
				return err
			},
		},
		{
			name: "abort_in_SendMsg_client_streaming",
			interceptor: &testClientInterceptor{
				newStream: func(ctx context.Context, _ iresolver.RPCInfo, newStream func(ctx context.Context, opts ...grpc.CallOption) (grpc.ClientStream, error), opts ...grpc.CallOption) (grpc.ClientStream, error) {
					cs, err := newStream(ctx, opts...)
					if err != nil {
						return nil, err
					}
					return &mutatingClientStream{ClientStream: cs, sendMsgErr: wantErr}, nil
				},
			},
			makeRPC: func(ctx context.Context, client testgrpc.TestServiceClient, opt grpc.CallOption) error {
				stream, err := client.StreamingInputCall(ctx, opt)
				if err != nil {
					return err
				}
				return stream.Send(&testpb.StreamingInputCallRequest{})
			},
		},
		{
			name: "abort_in_CloseSend_unary",
			interceptor: &testClientInterceptor{
				newStream: func(ctx context.Context, _ iresolver.RPCInfo, newStream func(ctx context.Context, opts ...grpc.CallOption) (grpc.ClientStream, error), opts ...grpc.CallOption) (grpc.ClientStream, error) {
					cs, err := newStream(ctx, opts...)
					if err != nil {
						return nil, err
					}
					return &mutatingClientStream{ClientStream: cs, closeSendErr: wantErr}, nil
				},
			},
			makeRPC: func(ctx context.Context, client testgrpc.TestServiceClient, opt grpc.CallOption) error {
				_, err := client.EmptyCall(ctx, &testpb.Empty{}, opt)
				return err
			},
		},
		{
			name: "abort_in_RecvMsg_unary",
			interceptor: &testClientInterceptor{
				newStream: func(ctx context.Context, _ iresolver.RPCInfo, newStream func(ctx context.Context, opts ...grpc.CallOption) (grpc.ClientStream, error), opts ...grpc.CallOption) (grpc.ClientStream, error) {
					cs, err := newStream(ctx, opts...)
					if err != nil {
						return nil, err
					}
					return &mutatingClientStream{ClientStream: cs, recvMsgErr: wantErr}, nil
				},
			},
			makeRPC: func(ctx context.Context, client testgrpc.TestServiceClient, opt grpc.CallOption) error {
				_, err := client.EmptyCall(ctx, &testpb.Empty{}, opt)
				return err
			},
		},
		{
			name: "abort_in_RecvMsg_server_streaming",
			interceptor: &testClientInterceptor{
				newStream: func(ctx context.Context, _ iresolver.RPCInfo, newStream func(ctx context.Context, opts ...grpc.CallOption) (grpc.ClientStream, error), opts ...grpc.CallOption) (grpc.ClientStream, error) {
					cs, err := newStream(ctx, opts...)
					if err != nil {
						return nil, err
					}
					return &mutatingClientStream{ClientStream: cs, recvMsgErr: wantErr}, nil
				},
			},
			makeRPC: func(ctx context.Context, client testgrpc.TestServiceClient, opt grpc.CallOption) error {
				stream, err := client.StreamingOutputCall(ctx, &testpb.StreamingOutputCallRequest{}, opt)
				if err != nil {
					return err
				}
				_, err = stream.Recv()
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ss := &stubserver.StubServer{
				EmptyCallF: func(context.Context, *testpb.Empty) (*testpb.Empty, error) {
					return &testpb.Empty{}, nil
				},
				StreamingInputCallF: func(stream testgrpc.TestService_StreamingInputCallServer) error {
					for {
						if _, err := stream.Recv(); err != nil {
							if err == io.EOF {
								return stream.SendAndClose(&testpb.StreamingInputCallResponse{})
							}
							return err
						}
					}
				},
				StreamingOutputCallF: func(_ *testpb.StreamingOutputCallRequest, stream testgrpc.TestService_StreamingOutputCallServer) error {
					return stream.Send(&testpb.StreamingOutputCallResponse{})
				},
			}
			ss.R = manual.NewBuilderWithScheme("onfinish-abort")
			if err := ss.Start(nil); err != nil {
				t.Fatalf("Error starting stub server: %v", err)
			}
			defer ss.Stop()

			state := iresolver.SetConfigSelector(resolver.State{
				Addresses:     []resolver.Address{{Addr: ss.Address}},
				ServiceConfig: ss.R.CC().ParseServiceConfig("{}"),
			}, funcConfigSelector{
				f: func(iresolver.RPCInfo) (*iresolver.RPCConfig, error) {
					return &iresolver.RPCConfig{Interceptor: tc.interceptor}, nil
				},
			})
			ss.R.UpdateState(state)

			ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
			defer cancel()

			var onFinishCount int
			var onFinishErr error
			err := tc.makeRPC(ctx, ss.Client, grpc.OnFinish(func(err error) {
				onFinishCount++
				onFinishErr = err
			}))
			if status.Code(err) != codes.PermissionDenied {
				t.Fatalf("RPC failed with error %v, want code %v", err, codes.PermissionDenied)
			}
			if onFinishCount != 1 {
				t.Fatalf("OnFinish called %d times, want 1", onFinishCount)
			}
			if status.Code(onFinishErr) != codes.PermissionDenied {
				t.Fatalf("OnFinish received error %v, want code %v", onFinishErr, codes.PermissionDenied)
			}
		})
	}
}
