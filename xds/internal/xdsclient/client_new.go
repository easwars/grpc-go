/*
 *
 * Copyright 2022 gRPC authors.
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

package xdsclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/internal"
	"google.golang.org/grpc/internal/cache"
	"google.golang.org/grpc/internal/grpcsync"
	"google.golang.org/grpc/internal/xds/bootstrap"
	"google.golang.org/grpc/xds/internal/xdsclient/xdsresource"
)

// New returns a new xDS client configured by the bootstrap file specified in env
// variable GRPC_XDS_BOOTSTRAP or GRPC_XDS_BOOTSTRAP_CONFIG.
//
// The returned client is a reference counted singleton instance. This function
// creates a new client only when one doesn't already exist.
//
// The second return value represents a close function which releases the
// caller's reference on the returned client.  The caller is expected to invoke
// it once they are done using the client. The underlying client will be closed
// only when all references are released, and it is safe for the caller to
// invoke this close function multiple times.
func New() (XDSClient, func(), error) {
	return newRefCountedWithConfig(nil)
}

// NewWithConfig returns a new xDS client configured by the given config.
//
// The second return value represents a close function which releases the
// caller's reference on the returned client.  The caller is expected to invoke
// it once they are done using the client. The underlying client will be closed
// only when all references are released, and it is safe for the caller to
// invoke this close function multiple times.
//
// # Internal/Testing Only
//
// This function should ONLY be used by the internal google-c2p resolver.
// DO NOT use this elsewhere. Use New() instead.
func NewWithConfig(config *bootstrap.Config) (XDSClient, func(), error) {
	return newRefCountedWithConfig(config)
}

// newWithConfig returns a new xDS client with the given config.
func newWithConfig(config *bootstrap.Config, watchExpiryTimeout time.Duration, idleAuthorityDeleteTimeout time.Duration) (*clientImpl, error) {
	ctx, cancel := context.WithCancel(context.Background())
	c := &clientImpl{
		done:               grpcsync.NewEvent(),
		config:             config,
		watchExpiryTimeout: watchExpiryTimeout,
		serializer:         grpcsync.NewCallbackSerializer(ctx),
		serializerClose:    cancel,
		resourceTypes:      newResourceTypeRegistry(),
		authorities:        make(map[string]*authority),
		idleAuthorities:    cache.NewTimeoutCache(idleAuthorityDeleteTimeout),
	}

	c.logger = prefixLogger(c)
	c.logger.Infof("Created client to xDS management server: %s", config.XDSServer)
	return c, nil
}

// ClientOptionsForTesting TBD.
type ClientOptionsForTesting struct {
	// Name uniquely identifies an xDS client instance. Must be non-empty. Tests
	// are recommended to pass `t.Name()` for this field. If a test creates more
	// than one xDS client, it should pass `t.Name() + <unique-suffix>`.
	Name string

	// BootstrapConfig contains parsed bootstrap configuration. If both
	// BootstrapConfig and BootstrapContents are specified, the former takes
	// preference.
	BootstrapConfig *bootstrap.Config

	/// BootstrapContents contains raw JSON bootstrap configuration.
	BootstrapContents []byte

	// WatchExpiryTimeout specifies the timeout for watches registered by the
	// xDS client, for xDS resources. If unspecified, the default value used by
	// the implementation is used, which is currently 15 seconds.
	WatchExpiryTimeout time.Duration

	// AuthorityIdleTimeout specifies the timeout for authorities to be cleaned
	// up after being removed (because of no more watches to them). Uf
	// unspecified, the default value used by the implementation is used, which
	// is currently 5 minutes.
	AuthorityIdleTimeout time.Duration
}

// NewForTesting TBD.
func NewForTesting(opts ClientOptionsForTesting) (XDSClient, func(), error) {
	if opts.Name == "" {
		return nil, nil, fmt.Errorf("empty Name field in options struct")
	}

	// TODO: make sure that opts.Name is not already used for another client.

	// TODO: Use opts.Name and place these clients in the same clients map as
	// used by non-test code.

	cfg := opts.BootstrapConfig
	if cfg == nil {
		var err error
		cfg, err = bootstrap.NewConfigFromContents(opts.BootstrapContents)
		if err != nil {
			return nil, nil, fmt.Errorf("bootstrap config %s: %v", string(opts.BootstrapContents), err)
		}
	}
	if opts.WatchExpiryTimeout == 0 {
		opts.WatchExpiryTimeout = defaultWatchExpiryTimeout
	}
	if opts.AuthorityIdleTimeout == 0 {
		opts.AuthorityIdleTimeout = defaultIdleAuthorityDeleteTimeout
	}
	return NewWithConfigForTesting(cfg, opts.WatchExpiryTimeout, opts.AuthorityIdleTimeout)
}

// NewWithConfigForTesting returns an xDS client for the specified bootstrap
// config, separate from the global singleton.
//
// The second return value represents a close function which the caller is
// expected to invoke once they are done using the client.  It is safe for the
// caller to invoke this close function multiple times.
//
// # Testing Only
//
// This function should ONLY be used for testing purposes.
func NewWithConfigForTesting(config *bootstrap.Config, watchExpiryTimeout, authorityIdleTimeout time.Duration) (XDSClient, func(), error) {
	cl, err := newWithConfig(config, watchExpiryTimeout, authorityIdleTimeout)
	if err != nil {
		return nil, nil, err
	}
	return cl, grpcsync.OnceFunc(cl.close), nil
}

func init() {
	internal.TriggerXDSResourceNameNotFoundClient = triggerXDSResourceNameNotFoundClient
	internal.NewXDSClientForTesting = NewForTesting
}

var singletonClientForTesting = atomic.Pointer[clientRefCounted]{}

func triggerXDSResourceNameNotFoundClient(resourceType, resourceName string) error {
	c := singletonClientForTesting.Load()
	return internal.TriggerXDSResourceNameNotFoundForTesting.(func(func(xdsresource.Type, string) error, string, string) error)(c.clientImpl.triggerResourceNotFoundForTesting, resourceType, resourceName)
}

// NewWithBootstrapContentsForTesting returns an xDS client for this config,
// separate from the global singleton.
//
// The second return value represents a close function which the caller is
// expected to invoke once they are done using the client.  It is safe for the
// caller to invoke this close function multiple times.
//
// # Testing Only
//
// This function should ONLY be used for testing purposes.
func NewWithBootstrapContentsForTesting(contents []byte) (XDSClient, func(), error) {
	// Normalize the contents
	buf := bytes.Buffer{}
	err := json.Indent(&buf, contents, "", "")
	if err != nil {
		return nil, nil, fmt.Errorf("xds: error normalizing JSON: %v", err)
	}
	contents = bytes.TrimSpace(buf.Bytes())

	c, err := getOrMakeClientForTesting(contents)
	if err != nil {
		return nil, nil, err
	}
	singletonClientForTesting.Store(c)
	return c, grpcsync.OnceFunc(func() {
		clientsMu.Lock()
		defer clientsMu.Unlock()
		if c.decrRef() == 0 {
			c.close()
			delete(clients, string(contents))
			singletonClientForTesting.Store(nil)
		}
	}), nil
}

// getOrMakeClientForTesting creates a new reference counted client (separate
// from the global singleton) for the given config, or returns an existing one.
// It takes care of incrementing the reference count for the returned client,
// and leaves the caller responsible for decrementing the reference count once
// the client is no longer needed.
func getOrMakeClientForTesting(config []byte) (*clientRefCounted, error) {
	clientsMu.Lock()
	defer clientsMu.Unlock()

	if c := clients[string(config)]; c != nil {
		c.incrRef()
		return c, nil
	}

	bcfg, err := bootstrap.NewConfigFromContents(config)
	if err != nil {
		return nil, fmt.Errorf("bootstrap config %s: %v", string(config), err)
	}
	cImpl, err := newWithConfig(bcfg, defaultWatchExpiryTimeout, defaultIdleAuthorityDeleteTimeout)
	if err != nil {
		return nil, fmt.Errorf("creating xDS client: %v", err)
	}
	c := &clientRefCounted{clientImpl: cImpl, refCount: 1}
	clients[string(config)] = c
	return c, nil
}

var (
	clients   = map[string]*clientRefCounted{}
	clientsMu sync.Mutex
)
