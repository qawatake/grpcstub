package grpcstub

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhump/protoreflect/desc"
	"github.com/jhump/protoreflect/desc/protoparse"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

type serverStatus int

const (
	status_unknown serverStatus = iota
	status_start
	status_starting
	status_closing
	status_closed
)

const (
	HealthCheckService_DEFAULT  = "default"
	HealthCheckService_FLAPPING = "flapping"
)

type Message map[string]interface{}

type Request struct {
	Service string
	Method  string
	Headers metadata.MD
	Message Message
}

func newRequest(md protoreflect.MethodDescriptor, message Message) *Request {
	service, method := splitMethodFullName(md.FullName())
	return &Request{
		Service: service,
		Method:  method,
		Headers: metadata.MD{},
		Message: message,
	}
}

type Response struct {
	Headers  metadata.MD
	Messages []Message
	Trailers metadata.MD
	Status   *status.Status
}

// NewResponse returns a new empty response
func NewResponse() *Response {
	return &Response{
		Headers:  metadata.MD{},
		Messages: []Message{},
		Trailers: metadata.MD{},
		Status:   nil,
	}
}

type Server struct {
	matchers    []*matcher
	fds         *descriptorpb.FileDescriptorSet
	listener    net.Listener
	server      *grpc.Server
	tlsc        *tls.Config
	cacert      []byte
	cc          *grpc.ClientConn
	requests    []*Request
	healthCheck bool
	status      serverStatus
	t           *testing.T
	mu          sync.RWMutex
}

type matcher struct {
	matchFuncs []matchFunc
	handler    handlerFunc
	requests   []*Request
	mu         sync.RWMutex
}

type matchFunc func(r *Request) bool
type handlerFunc func(r *Request, md protoreflect.MethodDescriptor) *Response

// NewServer returns a new server with registered *grpc.Server
func NewServer(t *testing.T, protopath string, opts ...Option) *Server {
	t.Helper()
	c := &config{}
	opts = append(opts, Proto(protopath))
	for _, opt := range opts {
		if err := opt(c); err != nil {
			t.Fatal(err)
		}
	}
	fds, err := descriptorFromFiles(c.importPaths, c.protos...)
	if err != nil {
		t.Error(err)
		return nil
	}
	s := &Server{
		fds:         fds,
		t:           t,
		healthCheck: c.healthCheck,
	}
	if c.useTLS {
		certificate, err := tls.X509KeyPair(c.cert, c.key)
		if err != nil {
			t.Fatal(err)
		}
		tlsc := &tls.Config{
			Certificates: []tls.Certificate{certificate},
		}
		creds := credentials.NewTLS(tlsc)
		s.tlsc = tlsc
		s.cacert = c.cacert
		s.server = grpc.NewServer(grpc.Creds(creds))
	} else {
		s.server = grpc.NewServer()
	}
	s.startServer()
	return s
}

// NewTLSServer returns a new server with registered secure *grpc.Server
func NewTLSServer(t *testing.T, proto string, cacert, cert, key []byte, opts ...Option) *Server {
	opts = append(opts, UseTLS(cacert, cert, key))
	return NewServer(t, proto, opts...)
}

// Close shuts down *grpc.Server
func (s *Server) Close() {
	s.status = status_closing
	defer func() {
		s.status = status_closed
	}()
	s.t.Helper()
	if s.listener == nil {
		s.t.Error("server is not started yet")
		return
	}
	if s.cc != nil {
		_ = s.cc.Close()
		s.cc = nil
	}
	done := make(chan struct{})
	go func() {
		s.server.GracefulStop()
		close(done)
	}()
	t := time.NewTimer(5 * time.Second)
	select {
	case <-done:
		if !t.Stop() {
			<-t.C
		}
	case <-t.C:
		s.server.Stop()
	}
}

// Addr returns server listener address
func (s *Server) Addr() string {
	s.t.Helper()
	if s.listener == nil {
		s.t.Error("server is not started yet")
		return ""
	}
	return s.listener.Addr().String()
}

