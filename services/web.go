package services

import (
	"fmt"
	"net"
	"net/http"

	"github.com/pkg/errors"

	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

const (
	webHostFlag = "host"
	webPortFlag = "port"
)

type Web struct {
	host    string
	port    int
	ln      net.Listener
	handler *http.ServeMux
}

func NewWeb(c *cli.Context) *Web {
	return &Web{
		host:    c.String(webHostFlag),
		port:    c.Int(webPortFlag),
		handler: http.NewServeMux(),
	}
}

func RegisterWebFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   webHostFlag,
			Usage:  "listening host",
			Value:  "",
			EnvVar: "WEB_HOST",
		},
		cli.IntFlag{
			Name:   webPortFlag,
			Usage:  "http listening port",
			Value:  8080,
			EnvVar: "WEB_PORT",
		})
}

func (s *Web) Serve() error {
	addr := fmt.Sprintf("%s:%d", s.host, s.port)
	ln, err := net.Listen("tcp", addr)
	s.ln = ln
	if err != nil {
		return errors.Wrap(err, "Failed to web listen to tcp connection")
	}
	log.Infof("Serving Web at %v", addr)
	return http.Serve(s.ln, s.handler)
}

func (s *Web) RegisterProvider(n string, h func(http.ResponseWriter, *http.Request)) {
	s.handler.HandleFunc(n, h)
}

func (s *Web) Close() {
	log.Info("Closing Web")
	defer func() {
		log.Info("Web closed")
	}()
	if s.ln != nil {
		s.ln.Close()
	}
}
