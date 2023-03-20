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

// Binary server is an example server.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"net"

	"google.golang.org/grpc"

	pb "google.golang.org/grpc/examples/features/proto/echo"
)

var port = flag.Int("port", 50051, "the port to serve on")

type server struct {
	pb.UnimplementedEchoServer
}

func (s *server) UnaryEcho(ctx context.Context, in *pb.EchoRequest) (*pb.EchoResponse, error) {
	return &pb.EchoResponse{Message: in.Message}, nil
}

func (s *server) ServerStreamingEcho(in *pb.EchoRequest, stream pb.Echo_ServerStreamingEchoServer) error {
	for i := 0; i < 10; i++ {
		if err := stream.Send(&pb.EchoResponse{Message: string(bytes.Repeat([]byte{0x41}, 65536))}); err != nil {
			return err
		}
		log.Println("Sent message number", i)
	}
	return nil
}

func main() {
	flag.Parse()

	lis, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", *port))
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}
	log.Printf("Server listening at %v\n", lis.Addr())

	s := grpc.NewServer()
	pb.RegisterEchoServer(s, &server{})
	s.Serve(lis)
}