// Conn returns *grpc.ClientConn which connects *grpc.Server.
func (s *Server) Conn() *grpc.ClientConn {
	s.t.Helper()
	if s.listener == nil {
		s.t.Error("server is not started yet")
		return nil
	}
	var creds credentials.TransportCredentials
	if s.tlsc == nil {
		creds = insecure.NewCredentials()
	} else {
		if s.cacert == nil {
			s.tlsc.InsecureSkipVerify = true
		} else {
			pool := x509.NewCertPool()
			if ok := pool.AppendCertsFromPEM(s.cacert); !ok {
				s.t.Fatal(errors.New("failed to append ca certs"))
			}
			s.tlsc.RootCAs = pool
		}
		creds = credentials.NewTLS(s.tlsc)
	}
	conn, err := grpc.Dial(
		s.listener.Addr().String(),
		grpc.WithTransportCredentials(creds),
	)
	if err != nil {
		s.t.Error(err)
		return nil
	}
	s.cc = conn
	return conn
}

// ClientConn is alias of Conn
func (s *Server) ClientConn() *grpc.ClientConn {
	return s.Conn()
}

func (s *Server) startServer() {
	s.status = status_starting
	defer func() {
		s.status = status_start
	}()
	s.t.Helper()
	reflection.Register(s.server)
	s.registerServer()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		s.t.Error(err)
		return
	}
	s.listener = l
	go func() {
		_ = s.server.Serve(l)
	}()
}

