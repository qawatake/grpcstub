package grpcstub

import (
	"context"
	"fmt"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

type MockServer struct {
	gs  *grpc.Server
	lis net.Listener
	// methodInfo  methodInfo
	// methodInfos []methodInfo
	matchers []*Matcher
}

type methodInfo struct {
	serverName string
	methodName string
	request    protoreflect.ProtoMessage
	response   protoreflect.ProtoMessage
}

func NewMockServer() (*MockServer, error) {
	s := &MockServer{
		// methodInfo: methodInfo{
		// 	serverName: "routeguide.RouteGuide",
		// 	methodName: "GetFeature",
		// 	request:    new(routeguide.Point),
		// 	response:   new(routeguide.Feature),
		// },
	}
	gs := grpc.NewServer()
	// gs.RegisterService(
	// 	&grpc.ServiceDesc{
	// 		ServiceName: s.methodInfo.serverName,
	// 		Methods: []grpc.MethodDesc{
	// 			{
	// 				MethodName: s.methodInfo.methodName,
	// 				Handler:    s.newUnaryHandler(),
	// 			},
	// 		},
	// 	}, nil)
	s.gs = gs
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s.lis = lis
	return s, nil
}

func (s *MockServer) Start() error {
	go s.gs.Serve(s.lis)
	return nil
}

func (s *MockServer) Conn() (*grpc.ClientConn, error) {
	return grpc.Dial(s.lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
}

func (s *MockServer) Method(serverName, methodName string, reqType protoreflect.ProtoMessage, resType protoreflect.ProtoMessage) *Matcher {
	m := &Matcher{
		ServerName:   serverName,
		MethodName:   methodName,
		RequestType:  reqType,
		ResponseType: resType,
	}
	s.matchers = append(s.matchers, m)
	s.gs.RegisterService(
		&grpc.ServiceDesc{
			ServiceName: m.ServerName,
			Methods: []grpc.MethodDesc{
				{
					MethodName: m.MethodName,
					Handler:    s.newUnaryHandler(m),
				},
			},
		}, nil)
	return m
}

type Matcher struct {
	ServerName   string
	MethodName   string
	RequestType  protoreflect.ProtoMessage
	ResponseType protoreflect.ProtoMessage
	response     protoreflect.ProtoMessage
}

func (m *Matcher) Response(v protoreflect.ProtoMessage) {
	m.response = v
}

func (s *MockServer) newUnaryHandler(matcher *Matcher) func(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	return func(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
		// var matcher *Matcher
		// for _, m := range s.matchers {
		// 	if m.ServerName == serverName && m.MethodName == methodName {
		// 		matcher = m
		// 		break
		// 	}
		// }
		// if matcher == nil {
		// 	return nil, fmt.Errorf("no matcher found")
		// }

		// in := dynamicpb.NewMessage(s.methodInfo.request.ProtoReflect().Descriptor())
		in := dynamicpb.NewMessage(matcher.RequestType.ProtoReflect().Descriptor())
		if err := dec(in); err != nil {
			return nil, err
		}
		fmt.Println("😀")
		if matcher.response != nil {
			return matcher.response, nil
		}
		out := dynamicpb.NewMessage(matcher.response.ProtoReflect().Descriptor())
		// out := dynamicpb.NewMessage(s.methodInfo.response.ProtoReflect().Descriptor())
		if err := protojson.Unmarshal([]byte(`{"name":"hello"}`), out); err != nil {
			return nil, err
		}
		return out, nil
	}
}
