package cermanager

import (
	"context"
	"io"
	"net"
	"os"
	"path"
	"sync"

	cerm "github.com/YLonely/cer-manager"
	"github.com/YLonely/cer-manager/api/types"
	"github.com/YLonely/cer-manager/http"
	"github.com/YLonely/cer-manager/log"
	"github.com/YLonely/cer-manager/services"
	"github.com/YLonely/cer-manager/services/checkpoint"
	"github.com/YLonely/cer-manager/services/namespace"
	"github.com/YLonely/cer-manager/utils"
	"github.com/pkg/errors"
)

const DefaultRootPath = "/var/lib/cermanager"
const DefaultSocketName = "daemon.socket"

type Server struct {
	services   map[cerm.ServiceType]services.Service
	httpServer *http.Server
	listener   net.Listener
	group      sync.WaitGroup
}

func NewServer(httpPort int) (*Server, error) {
	if err := os.MkdirAll(DefaultRootPath, 0755); err != nil {
		return nil, err
	}
	socketPath := path.Join(DefaultRootPath, DefaultSocketName)
	os.Remove(socketPath)
	addr, err := net.ResolveUnixAddr("unix", socketPath)
	if err != nil {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", addr)
	if err != nil {
		return nil, err
	}
	checkpointSvr, err := checkpoint.New(DefaultRootPath)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create checkpoint service")
	}
	namespaceSvr, err := namespace.New(DefaultRootPath, checkpointSvr.(types.Supplier))
	if err != nil {
		return nil, errors.Wrap(err, "failed to create namespace service")
	}
	var httpServer *http.Server
	if httpPort != 0 {
		httpServer, err = http.NewServer(DefaultRootPath, httpPort)
		if err != nil {
			return nil, errors.Wrap(err, "failed to create http server")
		}
	}
	svr := &Server{
		services: map[cerm.ServiceType]services.Service{
			cerm.NamespaceService:  namespaceSvr,
			cerm.CheckpointService: checkpointSvr,
		},
		listener:   listener,
		httpServer: httpServer,
	}
	for _, service := range svr.services {
		if err = service.Init(); err != nil {
			return nil, err
		}
	}
	return svr, nil
}

func (s *Server) Start(ctx context.Context) chan error {
	errorC := make(chan error, 1)
	if s.httpServer != nil {
		go func() {
			ec := s.httpServer.Start()
			err := <-ec
			errorC <- err
		}()
	}
	go func() {
		for {
			conn, err := s.listener.Accept()
			if err != nil {
				errorC <- err
				return
			}
			s.group.Add(1)
			go s.serve(ctx, conn, errorC)
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
	}()
	return errorC
}

func (s *Server) serve(ctx context.Context, conn net.Conn, errorC chan error) {
	defer s.group.Done()
	for {
		svrType, err := utils.ReceiveServiceType(conn)
		if err != nil {
			if err != io.EOF {
				log.Raw().WithError(err).Error("invalid request")
			}
			conn.Close()
			return
		}
		if svr, exists := s.services[svrType]; !exists {
			conn.Close()
			log.Raw().Errorf("invalid service type %v", svrType)
		} else {
			svr.Handle(ctx, conn)
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

func (s *Server) Shutdown() {
	s.group.Wait()
	for t, ss := range s.services {
		if err := ss.Stop(); err != nil {
			svrName := cerm.Type2Services[t]
			log.Raw().Errorf("%s service shutdown with error %v", svrName, err)
		}
	}
	if s.httpServer != nil {
		if err := s.httpServer.Shutdown(); err != nil {
			log.Raw().WithError(err).Error("http server shutdown with error")
		}
	}
}
