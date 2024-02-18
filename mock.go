package grpcstub

import (
	"context"
	"fmt"
	"net"

	"github.com/k1LoW/grpcstub/testdata/routeguide"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

type MockServer struct {
	gs          *grpc.Server
	lis         net.Listener
	methodInfos methodInfo
}

type methodInfo struct {
	serverName string
	methodName string
	request    protoreflect.ProtoMessage
	response   protoreflect.ProtoMessage
}

func NewMockServer() *MockServer {
	s := &MockServer{
		methodInfos: methodInfo{
			serverName: "routeguide.RouteGuide",
			methodName: "GetFeature",
			request:    new(routeguide.Point),
			response:   new(routeguide.Feature),
		},
	}
	gs := grpc.NewServer()
	gs.RegisterService(
		&grpc.ServiceDesc{
			ServiceName: s.methodInfos.serverName,
			Methods: []grpc.MethodDesc{
				{
					MethodName: s.methodInfos.methodName,
					Handler:    s.newUnaryHandler(),
				},
			},
		}, nil)
	s.gs = gs
	return s
}

func (s *MockServer) Start() error {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	s.lis = lis
	go s.gs.Serve(lis)
	return nil
}

func (s *MockServer) Conn() (*grpc.ClientConn, error) {
	return grpc.Dial(s.lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
}

func (s *MockServer) newUnaryHandler() func(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	return func(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
		in := dynamicpb.NewMessage(s.methodInfos.request.ProtoReflect().Descriptor())
		if err := dec(in); err != nil {
			return nil, err
		}
		fmt.Println("😀")
		out := dynamicpb.NewMessage(s.methodInfos.response.ProtoReflect().Descriptor())
		if err := protojson.Unmarshal([]byte(`{"name":"hello"}`), out); err != nil {
			return nil, err
		}
		return out, nil
	}
}
