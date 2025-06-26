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
 */

package xdsresource

import (
	"fmt"

	"google.golang.org/grpc/internal/xds/bootstrap"
	"google.golang.org/grpc/xds/internal/clients/xdsclient"
	"google.golang.org/grpc/xds/internal/xdsclient/xdsresource/version"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

const (
	// ListenerResourceTypeName represents the transport agnostic name for the
	// listener resource.
	ListenerResourceTypeName = "ListenerResource"
)

var (
	// Compile time interface checks.
	_ xdsclient.Decoder = &listenerResourceDecoder{}
)

func securityConfigValidator(bc *bootstrap.Config, sc *SecurityConfig) error {
	if sc == nil {
		return nil
	}
	if sc.IdentityInstanceName != "" {
		if _, ok := bc.CertProviderConfigs()[sc.IdentityInstanceName]; !ok {
			return fmt.Errorf("identity certificate provider instance name %q missing in bootstrap configuration", sc.IdentityInstanceName)
		}
	}
	if sc.RootInstanceName != "" {
		if _, ok := bc.CertProviderConfigs()[sc.RootInstanceName]; !ok {
			return fmt.Errorf("root certificate provider instance name %q missing in bootstrap configuration", sc.RootInstanceName)
		}
	}
	return nil
}

func listenerValidator(bc *bootstrap.Config, lis ListenerUpdate) error {
	if lis.InboundListenerCfg == nil || lis.InboundListenerCfg.FilterChains == nil {
		return nil
	}
	return lis.InboundListenerCfg.FilterChains.Validate(func(fc *FilterChain) error {
		if fc == nil {
			return nil
		}
		return securityConfigValidator(bc, fc.SecurityCfg)
	})
}

// ListenerWatcher wraps the callbacks to be invoked for different
// events corresponding to the listener resource being watched. gRFC A88
// contains an exhaustive list of what method is invoked under what conditions.
type ListenerWatcher interface {
	// ResourceChanged indicates a new version of the resource is available.
	ResourceChanged(resource *ListenerResourceData, done func())

	// ResourceError indicates an error occurred while trying to fetch or
	// decode the associated resource. The previous version of the resource
	// should be considered invalid.
	ResourceError(err error, done func())

	// AmbientError indicates an error occurred after a resource has been
	// received that should not modify the use of that resource but may provide
	// useful information about the state of the XDSClient for debugging
	// purposes. The previous version of the resource should still be
	// considered valid.
	AmbientError(err error, done func())
}

type delegatingListenerWatcher struct {
	watcher ListenerWatcher
}

func (d *delegatingListenerWatcher) ResourceChanged(data xdsclient.ResourceData, onDone func()) {
	l := data.(*ListenerResourceData)
	d.watcher.ResourceChanged(l, onDone)
}
func (d *delegatingListenerWatcher) ResourceError(err error, onDone func()) {
	d.watcher.ResourceError(err, onDone)
}

func (d *delegatingListenerWatcher) AmbientError(err error, onDone func()) {
	d.watcher.AmbientError(err, onDone)
}

// WatchListener uses xDS to discover the configuration associated with the
// provided listener resource name.
func WatchListener(c Producer, name string, w ListenerWatcher) (cancel func()) {
	delegator := &delegatingListenerWatcher{watcher: w}
	return c.GenericWatchResource(version.V3ListenerURL, name, delegator)
}

// ListenerResourceData contains the parsed configuration of a Listener
// resource as received from the management server.
//
// Implements the xdsclient.ResourceData interface.
type ListenerResourceData struct {
	Resource ListenerUpdate
}

func (l *ListenerResourceData) Equal(other xdsclient.ResourceData) bool {
	if l == nil && other == nil {
		return true
	}
	if other == nil {
		return false
	}

	otherResourceData, ok := other.(*ListenerResourceData)
	if !ok {
		return false
	}
	return proto.Equal(l.Resource.Raw, otherResourceData.Resource.Raw)
}

func (l *ListenerResourceData) Bytes() []byte {
	return l.Resource.Raw.Value
}

type listenerResourceDecoder struct {
	bootstrapConfig *bootstrap.Config
}

func (ld *listenerResourceDecoder) Decode(resourceBytes []byte, gOpts xdsclient.DecodeOptions) (*xdsclient.DecodeResult, error) {
	// Unmarshal the received resource bytes into an Any proto and unwrap to get
	// to the actual resource proto, if required.
	receivedAny := &anypb.Any{}
	if err := proto.Unmarshal(resourceBytes, receivedAny); err != nil {
		return nil, fmt.Errorf("listener resource: failed to unmarshal received resource into Any proto: %v", err)
	}
	/*
		resourceProto, err := UnwrapResource(receivedAny)
		if err != nil {
			return nil, fmt.Errorf("listener resource: failed to unwrap received Any proto: %v", err)
		}
	*/

	name, listener, err := unmarshalListenerResource(receivedAny)
	switch {
	case name == "":
		// Name is unset only when protobuf deserialization fails.
		return nil, err
	case err != nil:
		// Protobuf deserialization succeeded, but resource validation failed.
		return &xdsclient.DecodeResult{
			Name:     name,
			Resource: &ListenerResourceData{},
		}, err
	}

	// Perform extra validation here.
	if err := listenerValidator(ld.bootstrapConfig, listener); err != nil {
		return &xdsclient.DecodeResult{
			Name:     name,
			Resource: &ListenerResourceData{},
		}, err
	}

	return &xdsclient.DecodeResult{
		Name:     name,
		Resource: &ListenerResourceData{Resource: listener},
	}, nil
}

func NewGenericListenerResourceTypeImplemetation(bc *bootstrap.Config) xdsclient.ResourceType {
	return xdsclient.ResourceType{
		TypeURL:                    version.V3ListenerURL,
		TypeName:                   ListenerResourceTypeName,
		AllResourcesRequiredInSotW: true,
		Decoder:                    &listenerResourceDecoder{bootstrapConfig: bc},
	}
}
