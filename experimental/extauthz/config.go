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

package extauthz

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/internal/xds/matcher"
	"google.golang.org/grpc/metadata"

	v3corepb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
)

// InterceptorConfig contains the configuration for the external authorization
// server interceptor.
type InterceptorConfig struct {
	// Server is the configuration for the external authorization server.
	Server ServerConfig
	// EnabledPercent specifies the percentage of requests to be authorized by
	// the external authorization server. If unset, no requests will be
	// authorized by the external authorization server.
	EnabledPercent uint32
	// DenyAtDisabled specifies whether to deny requests when external
	// authorization is disabled via the EnabledPercent configuration. If true,
	// requests will be denied with a status based on StatusCodeOnError.
	DenyAtDisabled bool
	// StatusCodeOnError is a HTTP status code to use when the external
	// authorization is disabled or when the call to the external authorization
	// server fails. The HTTP status code will be converted to a gRPC status
	// code using the mapping defined in
	// https://github.com/grpc/grpc/blob/master/doc/http-grpc-status-mapping.md.
	//
	// If unset, defaults to status code used 403 (Forbidden) which translates
	// to grpc status PermissionDenied.
	StatusCodeOnError int
	// FailureModeAllow specifies the behavior when the call to the external
	// authorization server fails. If true, the request will be allowed in such
	// cases. If false, the request will be denied with a status based on
	// StatusCodeOnError.
	FailureModeAllow bool
	// FailureModeAllowHeaderAdd specifies whether to add the
	// `x-envoy-auth-failure-mode-allowed: true` header to the data plane RPC
	// when the call to the external authorization server fails.
	FailureModeAllowHeaderAdd bool
	// AllowedHeaders specifies the headers that are allowed to be sent to the
	// external authorization server.
	AllowedHeaders []matcher.StringMatcher
	// DisallowedHeaders specifies the headers that will not be sent to the
	// external authorization server. This overrides the above AllowedHeaders if
	// a header matches both.
	DisallowedHeaders []matcher.StringMatcher
	// DecoderHeaderMutationRules specifies the rules for what modifications an
	// external authorization server may make to headers sent on the data plane
	// RPC.
	DecoderHeaderMutationRules HeaderMutationRules
	// IncludePeerCertificate specifies whether to include the peer certificate
	// in the request sent to the external authorization server.
	IncludePeerCertificate bool
}

// ServerConfig contains the configuration for the external authorization
// server.
type ServerConfig struct {
	// TargetURI is the name of the external authorization server.
	TargetURI string
	// ChannelCredentials specifies the transport credentials to use to connect
	// to the external authorization server. Must not be nil.
	ChannelCredentials credentials.TransportCredentials
	// CallCredentials specifies the per-RPC credentials to use when making
	// calls to the external authorization server.
	CallCredentials []credentials.PerRPCCredentials
	// Timeout is the timeout for the external authorization check request.
	// The actual timeout value used for the ext_authz RPC will be the
	// minimum of this value and the timeout on the data plane RPC. If unset,
	// timeout for the ext_authz RPC will be the timeout on the data plane
	// RPC.
	Timeout time.Duration
	// InitialMetadata is the additional metadata to include in all RPCs sent to
	// the external authorization server.
	InitialMetadata metadata.MD
}

// HeaderMutationRules specifies the rules for what modifications an external
// authorization server may make to headers sent on the data plane RPC.
type HeaderMutationRules struct {
	// AllowExpr specifies a regular expression that matches the headers that
	// can be mutated.
	AllowExpr *regexp.Regexp
	// DisallowExpr specifies a regular expression that matches the headers that
	// cannot be mutated. This overrides the above AllowExpr if a header matches
	// both.
	DisallowExpr *regexp.Regexp
	// DisallowAll specifies that no header mutations are allowed. This
	// overrides all other settings.
	DisallowAll bool
	// DisallowIsError specifies whether to return an error if a header mutation
	// is disallowed. If true, the data plane RPC will be failed with a grpc
	// status code of Unknown.
	DisallowIsError bool
}

