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
	"sync"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// certHolder holds the current TLS certificate and allows atomic replacement.
// This is the fix for C-4: Go's tls.Config.Certificates[] is read once at
// server start; using GetCertificate with a mutex-protected holder lets the
// rotation loop update the cert in-place without restarting the server.
type certHolder struct {
	mu   sync.RWMutex
	cert *tls.Certificate
}

func (h *certHolder) get() *tls.Certificate {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cert
}

func (h *certHolder) set(cert tls.Certificate) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cert = &cert
}

// WebhookRunnable is a controller-runtime Runnable that starts the HTTPS
// admission webhook server and the TLS rotation loop.
type WebhookRunnable struct {
	tlsMgr      *TLSManager
	gateHandler http.Handler
	wdValidator http.Handler
	addr        string
}

func NewWebhookRunnable(tlsMgr *TLSManager, gateHandler http.Handler, wdValidator http.Handler, addr string) *WebhookRunnable {
	return &WebhookRunnable{tlsMgr: tlsMgr, gateHandler: gateHandler, wdValidator: wdValidator, addr: addr}
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

	holder := &certHolder{}
	holder.set(cert)

	mux := http.NewServeMux()
	mux.Handle("/validate", w.gateHandler)
	mux.Handle("/validate-wd", w.wdValidator)
	mux.HandleFunc("/healthz", func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{
		Addr:    w.addr,
		Handler: mux,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
			// GetCertificate is called per-handshake and reads from the holder,
			// allowing the rotation loop to update the cert without a server restart.
			GetCertificate: func(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
				return holder.get(), nil
			},
		},
	}

	// Start the rotation loop. It calls EnsureCert which updates the Secret
	// and patches caBundle. We also hook it to update our in-memory holder.
	go w.runRotationLoop(ctx, holder)

	go func() {
		<-ctx.Done()
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

// runRotationLoop extends the TLSManager rotation loop to also update the
// in-memory cert holder so the running TLS server picks up the new cert.
func (w *WebhookRunnable) runRotationLoop(ctx context.Context, holder *certHolder) {
	log := logf.FromContext(ctx)
	ticker := time.NewTicker(12 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			certPEM, keyPEM, err := w.tlsMgr.EnsureCert(ctx)
			if err != nil {
				log.Error(err, "cert rotation failed")
				continue
			}
			cert, err := tls.X509KeyPair(certPEM, keyPEM)
			if err != nil {
				log.Error(err, "failed to load rotated keypair")
				continue
			}
			holder.set(cert)
			log.Info("webhook TLS cert rotated and loaded into server")
		}
	}
}
