package server

import (
	"context"
	"crypto/tls"
	"kaze-sync/internal/ntfy"
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
	tlsConf := &tls.Config{MinVersion: tls.VersionTLS12}
	if certFile, keyFile := MetaString(s.db, "cert"), MetaString(s.db, "key"); certFile != "" {
		var cert tls.Certificate
		if cert, err = tls.LoadX509KeyPair(certFile, keyFile); err != nil {
			return err
		}
		tlsConf.Certificates = []tls.Certificate{cert}
	} else if MetaString(s.db, "tls") == "ca" {
		cs, err := NewCertSource(serverPEMPath(data))
		if err != nil {
			return err
		}
		tlsConf.GetCertificate = cs.GetCertificate
		s.Fingerprint = CAFingerprint(cs.Current())
		go s.certLoop(ctx, data)
	} else {
		cert, err := EnsureSelfSigned(filepath.Join(data, VarDir, TLSDir))
		if err != nil {
			return err
		}
		tlsConf.Certificates = []tls.Certificate{cert}
		s.Fingerprint = Fingerprint(cert)
	}
	ln, err := net.Listen("tcp", MetaString(s.db, "listen"))
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		TLSConfig:         tlsConf,
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
	if s.Push, err = ntfy.Load(data, "server"); err != nil {
		slog.Error("ntfy is off", "err", err)
	}
	go s.gcLoop(ctx)
	go s.pushLoop(ctx)
	slog.Info("listening", "addr", ln.Addr().String(), "fingerprint", s.Fingerprint)
	if err := srv.ServeTLS(ln, "", ""); err != http.ErrServerClosed {
		return err
	}
	return nil
}

// certLoop renews the CA-signed server certificate when this process may sign, and warns when it may not
// and the certificate is about to expire.
func (s *Server) certLoop(ctx context.Context, data string) {
	for {
		if _, err := CheckServerCert(s.db, data, time.Now()); err != nil {
			slog.Error("server certificate check failed", "err", err)
			s.Push.Fail("cert", "The server certificate cannot be renewed or is about to expire", 1)
		} else {
			s.Push.Ok("cert", "The server certificate is in order again")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(certCheckGap):
		}
	}
}