// Match create request matcher with matchFunc (func(r *grpcstub.Request) bool).
func (s *Server) Match(fn func(r *Request) bool) *matcher {
	m := &matcher{
		matchFuncs: []matchFunc{fn},
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.matchers = append(s.matchers, m)
	return m
}

// Match append matchFunc (func(r *grpcstub.Request) bool) to request matcher.
func (m *matcher) Match(fn func(r *Request) bool) *matcher {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.matchFuncs = append(m.matchFuncs, fn)
	return m
}

// Service create request matcher using service.
func (s *Server) Service(service string) *matcher {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn := serviceMatchFunc(service)
	m := &matcher{
		matchFuncs: []matchFunc{fn},
	}
	s.matchers = append(s.matchers, m)
	return m
}

// Service append request matcher using service.
func (m *matcher) Service(service string) *matcher {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn := serviceMatchFunc(service)
	m.matchFuncs = append(m.matchFuncs, fn)
	return m
}

// Servicef create request matcher using sprintf-ed service.
func (s *Server) Servicef(format string, a ...any) *matcher {
	return s.Service(fmt.Sprintf(format, a...))
}

// Servicef append request matcher using sprintf-ed service.
func (m *matcher) Servicef(format string, a ...any) *matcher {
	return m.Service(fmt.Sprintf(format, a...))
}

// Method create request matcher using method.
func (s *Server) Method(method string) *matcher {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn := methodMatchFunc(method)
	m := &matcher{
		matchFuncs: []matchFunc{fn},
	}
	s.matchers = append(s.matchers, m)
	return m
}

// Method append request matcher using method.
func (m *matcher) Method(method string) *matcher {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn := methodMatchFunc(method)
	m.matchFuncs = append(m.matchFuncs, fn)
	return m
}

// Methodf create request matcher using sprintf-ed method.
func (s *Server) Methodf(format string, a ...any) *matcher {
	return s.Method(fmt.Sprintf(format, a...))
}

// Methodf append request matcher using sprintf-ed method.
func (m *matcher) Methodf(format string, a ...any) *matcher {
	return m.Method(fmt.Sprintf(format, a...))
}

// Header append handler which append header to response.
func (m *matcher) Header(key, value string) *matcher {
	prev := m.handler
	m.handler = func(r *Request, md protoreflect.MethodDescriptor) *Response {
		var res *Response
		if prev == nil {
			res = NewResponse()
		} else {
			res = prev(r, md)
		}
		res.Headers.Append(key, value)
		return res
	}
	return m
}

// Trailer append handler which append trailer to response.
func (m *matcher) Trailer(key, value string) *matcher {
	prev := m.handler
	m.handler = func(r *Request, md protoreflect.MethodDescriptor) *Response {
		var res *Response
		if prev == nil {
			res = NewResponse()
		} else {
			res = prev(r, md)
		}
		res.Trailers.Append(key, value)
		return res
	}
	return m
}

// Handler set handler
func (m *matcher) Handler(fn func(r *Request) *Response) {
	m.handler = func(r *Request, md protoreflect.MethodDescriptor) *Response {
		return fn(r)
	}
}

// Response set handler which return response.
func (m *matcher) Response(message map[string]interface{}) *matcher {
	prev := m.handler
	m.handler = func(r *Request, md protoreflect.MethodDescriptor) *Response {
		var res *Response
		if prev == nil {
			res = NewResponse()
		} else {
			res = prev(r, md)
		}
		res.Messages = append(res.Messages, message)
		return res
	}
	return m
}

// ResponseString set handler which return response.
func (m *matcher) ResponseString(message string) *matcher {
	mes := make(map[string]interface{})
	_ = json.Unmarshal([]byte(message), &mes)
	return m.Response(mes)
}

// ResponseStringf set handler which return sprintf-ed response.
func (m *matcher) ResponseStringf(format string, a ...any) *matcher {
	return m.ResponseString(fmt.Sprintf(format, a...))
}

// Status set handler which return response with status
func (m *matcher) Status(s *status.Status) *matcher {
	prev := m.handler
	m.handler = func(r *Request, md protoreflect.MethodDescriptor) *Response {
		var res *Response
		if prev == nil {
			res = NewResponse()
		} else {
			res = prev(r, md)
		}
		res.Status = s
		return res
	}
	return m
}

// Requests returns []*grpcstub.Request received by router.
func (s *Server) Requests() []*Request {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.requests
}

// Requests returns []*grpcstub.Request received by matcher.
func (m *matcher) Requests() []*Request {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.requests
}

func (s *Server) registerServer() {
	files := protoregistry.GlobalFiles
	for _, fd := range s.fds.File {
		d, err := protodesc.NewFile(fd, files)
		if err != nil {
			s.t.Error(err)
		}
		for i := 0; i < d.Services().Len(); i++ {
			s.server.RegisterService(s.createServiceDesc(d.Services().Get(i)), nil)
		}
	}
	if s.healthCheck {
		healthSrv := health.NewServer()
		healthpb.RegisterHealthServer(s.server, healthSrv)
		healthSrv.SetServingStatus(HealthCheckService_DEFAULT, healthpb.HealthCheckResponse_SERVING)
		go func() {
			status := healthpb.HealthCheckResponse_SERVING
			healthSrv.SetServingStatus(HealthCheckService_FLAPPING, status)
			for {
				switch s.status {
				case status_start, status_starting:
					if status == healthpb.HealthCheckResponse_SERVING {
						status = healthpb.HealthCheckResponse_NOT_SERVING
					} else {
						status = healthpb.HealthCheckResponse_SERVING
					}
					healthSrv.SetServingStatus(HealthCheckService_FLAPPING, status)
				}
				time.Sleep(100 * time.Millisecond)
			}
		}()
	}
}

func (s *Server) createServiceDesc(sd protoreflect.ServiceDescriptor) *grpc.ServiceDesc {
	gsd := &grpc.ServiceDesc{
		ServiceName: string(sd.FullName()),
		HandlerType: nil,
		Metadata:    sd.ParentFile().Name(),
	}

	mds := []protoreflect.MethodDescriptor{}
	for i := 0; i < sd.Methods().Len(); i++ {
		mds = append(mds, sd.Methods().Get(i))
	}

	gsd.Methods, gsd.Streams = s.createMethodDescs(mds)
	return gsd
}

func (s *Server) createMethodDescs(mds []protoreflect.MethodDescriptor) ([]grpc.MethodDesc, []grpc.StreamDesc) {
	var methods []grpc.MethodDesc
	var streams []grpc.StreamDesc
	for _, md := range mds {
		if !md.IsStreamingClient() && !md.IsStreamingServer() {
			method := grpc.MethodDesc{
				MethodName: string(md.Name()),
				Handler:    s.createUnaryHandler(md),
			}
			methods = append(methods, method)
		} else {
			stream := grpc.StreamDesc{
				StreamName:    string(md.Name()),
				Handler:       s.createStreamHandler(md),
				ServerStreams: md.IsStreamingServer(),
				ClientStreams: md.IsStreamingClient(),
			}
			streams = append(streams, stream)
		}
	}
	return methods, streams
}

func (s *Server) createUnaryHandler(md protoreflect.MethodDescriptor) func(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	return func(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
		in := dynamicpb.NewMessage(md.Input())
		if err := dec(in); err != nil {
			return nil, err
		}
		b, err := protojson.MarshalOptions{UseProtoNames: true, UseEnumNumbers: true, EmitUnpopulated: true}.Marshal(in)
		if err != nil {
			return nil, err
		}
		m := Message{}
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, err
		}

		r := newRequest(md, m)
		h, ok := metadata.FromIncomingContext(ctx)
		if ok {
			r.Headers = h
		}
		s.mu.Lock()
		s.requests = append(s.requests, r)
		s.mu.Unlock()

		var mes *dynamicpb.Message
		for _, m := range s.matchers {
			match := true
			for _, fn := range m.matchFuncs {
				if !fn(r) {
					match = false
				}
			}
			if match {
				m.mu.Lock()
				m.requests = append(m.requests, r)
				m.mu.Unlock()
				res := m.handler(r, md)
				for k, v := range res.Headers {
					for _, vv := range v {
						if err := grpc.SetHeader(ctx, metadata.Pairs(k, vv)); err != nil {
							return nil, err
						}
					}
				}
				for k, v := range res.Trailers {
					for _, vv := range v {
						if err := grpc.SetTrailer(ctx, metadata.Pairs(k, vv)); err != nil {
							return nil, err
						}
					}
				}
				if res.Status != nil && res.Status.Err() != nil {
					return nil, res.Status.Err()
				}
				mes = dynamicpb.NewMessage(md.Output())
				if len(res.Messages) > 0 {
					b, err := json.Marshal(res.Messages[0])
					if err != nil {
						return nil, err
					}
					if err := (protojson.UnmarshalOptions{}).Unmarshal(b, mes); err != nil {
						return nil, err
					}
				}
				return mes, nil
			}
		}

		return mes, status.Error(codes.NotFound, codes.NotFound.String())
	}
}