// ApplyAdditions takes a set of header mutations (for additions and
// modifications) received from an authorization server and applies them to the
// provided metadata, subject to the rules defined in hmr.
//
// If the DisallowAll field is true, no mutations are performed, and the input
// metadata is returned unmodified.
//
// It iterates through each header mutation, performs validation on the header
// key and value, and checks if the mutation is permitted by the AllowExpr and
// DisallowExpr regular expressions.
//
// The following headers are always ignored:
// - Pseudo-headers (keys starting with ':').
// - The 'host' header.
// - Headers with non-lowercase keys.
// - Headers with keys or values exceeding 16384 bytes.
//
// If a mutation is disallowed and DisallowIsError is true, an error is
// returned. Otherwise, the disallowed mutation is silently ignored.
//
// Allowed mutations are applied to a copy of the input metadata, which is then
// returned. The original input metadata is not modified.
//
// If the receiver `hmr` is nil, all header mutations are permitted.
func (hmr *HeaderMutationRules) ApplyAdditions(hvos []*v3corepb.HeaderValueOption, input metadata.MD) (output metadata.MD, err error) {
	if hmr == nil {
		hmr = &HeaderMutationRules{}
	}

	if hmr.DisallowAll {
		return input, nil
	}

	output = input.Copy()
	for _, hvo := range hvos {
		header := hvo.GetHeader()
		key := header.GetKey()
		if len(key) == 0 || key[0] == ':' || key == "host" || key != strings.ToLower(key) || len(key) > 16384 {
			continue
		}

		value := header.GetValue()
		if strings.HasSuffix(key, "-bin") {
			value = string(header.GetRawValue())
		}
		if len(value) > 16384 {
			continue
		}

		if !hmr.allow(key) {
			if hmr.DisallowIsError {
				return nil, fmt.Errorf("extauthz: header mutation disallowed by HeaderMutationRules for header key %q", key)
			}
			continue
		}

		// Perform the mutation on output metadata using the append_action
		// field from the header value option.
		switch hvo.GetAppendAction() {
		case v3corepb.HeaderValueOption_APPEND_IF_EXISTS_OR_ADD:
			output.Append(key, value)
		case v3corepb.HeaderValueOption_ADD_IF_ABSENT:
			if output.Get(key) == nil {
				output.Set(key, value)
			}
		case v3corepb.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD:
			output.Set(key, value)
		case v3corepb.HeaderValueOption_OVERWRITE_IF_EXISTS:
			if output.Get(key) != nil {
				output.Set(key, value)
			}
		}
	}
	return output, nil
}

// ApplyRemovals takes a set of headers (for removal) received from an
// authorization server and applies them to the provided metadata, subject to
// the rules defined in hmr.
//
// This method is very similar to ApplyAdditions, except that headers are
// removed here instead of added or mutated as is the case in the latter. See
// ApplyAdditions for more details.
func (hmr *HeaderMutationRules) ApplyRemovals(headersToRemove []string, input metadata.MD) (output metadata.MD, err error) {
	if hmr == nil {
		hmr = &HeaderMutationRules{}
	}

	if hmr.DisallowAll {
		return input, nil
	}

	output = input.Copy()
	for _, header := range headersToRemove {
		if len(header) == 0 || header[0] == ':' || header == "host" || header != strings.ToLower(header) || len(header) > 16384 {
			continue
		}

		if !hmr.allow(header) {
			if hmr.DisallowIsError {
				return nil, fmt.Errorf("extauthz: header mutation disallowed by HeaderMutationRules for header %q", header)
			}
			continue
		}

		output.Delete(header)
	}
	return output, nil
}

func (hmr *HeaderMutationRules) allow(key string) bool {
	if hmr.DisallowExpr != nil && hmr.DisallowExpr.MatchString(key) {
		return false
	}
	if hmr.AllowExpr != nil && hmr.AllowExpr.MatchString(key) {
		return true
	}
	if hmr.AllowExpr != nil {
		return false
	}
	return true
}

func validateInterceptorConfig(cfg *InterceptorConfig) error {
	if cfg == nil {
		return errors.New("extauthz: InterceptorConfig must be non-nil")
	}
	if err := validateServerConfig(&cfg.Server); err != nil {
		return err
	}
	if cfg.StatusCodeOnError != 0 && http.StatusText(cfg.StatusCodeOnError) == "" {
		return fmt.Errorf("extauthz: StatusCodeOnError (%d) is not a valid HTTP status code", cfg.StatusCodeOnError)
	}
	if cfg.StatusCodeOnError == 0 {
		cfg.StatusCodeOnError = http.StatusForbidden // Default to 403 (Forbidden).
	}
	return nil
}

func validateServerConfig(cfg *ServerConfig) error {
	if cfg.TargetURI == "" {
		return errors.New("extauthz: ServerConfig.TargetURI must be a non-empty string")
	}
	if cfg.ChannelCredentials == nil {
		return errors.New("extauthz: ServerConfig.ChannelCredentials must be non-nil")
	}
	if cfg.Timeout < 0 {
		return errors.New("extauthz: ServerConfig.Timeout must be non-negative")
	}
	for k, vals := range cfg.InitialMetadata {
		if len(k) == 0 || len(k) >= 16384 {
			return fmt.Errorf("extauthz: ServerConfig.InitialMetadata key %q has invalid length %d; must be in range [1, 16384)", k, len(k))
		}
		for _, v := range vals {
			if len(v) >= 16384 {
				return fmt.Errorf("extauthz: ServerConfig.InitialMetadata value for key %q has invalid length %d; must be less than 16384", k, len(v))
			}
		}
	}
	return nil
}
