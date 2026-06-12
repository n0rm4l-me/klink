/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package webhook

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// WebhookRunnable is a controller-runtime Runnable that starts the HTTPS
// admission webhook server and the TLS rotation loop.
type WebhookRunnable struct {
	tlsMgr  *TLSManager
	handler http.Handler
	addr    string
}

func NewWebhookRunnable(tlsMgr *TLSManager, handler http.Handler, addr string) *WebhookRunnable {
	return &WebhookRunnable{tlsMgr: tlsMgr, handler: handler, addr: addr}
}

func (w *WebhookRunnable) Start(ctx context.Context) error {
	log := logf.FromContext(ctx)

	certPEM, keyPEM, err := w.tlsMgr.EnsureCert(ctx)
	if err != nil {
		return fmt.Errorf("ensure webhook cert: %w", err)
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("load webhook keypair: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/validate", w.handler)
	mux.HandleFunc("/healthz", func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{
		Addr:    w.addr,
		Handler: mux,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS13,
		},
	}

	go w.tlsMgr.StartRotationLoop(ctx)

	go func() {
		<-ctx.Done()
		// Use a timeout for graceful shutdown so pod termination doesn't hang
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Info("starting gate webhook server", "addr", w.addr)
	if err := srv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("webhook server: %w", err)
	}
	return nil
}