func (s *Server) createStreamHandler(md protoreflect.MethodDescriptor) func(srv interface{}, stream grpc.ServerStream) error {
	switch {
	case !md.IsStreamingClient() && md.IsStreamingServer():
		return s.createServerStreamingHandler(md)
	case md.IsStreamingClient() && !md.IsStreamingServer():
		return s.createClientStreamingHandler(md)
	case md.IsStreamingClient() && md.IsStreamingServer():
		return s.createBiStreamingHandler(md)
	default:
		return func(srv interface{}, stream grpc.ServerStream) error {
			return nil
		}
	}
}

func (s *Server) createServerStreamingHandler(md protoreflect.MethodDescriptor) func(srv interface{}, stream grpc.ServerStream) error {
	return func(srv interface{}, stream grpc.ServerStream) error {
		in := dynamicpb.NewMessage(md.Input())
		if err := stream.RecvMsg(in); err != nil {
			return err
		}
		b, err := protojson.MarshalOptions{UseProtoNames: true, UseEnumNumbers: true, EmitUnpopulated: true}.Marshal(in)
		if err != nil {
			return err
		}
		m := Message{}
		if err := json.Unmarshal(b, &m); err != nil {
			return err
		}
		r := newRequest(md, m)
		h, ok := metadata.FromIncomingContext(stream.Context())
		if ok {
			r.Headers = h
		}
		s.mu.Lock()
		s.requests = append(s.requests, r)
		s.mu.Unlock()
		for _, m := range s.matchers {
			match := true
			for _, fn := range m.matchFuncs {
				if !fn(r) {
					match = false
				}
			}
			if match {
				m.mu.Lock()
				m.requests = append(m.requests, r)
				m.mu.Unlock()
				res := m.handler(r, md)
				for k, v := range res.Headers {
					for _, vv := range v {
						if err := stream.SendHeader(metadata.Pairs(k, vv)); err != nil {
							return err
						}
					}
				}
				for k, v := range res.Trailers {
					for _, vv := range v {
						stream.SetTrailer(metadata.Pairs(k, vv))
					}
				}
				if res.Status != nil && res.Status.Err() != nil {
					return res.Status.Err()
				}
				if len(res.Messages) > 0 {
					for _, resm := range res.Messages {
						mes := dynamicpb.NewMessage(md.Output())
						b, err := json.Marshal(resm)
						if err != nil {
							return err
						}
						if err := (protojson.UnmarshalOptions{}).Unmarshal(b, mes); err != nil {
							return err
						}
						if err := stream.SendMsg(mes); err != nil {
							return err
						}
					}
				}
			}
		}
		return nil
	}
}

