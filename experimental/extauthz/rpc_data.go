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
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/url"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/internal/xds/matcher"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"

	v3corepb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
)

// rpcData contains the extracted data from the incoming RPC to be sent to
// the external authorization server.
type rpcData struct {
	localAddr  string
	remoteAddr string
	methodName string
	principal  string
	peerCert   string
	headerMap  *v3corepb.HeaderMap
}

// newRPCData extracts the relevant data from the incoming RPC context and
// constructs an rpcData struct. The allowedHeaders and disallowedHeaders
// parameters are used to filter the incoming metadata headers.
func newRPCData(ctx context.Context, allowedHeaders, disallowedHeaders []matcher.StringMatcher) (*rpcData, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, fmt.Errorf("missing metadata in incoming context")
	}
	methodName, ok := grpc.Method(ctx)
	if !ok {
		return nil, fmt.Errorf("missing method in incoming context")
	}

	peerInfo, ok := peer.FromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("missing peer info in incoming context")
	}
	remoteAddr, localAddr := peerInfo.Addr.String(), peerInfo.LocalAddr.String()

	// Extract the peer certificates, if available.
	var principal, peerCert string
	if tlsInfo, ok := peerInfo.AuthInfo.(credentials.TLSInfo); ok {
		if len(tlsInfo.State.PeerCertificates) > 0 {
			leafCert := tlsInfo.State.PeerCertificates[0]
			principal = principalFromCert(leafCert)

			// Encode the certificate in PEM format and URL-escape it.
			pemBytes := pem.EncodeToMemory(&pem.Block{
				Type:  "CERTIFICATE",
				Bytes: leafCert.Raw,
			})
			if pemBytes == nil {
				return nil, fmt.Errorf("failed to encode peer certificate to PEM")
			}
			peerCert = url.QueryEscape(string(pemBytes))
		}
	}

	return &rpcData{
		localAddr:  localAddr,
		remoteAddr: remoteAddr,
		methodName: methodName,
		principal:  principal,
		peerCert:   peerCert,
		headerMap:  constructHeaderMap(md, allowedHeaders, disallowedHeaders),
	}, nil
}

// constructHeaderMap constructs a HeaderMap from the given metadata, using the
// following rules:
//   - if the header is matched by the disallowed_headers config field, it will
//     not be added to the map, otherwise,
//   - if the allowed_headers config field is unset or matches the header, the
//     header will be added to the map, otherwise,
//   - the header will be excluded from the map.
func constructHeaderMap(md metadata.MD, allowedHeaders, disallowedHeaders []matcher.StringMatcher) *v3corepb.HeaderMap {
	headerMap := &v3corepb.HeaderMap{}
	for key, values := range md {
		if isDisallowedHeader(key, disallowedHeaders) {
			continue
		}
		if isAllowedHeader(key, allowedHeaders) {
			for _, value := range values {
				headerMap.Headers = append(headerMap.Headers, &v3corepb.HeaderValue{
					Key:      key,
					RawValue: []byte(value),
				})
			}
		}
	}
	if len(headerMap.Headers) == 0 {
		return nil
	}
	return headerMap
}

// isDisallowedHeader returns true if the given header key matches any of the
// provided disallowed header matchers.
func isDisallowedHeader(key string, matchers []matcher.StringMatcher) bool {
	for _, m := range matchers {
		if m.Match(key) {
			return true
		}
	}
	return false
}

// isAllowedHeader returns true if the allowed header matchers list is empty,
// or if the given header key matches any of the provided allowed header
// matchers.
func isAllowedHeader(key string, matchers []matcher.StringMatcher) bool {
	if len(matchers) == 0 {
		return true
	}
	for _, m := range matchers {
		if m.Match(key) {
			return true
		}
	}
	return false
}

// principalFromCert extracts the principal from the given certificate. This
// will be set to the cert's first URI SAN if set, otherwise the cert's first
// DNS SAN if set, otherwise the subject field of the certificate in RFC 2253
// format.
func principalFromCert(cert *x509.Certificate) string {
	if len(cert.URIs) > 0 {
		return cert.URIs[0].String()
	}
	if len(cert.DNSNames) > 0 {
		return cert.DNSNames[0]
	}
	return cert.Subject.String()
}
