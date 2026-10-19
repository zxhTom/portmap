package webui

import (
	"crypto/tls"
	"net"
)

func (s *Server) listenTLS(addr, cert, key string) error {
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	config := &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	go s.http.Serve(tls.NewListener(ln, config))
	return nil
}
