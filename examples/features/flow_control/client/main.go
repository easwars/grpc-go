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

// Binary client is an example client.
package main

import (
	"context"
	"flag"
	"io"
	"log"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "google.golang.org/grpc/examples/features/proto/echo"
)

var addr = flag.String("addr", "localhost:50051", "the address to connect to")

func main() {
	flag.Parse()

	// Set up a connection to the server, with an initial connection window size
	// of 65K bytes. The server is set up to send messages of size 64K bytes as
	// a stream. But since the client only reads one message per second, the
	// flow control window takes care of throttling the server.
	conn, err := grpc.Dial(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithInitialConnWindowSize(66560))
	if err != nil {
		log.Fatalf("Failed to connect to server at %q: %v", *addr, err)
	}
	defer conn.Close()

	c := pb.NewEchoClient(conn)
	stream, err := c.ServerStreamingEcho(context.Background(), &pb.EchoRequest{})
	if err != nil {
		log.Fatalf("Failed to create server streaming RPC: %v", err)
	}

	// Read one response per second.
	ticker := time.NewTicker(time.Second)
	for range ticker.C {
		resp, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			log.Fatalf("Failed to read from stream: %v", err)
		}
		log.Println("Read response from server")
	}
}