func (s *Server) createClientStreamingHandler(md protoreflect.MethodDescriptor) func(srv interface{}, stream grpc.ServerStream) error {
	return func(srv interface{}, stream grpc.ServerStream) error {
		rs := []*Request{}
		for {
			in := dynamicpb.NewMessage(md.Input())
			err := stream.RecvMsg(in)
			if err == nil {
				b, err := protojson.MarshalOptions{UseProtoNames: true, UseEnumNumbers: true, EmitUnpopulated: true}.Marshal(in)
				if err != nil {
					return err
				}
				m := Message{}
				if err := json.Unmarshal(b, &m); err != nil {
					return err
				}
				r := newRequest(md, m)
				h, ok := metadata.FromIncomingContext(stream.Context())
				if ok {
					r.Headers = h
				}
				s.mu.Lock()
				s.requests = append(s.requests, r)
				s.mu.Unlock()
				rs = append(rs, r)
			}
			if err == io.EOF {
				var mes *dynamicpb.Message
				for _, r := range rs {
					for _, m := range s.matchers {
						match := true
						for _, fn := range m.matchFuncs {
							if !fn(r) {
								match = false
							}
						}
						if match {
							m.mu.Lock()
							m.requests = append(m.requests, r)
							m.mu.Unlock()
							res := m.handler(r, md)
							if res.Status != nil && res.Status.Err() != nil {
								return res.Status.Err()
							}
							mes = dynamicpb.NewMessage(md.Output())
							if len(res.Messages) > 0 {
								b, err := json.Marshal(res.Messages[0])
								if err != nil {
									return err
								}
								if err := (protojson.UnmarshalOptions{}).Unmarshal(b, mes); err != nil {
									return err
								}
							}
							for k, v := range res.Headers {
								for _, vv := range v {
									if err := stream.SendHeader(metadata.Pairs(k, vv)); err != nil {
										return err
									}
								}
							}
							for k, v := range res.Trailers {
								for _, vv := range v {
									stream.SetTrailer((metadata.Pairs(k, vv)))
								}
							}
							return stream.SendMsg(mes)
						}
					}
				}
				return status.Error(codes.NotFound, codes.NotFound.String())
			}
		}
	}
}

func (s *Server) createBiStreamingHandler(md protoreflect.MethodDescriptor) func(srv interface{}, stream grpc.ServerStream) error {
	return func(srv interface{}, stream grpc.ServerStream) error {
		headerSent := false
	L:
		for {
			in := dynamicpb.NewMessage(md.Input())
			err := stream.RecvMsg(in)
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			b, err := protojson.MarshalOptions{UseProtoNames: true, UseEnumNumbers: true, EmitUnpopulated: true}.Marshal(in)
			if err != nil {
				return err
			}
			m := Message{}
			if err := json.Unmarshal(b, &m); err != nil {
				return err
			}
			r := newRequest(md, m)
			h, ok := metadata.FromIncomingContext(stream.Context())
			if ok {
				r.Headers = h
			}
			s.mu.Lock()
			s.requests = append(s.requests, r)
			s.mu.Unlock()
			for _, m := range s.matchers {
				match := true
				for _, fn := range m.matchFuncs {
					if !fn(r) {
						match = false
					}
				}
				if match {
					m.mu.Lock()
					m.requests = append(m.requests, r)
					m.mu.Unlock()
					res := m.handler(r, md)
					if !headerSent {
						for k, v := range res.Headers {
							for _, vv := range v {
								if err := stream.SendHeader(metadata.Pairs(k, vv)); err != nil {
									return err
								}
								headerSent = true
							}
						}
					}
					for k, v := range res.Trailers {
						for _, vv := range v {
							stream.SetTrailer(metadata.Pairs(k, vv))
						}
					}
					if res.Status != nil && res.Status.Err() != nil {
						return res.Status.Err()
					}
					if len(res.Messages) > 0 {
						for _, resm := range res.Messages {
							mes := dynamicpb.NewMessage(md.Output())
							b, err := json.Marshal(resm)
							if err != nil {
								return err
							}
							if err := (protojson.UnmarshalOptions{}).Unmarshal(b, mes); err != nil {
								return err
							}
							if err := stream.SendMsg(mes); err != nil {
								return err
							}
						}
					}
					continue L
				}
			}
			return status.Error(codes.NotFound, codes.NotFound.String())
		}
	}
}

