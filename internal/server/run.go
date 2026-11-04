package server

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"time"
)

// Run serves the data directory until ctx is done. It fails when meta.db is missing.
func Run(ctx context.Context, data string) error {
	s, err := New(data, nil)
	if err != nil {
		return err
	}
	defer s.Close()
	var cert tls.Certificate
	if certFile, keyFile := MetaString(s.db, "cert"), MetaString(s.db, "key"); certFile != "" {
		cert, err = tls.LoadX509KeyPair(certFile, keyFile)
	} else if cert, err = EnsureSelfSigned(filepath.Join(data, VarDir, TLSDir)); err == nil {
		s.Fingerprint = Fingerprint(cert)
	}
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", MetaString(s.db, "listen"))
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		<-ctx.Done()
		s.StopEvents() // Shutdown would wait for the streams
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
		srv.Close()
	}()
	go s.gcLoop(ctx)
	slog.Info("listening", "addr", ln.Addr().String(), "fingerprint", s.Fingerprint)
	if err := srv.ServeTLS(ln, "", ""); err != http.ErrServerClosed {
		return err
	}
	return nil
}
