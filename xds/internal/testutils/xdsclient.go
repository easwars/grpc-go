/*
 *
 * Copyright 2024 gRPC authors.
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

package testutils

import (
	"testing"

	"google.golang.org/grpc/internal"
	"google.golang.org/grpc/internal/testutils/xds/e2e"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/xds/internal/xdsclient"
)

// SetupXDSClient TBD.
func SetupXDSClient(t *testing.T, nodeID, serverAddr string) (xdsclient.XDSClient, func()) {
	bc, err := e2e.DefaultBootstrapContents(nodeID, serverAddr)
	if err != nil {
		t.Fatalf("Failed to create bootstrap configuration for nodeID %q and server address %q: %v", nodeID, serverAddr, err)
	}
	xdsC, close, err := xdsclient.NewForTesting(xdsclient.ClientOptionsForTesting{
		Name:              t.Name(),
		BootstrapContents: bc,
	})
	if err != nil {
		t.Fatalf("Failed to create xDS client: %v", err)
	}
	return xdsC, close
}

// SetupXDSResolver TBD.
func SetupXDSResolver(t *testing.T, nodeID, serverAddr string) resolver.Builder {
	bc, err := e2e.DefaultBootstrapContents(nodeID, serverAddr)
	if err != nil {
		t.Fatalf("Failed to create bootstrap configuration for nodeID %q and server address %q: %v", nodeID, serverAddr, err)
	}
	xdsC, close, err := xdsclient.NewForTesting(xdsclient.ClientOptionsForTesting{
		Name:              t.Name(),
		BootstrapContents: bc,
	})
	if err != nil {
		t.Fatalf("Failed to create xDS client: %v", err)
	}
	t.Cleanup(close)

	var rb resolver.Builder
	if newResolver := internal.NewXDSResolverForTesting; newResolver != nil {
		rb, err = newResolver.(func(xdsclient.XDSClient, func()) (resolver.Builder, error))(xdsC, close)
		if err != nil {
			t.Fatalf("Failed to create xDS resolver for testing: %v", err)
		}
	}

	return rb
}
