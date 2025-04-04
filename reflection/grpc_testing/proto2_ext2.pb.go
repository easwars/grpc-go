// Copyright 2017 gRPC authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Code generated by protoc-gen-go. DO NOT EDIT.
// versions:
// 	protoc-gen-go v1.36.6
// 	protoc        v5.27.1
// source: reflection/grpc_testing/proto2_ext2.proto

package grpc_testing

import (
	protoreflect "google.golang.org/protobuf/reflect/protoreflect"
	protoimpl "google.golang.org/protobuf/runtime/protoimpl"
	reflect "reflect"
	sync "sync"
	unsafe "unsafe"
)

const (
	// Verify that this generated code is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(20 - protoimpl.MinVersion)
	// Verify that runtime/protoimpl is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(protoimpl.MaxVersion - 20)
)

type AnotherExtension struct {
	state           protoimpl.MessageState `protogen:"open.v1"`
	Whatchamacallit *int32                 `protobuf:"varint,1,opt,name=whatchamacallit" json:"whatchamacallit,omitempty"`
	unknownFields   protoimpl.UnknownFields
	sizeCache       protoimpl.SizeCache
}

func (x *AnotherExtension) Reset() {
	*x = AnotherExtension{}
	mi := &file_reflection_grpc_testing_proto2_ext2_proto_msgTypes[0]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *AnotherExtension) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*AnotherExtension) ProtoMessage() {}

func (x *AnotherExtension) ProtoReflect() protoreflect.Message {
	mi := &file_reflection_grpc_testing_proto2_ext2_proto_msgTypes[0]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use AnotherExtension.ProtoReflect.Descriptor instead.
func (*AnotherExtension) Descriptor() ([]byte, []int) {
	return file_reflection_grpc_testing_proto2_ext2_proto_rawDescGZIP(), []int{0}
}

func (x *AnotherExtension) GetWhatchamacallit() int32 {
	if x != nil && x.Whatchamacallit != nil {
		return *x.Whatchamacallit
	}
	return 0
}

var file_reflection_grpc_testing_proto2_ext2_proto_extTypes = []protoimpl.ExtensionInfo{
	{
		ExtendedType:  (*ToBeExtended)(nil),
		ExtensionType: (*string)(nil),
		Field:         23,
		Name:          "grpc.testing.frob",
		Tag:           "bytes,23,opt,name=frob",
		Filename:      "reflection/grpc_testing/proto2_ext2.proto",
	},
	{
		ExtendedType:  (*ToBeExtended)(nil),
		ExtensionType: (*AnotherExtension)(nil),
		Field:         29,
		Name:          "grpc.testing.nitz",
		Tag:           "bytes,29,opt,name=nitz",
		Filename:      "reflection/grpc_testing/proto2_ext2.proto",
	},
}

// Extension fields to ToBeExtended.
var (
	// optional string frob = 23;
	E_Frob = &file_reflection_grpc_testing_proto2_ext2_proto_extTypes[0]
	// optional grpc.testing.AnotherExtension nitz = 29;
	E_Nitz = &file_reflection_grpc_testing_proto2_ext2_proto_extTypes[1]
)

var File_reflection_grpc_testing_proto2_ext2_proto protoreflect.FileDescriptor

const file_reflection_grpc_testing_proto2_ext2_proto_rawDesc = "" +
	"\n" +
	")reflection/grpc_testing/proto2_ext2.proto\x12\fgrpc.testing\x1a$reflection/grpc_testing/proto2.proto\"<\n" +
	"\x10AnotherExtension\x12(\n" +
	"\x0fwhatchamacallit\x18\x01 \x01(\x05R\x0fwhatchamacallit:.\n" +
	"\x04frob\x12\x1a.grpc.testing.ToBeExtended\x18\x17 \x01(\tR\x04frob:N\n" +
	"\x04nitz\x12\x1a.grpc.testing.ToBeExtended\x18\x1d \x01(\v2\x1e.grpc.testing.AnotherExtensionR\x04nitzB0Z.google.golang.org/grpc/reflection/grpc_testing"

var (
	file_reflection_grpc_testing_proto2_ext2_proto_rawDescOnce sync.Once
	file_reflection_grpc_testing_proto2_ext2_proto_rawDescData []byte
)

func file_reflection_grpc_testing_proto2_ext2_proto_rawDescGZIP() []byte {
	file_reflection_grpc_testing_proto2_ext2_proto_rawDescOnce.Do(func() {
		file_reflection_grpc_testing_proto2_ext2_proto_rawDescData = protoimpl.X.CompressGZIP(unsafe.Slice(unsafe.StringData(file_reflection_grpc_testing_proto2_ext2_proto_rawDesc), len(file_reflection_grpc_testing_proto2_ext2_proto_rawDesc)))
	})
	return file_reflection_grpc_testing_proto2_ext2_proto_rawDescData
}

var file_reflection_grpc_testing_proto2_ext2_proto_msgTypes = make([]protoimpl.MessageInfo, 1)
var file_reflection_grpc_testing_proto2_ext2_proto_goTypes = []any{
	(*AnotherExtension)(nil), // 0: grpc.testing.AnotherExtension
	(*ToBeExtended)(nil),     // 1: grpc.testing.ToBeExtended
}
var file_reflection_grpc_testing_proto2_ext2_proto_depIdxs = []int32{
	1, // 0: grpc.testing.frob:extendee -> grpc.testing.ToBeExtended
	1, // 1: grpc.testing.nitz:extendee -> grpc.testing.ToBeExtended
	0, // 2: grpc.testing.nitz:type_name -> grpc.testing.AnotherExtension
	3, // [3:3] is the sub-list for method output_type
	3, // [3:3] is the sub-list for method input_type
	2, // [2:3] is the sub-list for extension type_name
	0, // [0:2] is the sub-list for extension extendee
	0, // [0:0] is the sub-list for field type_name
}

func init() { file_reflection_grpc_testing_proto2_ext2_proto_init() }
func file_reflection_grpc_testing_proto2_ext2_proto_init() {
	if File_reflection_grpc_testing_proto2_ext2_proto != nil {
		return
	}
	file_reflection_grpc_testing_proto2_proto_init()
	type x struct{}
	out := protoimpl.TypeBuilder{
		File: protoimpl.DescBuilder{
			GoPackagePath: reflect.TypeOf(x{}).PkgPath(),
			RawDescriptor: unsafe.Slice(unsafe.StringData(file_reflection_grpc_testing_proto2_ext2_proto_rawDesc), len(file_reflection_grpc_testing_proto2_ext2_proto_rawDesc)),
			NumEnums:      0,
			NumMessages:   1,
			NumExtensions: 2,
			NumServices:   0,
		},
		GoTypes:           file_reflection_grpc_testing_proto2_ext2_proto_goTypes,
		DependencyIndexes: file_reflection_grpc_testing_proto2_ext2_proto_depIdxs,
		MessageInfos:      file_reflection_grpc_testing_proto2_ext2_proto_msgTypes,
		ExtensionInfos:    file_reflection_grpc_testing_proto2_ext2_proto_extTypes,
	}.Build()
	File_reflection_grpc_testing_proto2_ext2_proto = out.File
	file_reflection_grpc_testing_proto2_ext2_proto_goTypes = nil
	file_reflection_grpc_testing_proto2_ext2_proto_depIdxs = nil
}
