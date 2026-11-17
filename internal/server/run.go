package server

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"os"
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
	caMode := false
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
		caMode = true
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
	if _, err := os.Stat(filepath.Join(data, "ntfy.json")); err == nil {
		slog.Info("ntfy.json is not used any more, see the user manual 9.1")
	}
	if caMode {
		go s.certLoop(ctx, data)
	}
	stopResources := s.resources.Start(ctx)
	defer stopResources()
	go s.gcLoop(ctx)
	go s.pushLoop(ctx)
	slog.Info("listening", "addr", ln.Addr().String(), "fingerprint", s.Fingerprint)
	if err := srv.ServeTLS(ln, "", ""); err != http.ErrServerClosed {
		return err
	}
	return nil
}

// certLoop sets the certificate state of the bell and of Web Push (design 4.3), and logs it, when the CA-signed server certificate is about to expire (renewal is done by root).
func (s *Server) certLoop(ctx context.Context, data string) {
	for {
		err := CheckServerCert(data, time.Now())
		if err != nil {
			slog.Error("server certificate check failed", "err", err)
		}
		s.setServerState(&s.certFailed, err != nil)
		select {
		case <-ctx.Done():
			return
		case <-time.After(certCheckGap):
		}
	}
}