func descriptorFromFiles(importPaths []string, protos ...string) (*descriptorpb.FileDescriptorSet, error) {
	protos, err := protoparse.ResolveFilenames(importPaths, protos...)
	if err != nil {
		return nil, err
	}
	importPaths, protos, accessor, err := resolvePaths(importPaths, protos...)
	if err != nil {
		return nil, err
	}
	p := protoparse.Parser{
		ImportPaths:           importPaths,
		InferImportPaths:      len(importPaths) == 0,
		IncludeSourceCodeInfo: true,
		Accessor:              accessor,
	}
	fds, err := p.ParseFiles(protos...)
	if err != nil {
		return nil, err
	}
	if err := registerFiles(fds); err != nil {
		return nil, err
	}

	return desc.ToFileDescriptorSet(fds...), nil
}

func resolvePaths(importPaths []string, protos ...string) ([]string, []string, func(filename string) (io.ReadCloser, error), error) {
	resolvedIPaths := importPaths
	resolvedProtos := []string{}
	for _, p := range protos {
		d, b := filepath.Split(p)
		resolvedIPaths = append(resolvedIPaths, d)
		resolvedProtos = append(resolvedProtos, b)
	}
	resolvedIPaths = unique(resolvedIPaths)
	resolvedProtos = unique(resolvedProtos)
	opened := []string{}
	return resolvedIPaths, resolvedProtos, func(filename string) (io.ReadCloser, error) {
		if contains(opened, filename) { // FIXME: Need to resolvePaths well without this condition
			return io.NopCloser(strings.NewReader("")), nil
		}
		opened = append(opened, filename)
		return os.Open(filename)
	}, nil
}

func serviceMatchFunc(service string) matchFunc {
	return func(r *Request) bool {
		return r.Service == strings.TrimPrefix(service, "/")
	}
}

func methodMatchFunc(method string) matchFunc {
	return func(r *Request) bool {
		if !strings.Contains(method, "/") {
			return r.Method == method
		}
		splitted := strings.Split(strings.TrimPrefix(method, "/"), "/")
		s := strings.Join(splitted[:len(splitted)-1], "/")
		m := splitted[len(splitted)-1]
		return r.Service == s && r.Method == m
	}
}

func registerFiles(fds []*desc.FileDescriptor) (err error) {
	var rf *protoregistry.Files
	rf, err = protodesc.NewFiles(desc.ToFileDescriptorSet(fds...))
	if err != nil {
		return err
	}

	rf.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if _, err := protoregistry.GlobalFiles.FindFileByPath(fd.Path()); !errors.Is(protoregistry.NotFound, err) {
			return true
		}

		// Skip registration of conflicted descriptors
		conflict := false
		rangeTopLevelDescriptors(fd, func(d protoreflect.Descriptor) {
			if _, err := protoregistry.GlobalFiles.FindDescriptorByName(d.FullName()); err == nil {
				conflict = true
			}
		})
		if conflict {
			return true
		}

		err = protoregistry.GlobalFiles.RegisterFile(fd)
		return (err == nil)
	})

	return err
}

func contains(s []string, e string) bool {
	for _, v := range s {
		if e == v {
			return true
		}
	}
	return false
}

// copy from google.golang.org/protobuf/reflect/protoregistry
func rangeTopLevelDescriptors(fd protoreflect.FileDescriptor, f func(protoreflect.Descriptor)) {
	eds := fd.Enums()
	for i := eds.Len() - 1; i >= 0; i-- {
		f(eds.Get(i))
		vds := eds.Get(i).Values()
		for i := vds.Len() - 1; i >= 0; i-- {
			f(vds.Get(i))
		}
	}
	mds := fd.Messages()
	for i := mds.Len() - 1; i >= 0; i-- {
		f(mds.Get(i))
	}
	xds := fd.Extensions()
	for i := xds.Len() - 1; i >= 0; i-- {
		f(xds.Get(i))
	}
	sds := fd.Services()
	for i := sds.Len() - 1; i >= 0; i-- {
		f(sds.Get(i))
	}
}

func splitMethodFullName(mn protoreflect.FullName) (string, string) {
	splitted := strings.Split(string(mn), ".")
	service := strings.Join(splitted[:len(splitted)-1], ".")
	method := splitted[len(splitted)-1]
	return service, method
}
