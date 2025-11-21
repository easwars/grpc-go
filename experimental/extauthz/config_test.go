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
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/internal/grpctest"
	"google.golang.org/grpc/metadata"

	v3corepb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
)

const defaultTestTimeout = 5 * time.Second

type s struct {
	grpctest.Tester
}

func Test(t *testing.T) {
	grpctest.RunSubTests(t, s{})
}

func (s) TestHeaderMutationRules_Apply(t *testing.T) {
	tests := []struct {
		name    string
		hmr     *HeaderMutationRules
		hvos    []*v3corepb.HeaderValueOption
		inputMD metadata.MD
		wantMD  metadata.MD
		wantErr bool
	}{
		{
			name: "NilReceiver",
			hmr:  nil,
			hvos: []*v3corepb.HeaderValueOption{
				{Header: &v3corepb.HeaderValue{Key: "a", Value: "1"}, AppendAction: v3corepb.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD},
			},
			inputMD: metadata.MD{},
			wantMD:  metadata.MD{"a": []string{"1"}},
		},
		{
			name: "DisallowAll",
			hmr:  &HeaderMutationRules{DisallowAll: true},
			hvos: []*v3corepb.HeaderValueOption{
				{Header: &v3corepb.HeaderValue{Key: "a", Value: "1"}, AppendAction: v3corepb.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD},
			},
			inputMD: metadata.MD{"x": []string{"y"}},
			wantMD:  metadata.MD{"x": []string{"y"}},
		},
		{
			name: "DisallowExprMatchAndDisallowIsErrorIsFalse",
			hmr:  &HeaderMutationRules{DisallowExpr: regexp.MustCompile("^a$")},
			hvos: []*v3corepb.HeaderValueOption{
				{Header: &v3corepb.HeaderValue{Key: "a", Value: "1"}, AppendAction: v3corepb.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD},
			},
			inputMD: metadata.MD{},
			wantMD:  metadata.MD{},
		},
		{
			name: "DisallowExprMatchAndDisallowIsErrorIsTrue",
			hmr:  &HeaderMutationRules{DisallowExpr: regexp.MustCompile("^a$"), DisallowIsError: true},
			hvos: []*v3corepb.HeaderValueOption{
				{Header: &v3corepb.HeaderValue{Key: "a", Value: "1"}, AppendAction: v3corepb.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD},
			},
			inputMD: metadata.MD{},
			wantErr: true,
		},
		{
			name: "AllowExprMatch",
			hmr:  &HeaderMutationRules{AllowExpr: regexp.MustCompile("^a$")},
			hvos: []*v3corepb.HeaderValueOption{
				{Header: &v3corepb.HeaderValue{Key: "a", Value: "1"}, AppendAction: v3corepb.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD},
				{Header: &v3corepb.HeaderValue{Key: "b", Value: "2"}, AppendAction: v3corepb.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD},
			},
			inputMD: metadata.MD{},
			wantMD:  metadata.MD{"a": []string{"1"}},
		},
		{
			name: "AllowExprNoMatch",
			hmr:  &HeaderMutationRules{AllowExpr: regexp.MustCompile("^a$")},
			hvos: []*v3corepb.HeaderValueOption{
				{Header: &v3corepb.HeaderValue{Key: "b", Value: "2"}, AppendAction: v3corepb.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD},
			},
			inputMD: metadata.MD{},
			wantMD:  metadata.MD{},
		},
		{
			name: "DisallowOverridesAllow",
			hmr:  &HeaderMutationRules{AllowExpr: regexp.MustCompile("."), DisallowExpr: regexp.MustCompile("^a$")},
			hvos: []*v3corepb.HeaderValueOption{
				{Header: &v3corepb.HeaderValue{Key: "a", Value: "1"}, AppendAction: v3corepb.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD},
				{Header: &v3corepb.HeaderValue{Key: "b", Value: "2"}, AppendAction: v3corepb.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD},
			},
			inputMD: metadata.MD{},
			wantMD:  metadata.MD{"b": []string{"2"}},
		},
		{
			name: "InvalidHeaderKeyPseudoHeader",
			hmr:  &HeaderMutationRules{},
			hvos: []*v3corepb.HeaderValueOption{
				{Header: &v3corepb.HeaderValue{Key: ":path", Value: "/"}, AppendAction: v3corepb.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD},
			},
			inputMD: metadata.MD{},
			wantMD:  metadata.MD{},
		},
		{
			name: "InvalidHeaderKeyHost",
			hmr:  &HeaderMutationRules{},
			hvos: []*v3corepb.HeaderValueOption{
				{Header: &v3corepb.HeaderValue{Key: "host", Value: "example.com"}, AppendAction: v3corepb.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD},
			},
			inputMD: metadata.MD{},
			wantMD:  metadata.MD{},
		},
		{
			name: "InvalidHeaderKeyNotLowercase",
			hmr:  &HeaderMutationRules{},
			hvos: []*v3corepb.HeaderValueOption{
				{Header: &v3corepb.HeaderValue{Key: "A", Value: "1"}, AppendAction: v3corepb.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD},
			},
			inputMD: metadata.MD{},
			wantMD:  metadata.MD{},
		},
		{
			name: "InvalidHeaderKeyTooLong",
			hmr:  &HeaderMutationRules{},
			hvos: []*v3corepb.HeaderValueOption{
				{Header: &v3corepb.HeaderValue{Key: strings.Repeat("a", 16385), Value: "1"}, AppendAction: v3corepb.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD},
			},
			inputMD: metadata.MD{},
			wantMD:  metadata.MD{},
		},
		{
			name: "InvalidHeaderValueTooLong",
			hmr:  &HeaderMutationRules{},
			hvos: []*v3corepb.HeaderValueOption{
				{Header: &v3corepb.HeaderValue{Key: "a", Value: strings.Repeat("1", 16385)}, AppendAction: v3corepb.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD},
			},
			inputMD: metadata.MD{},
			wantMD:  metadata.MD{},
		},
		{
			name: "InvalidBinaryHeaderValueTooLong",
			hmr:  &HeaderMutationRules{},
			hvos: []*v3corepb.HeaderValueOption{
				{Header: &v3corepb.HeaderValue{Key: "a-bin", RawValue: make([]byte, 16385)}, AppendAction: v3corepb.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD},
			},
			inputMD: metadata.MD{},
			wantMD:  metadata.MD{},
		},
		{
			name: "BinaryHeaderValue",
			hmr:  &HeaderMutationRules{},
			hvos: []*v3corepb.HeaderValueOption{
				{Header: &v3corepb.HeaderValue{Key: "a-bin", RawValue: []byte{1, 2, 3}}, AppendAction: v3corepb.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD},
			},
			inputMD: metadata.MD{},
			wantMD:  metadata.MD{"a-bin": []string{string([]byte{1, 2, 3})}},
		},
		{
			name: "AppendIfExistsOrAdd_Exists",
			hmr:  &HeaderMutationRules{},
			hvos: []*v3corepb.HeaderValueOption{
				{Header: &v3corepb.HeaderValue{Key: "a", Value: "1"}, AppendAction: v3corepb.HeaderValueOption_APPEND_IF_EXISTS_OR_ADD},
			},
			inputMD: metadata.MD{"a": []string{"0"}},
			wantMD:  metadata.MD{"a": []string{"0", "1"}},
		},
		{
			name: "AppendIfExistsOrAdd_Add",
			hmr:  &HeaderMutationRules{},
			hvos: []*v3corepb.HeaderValueOption{
				{Header: &v3corepb.HeaderValue{Key: "a", Value: "1"}, AppendAction: v3corepb.HeaderValueOption_APPEND_IF_EXISTS_OR_ADD},
			},
			inputMD: metadata.MD{},
			wantMD:  metadata.MD{"a": []string{"1"}},
		},
		{
			name: "AddIfAbsent_Absent",
			hmr:  &HeaderMutationRules{},
			hvos: []*v3corepb.HeaderValueOption{
				{Header: &v3corepb.HeaderValue{Key: "a", Value: "1"}, AppendAction: v3corepb.HeaderValueOption_ADD_IF_ABSENT},
			},
			inputMD: metadata.MD{},
			wantMD:  metadata.MD{"a": []string{"1"}},
		},
		{
			name: "AddIfAbsent_Present",
			hmr:  &HeaderMutationRules{},
			hvos: []*v3corepb.HeaderValueOption{
				{Header: &v3corepb.HeaderValue{Key: "a", Value: "1"}, AppendAction: v3corepb.HeaderValueOption_ADD_IF_ABSENT},
			},
			inputMD: metadata.MD{"a": []string{"1"}},
			wantMD:  metadata.MD{"a": []string{"1"}},
		},
		{
			name: "OverwriteIfExistsOrAdd_Absent",
			hmr:  &HeaderMutationRules{},
			hvos: []*v3corepb.HeaderValueOption{
				{Header: &v3corepb.HeaderValue{Key: "a", Value: "1"}, AppendAction: v3corepb.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD},
			},
			inputMD: metadata.MD{},
			wantMD:  metadata.MD{"a": []string{"1"}},
		},
		{
			name: "OverwriteIfExistsOrAdd_Present",
			hmr:  &HeaderMutationRules{},
			hvos: []*v3corepb.HeaderValueOption{
				{Header: &v3corepb.HeaderValue{Key: "a", Value: "1"}, AppendAction: v3corepb.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD},
			},
			inputMD: metadata.MD{"a": []string{"0"}},
			wantMD:  metadata.MD{"a": []string{"1"}},
		},
		{
			name: "OverwriteIfExists_Absent",
			hmr:  &HeaderMutationRules{},
			hvos: []*v3corepb.HeaderValueOption{
				{Header: &v3corepb.HeaderValue{Key: "a", Value: "1"}, AppendAction: v3corepb.HeaderValueOption_OVERWRITE_IF_EXISTS},
			},
			inputMD: metadata.MD{},
			wantMD:  metadata.MD{},
		},
		{
			name: "OverwriteIfExists_Present",
			hmr:  &HeaderMutationRules{},
			hvos: []*v3corepb.HeaderValueOption{
				{Header: &v3corepb.HeaderValue{Key: "a", Value: "1"}, AppendAction: v3corepb.HeaderValueOption_OVERWRITE_IF_EXISTS},
			},
			inputMD: metadata.MD{"a": []string{"0"}},
			wantMD:  metadata.MD{"a": []string{"1"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotMD, err := tt.hmr.ApplyAdditions(tt.hvos, tt.inputMD)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Apply() error = %v, wantErr %v", err, tt.wantErr)
			}
			if diff := cmp.Diff(tt.wantMD, gotMD); diff != "" {
				t.Errorf("Apply() returned diff in metadata (-want +got):\n%s", diff)
			}
		})
	}
}

// TODO: Add tests for ApplyRemovals.

// TODO: Add tests for configuration parsing and validation.

// TODO: See if this file can be merged with ext_authz_test.go, thereby living in a test package.
