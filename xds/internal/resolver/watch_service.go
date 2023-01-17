/*
 *
 * Copyright 2020 gRPC authors.
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

package resolver

import (
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc/xds/internal/clusterspecifier"
	"google.golang.org/grpc/xds/internal/xdsclient"
	"google.golang.org/grpc/xds/internal/xdsclient/xdsresource"
)

// serviceUpdate contains information received from the LDS/RDS responses which
// are of interest to the xds resolver. The RDS request is built by first
// making a LDS to get the RouteConfig name.
type serviceUpdate struct {
	// virtualHost contains routes and other configuration to route RPCs.
	virtualHost *xdsresource.VirtualHost
	// clusterSpecifierPlugins contains the configurations for any cluster
	// specifier plugins emitted by the xdsclient.
	clusterSpecifierPlugins map[string]clusterspecifier.BalancerConfig
	// ldsConfig contains configuration that applies to all routes.
	ldsConfig ldsConfig
}

// ldsConfig contains information received from the LDS responses which are of
// interest to the xds resolver.
type ldsConfig struct {
	// maxStreamDuration is from the HTTP connection manager's
	// common_http_protocol_options field.
	maxStreamDuration time.Duration
	httpFilterConfig  []xdsresource.HTTPFilter
}

type routeConfigWatcher struct {
	rdsName   string
	updateCb  func(xdsresource.RouteConfigUpdate, error)
	rdsCancel func()
}

func (rcw *routeConfigWatcher) OnUpdate(update *xdsresource.RouteConfigResourceData) {
	rcw.updateCb(update.Resource, nil)
}

func (rcw *routeConfigWatcher) OnError(err error) {
	rcw.updateCb(xdsresource.RouteConfigUpdate{}, err)
}
func (rcw *routeConfigWatcher) OnResourceDoesNotExist() {
	rcw.updateCb(xdsresource.RouteConfigUpdate{}, xdsresource.NewErrorf(xdsresource.ErrorTypeResourceNotFound, "route configuration resource %q not found", rcw.rdsName))
}

func (rcw *routeConfigWatcher) close() {
	rcw.rdsCancel()
}

type listenerWatcher struct {
	// These fields are configured when the watcher is created, and are not
	// written to subsequently. Hence these need not be protected by a mutex.
	serviceName string                     // Name of the listener resource being watched.
	xdsClient   xdsclient.XDSClient        // xDS client to watch route configuration.
	updateCb    func(serviceUpdate, error) // Callback to push updates.
	ldsCancel   func()                     // Cancel func for LDS watch.

	// Below fields, which can change based on updates received from the xDS
	// client, are protected by this mutex.
	mu         sync.Mutex
	rcWatcher  *routeConfigWatcher
	lastUpdate serviceUpdate
	closed     bool
}

func (lw *listenerWatcher) OnUpdate(resource *xdsresource.ListenerResourceData) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if lw.closed {
		return
	}

	update := resource.Resource
	lw.lastUpdate.ldsConfig = ldsConfig{
		maxStreamDuration: update.MaxStreamDuration,
		httpFilterConfig:  update.HTTPFilters,
	}

	if update.InlineRouteConfig != nil {
		// Cancel any existing RDS watch as we have an inline route
		// configuration now.
		if lw.rcWatcher != nil {
			lw.rcWatcher.close()
			lw.rcWatcher = nil
		}

		// Handle the inline route configurration as if it's from an RDS watch.
		lw.applyRouteConfigUpdateLocked(*update.InlineRouteConfig)
		return
	}

	// No inline route configuration, need an RDS watch to fetch the routes.

	// If the new route configuration name is same as the previous, don't cancel
	// and restart the RDS watch.
	if lw.rcWatcher != nil && lw.rcWatcher.rdsName == update.RouteConfigName {
		// If a previous route configuration was received, send an update with
		// new listener configuration and old route configuration.
		if lw.lastUpdate.virtualHost != nil {
			lw.updateCb(lw.lastUpdate, nil)
		}
		return
	}

	// If the route configuration name has changed, we need to cancel the old
	// watch, register a new one, and wait until the first RDS update before
	// reporting this LDS config.
	if lw.rcWatcher != nil {
		lw.rcWatcher.close()
	}
	lw.rcWatcher = &routeConfigWatcher{
		rdsName:  update.RouteConfigName,
		updateCb: lw.handleRouteConfigUpdate,
	}
	lw.rcWatcher.rdsCancel = xdsresource.WatchRouteConfig(lw.xdsClient, update.RouteConfigName, lw.rcWatcher)
}

func (lw *listenerWatcher) OnError(err error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if lw.closed {
		return
	}

	// Do not cancel the existing RDS watch and continue to use the previous
	// update (if one exists) for non-resource-not-found errors.
	lw.updateCb(serviceUpdate{}, err)
}

func (lw *listenerWatcher) OnResourceDoesNotExist() {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if lw.closed {
		return
	}

	// Cancel the existing RDS watch and stop using the previous update (if one
	// exists) for resource-not-found errors.
	if lw.rcWatcher != nil {
		lw.rcWatcher.close()
		lw.rcWatcher = nil
		lw.lastUpdate = serviceUpdate{}
	}
	lw.updateCb(serviceUpdate{}, xdsresource.NewErrorf(xdsresource.ErrorTypeResourceNotFound, "listener resource %q not found", lw.serviceName))
}

func (lw *listenerWatcher) handleRouteConfigUpdate(update xdsresource.RouteConfigUpdate, err error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if lw.closed {
		return
	}
	if lw.rcWatcher == nil {
		// This mean only the RDS watch is canceled, can happen if the LDS
		// resource is removed.
		return
	}
	if err != nil {
		lw.updateCb(serviceUpdate{}, err)
		return
	}
	lw.applyRouteConfigUpdateLocked(update)
}

func (lw *listenerWatcher) applyRouteConfigUpdateLocked(update xdsresource.RouteConfigUpdate) {
	matchVh := xdsresource.FindBestMatchingVirtualHost(lw.serviceName, update.VirtualHosts)
	if matchVh == nil {
		// No matching virtual host found.
		lw.updateCb(serviceUpdate{}, fmt.Errorf("no matching virtual host found for %q", lw.serviceName))
		return
	}

	lw.lastUpdate.virtualHost = matchVh
	lw.lastUpdate.clusterSpecifierPlugins = update.ClusterSpecifierPlugins
	lw.updateCb(lw.lastUpdate, nil)
}

func (lw *listenerWatcher) close() {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	lw.closed = true
	lw.ldsCancel()
	if lw.rcWatcher != nil {
		lw.rcWatcher.close()
		lw.rcWatcher = nil
	}
}
