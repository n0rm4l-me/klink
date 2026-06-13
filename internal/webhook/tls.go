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
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	certRenewBefore   = 30 * 24 * time.Hour // renew 30 days before expiry
	certValidity      = 365 * 24 * time.Hour
	tlsSecretName     = "klink-webhook-tls"
	webhookConfigName = "klink-gate"
	wdValidatorName   = "klink-wd-validator"
)

// TLSManager generates and rotates the self-signed TLS certificate used by the
// admission webhook. It patches the ValidatingWebhookConfiguration caBundle so
// the API server trusts the cert.
type TLSManager struct {
	client    client.Client
	reader    client.Reader // direct API reader, bypasses cache — safe to use before cache sync
	namespace string
	svcName   string // webhook Service name, e.g. "klink-webhook"
}

func NewTLSManager(c client.Client, reader client.Reader, namespace, svcName string) *TLSManager {
	return &TLSManager{client: c, reader: reader, namespace: namespace, svcName: svcName}
}

// EnsureCert checks the current cert and rotates if missing or expiring soon.
// Returns (certPEM, keyPEM, error).
func (m *TLSManager) EnsureCert(ctx context.Context) ([]byte, []byte, error) {
	log := logf.FromContext(ctx)

	secret := &corev1.Secret{}
	err := m.reader.Get(ctx, types.NamespacedName{Name: tlsSecretName, Namespace: m.namespace}, secret)

	if err == nil {
		// Secret exists — check expiry
		certPEM := secret.Data["tls.crt"]
		keyPEM := secret.Data["tls.key"]
		if m.certNeedsRenewal(certPEM) {
			log.Info("webhook cert expiring soon, rotating")
		} else {
			// Cert is valid — patch caBundle on every startup to handle restarts
			if patchErr := m.patchWebhookCABundle(ctx, certPEM); patchErr != nil {
				log.Error(patchErr, "failed to patch webhook caBundle on startup")
			} else {
				log.Info("webhook caBundle patched on startup")
			}
			return certPEM, keyPEM, nil
		}
	} else if !errors.IsNotFound(err) {
		return nil, nil, fmt.Errorf("get tls secret: %w", err)
	}

	// Generate new cert
	log.Info("generating webhook TLS certificate")
	certPEM, keyPEM, err := m.generateCert()
	if err != nil {
		return nil, nil, fmt.Errorf("generate cert: %w", err)
	}

	// Save to Secret
	newSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tlsSecretName,
			Namespace: m.namespace,
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": certPEM,
			"tls.key": keyPEM,
		},
	}

	if errors.IsNotFound(err) || secret.Name == "" {
		if createErr := m.client.Create(ctx, newSecret); createErr != nil {
			return nil, nil, fmt.Errorf("create tls secret: %w", createErr)
		}
	} else {
		secret.Data = newSecret.Data
		if updateErr := m.client.Update(ctx, secret); updateErr != nil {
			return nil, nil, fmt.Errorf("update tls secret: %w", updateErr)
		}
	}

	// Patch webhook caBundle
	if patchErr := m.patchWebhookCABundle(ctx, certPEM); patchErr != nil {
		log.Error(patchErr, "failed to patch webhook caBundle — webhook may not work until next rotation")
	}

	return certPEM, keyPEM, nil
}

// PatchCABundle patches the ValidatingWebhookConfiguration with the current CA cert.
func (m *TLSManager) PatchCABundle(ctx context.Context) error {
	secret := &corev1.Secret{}
	if err := m.reader.Get(ctx, types.NamespacedName{Name: tlsSecretName, Namespace: m.namespace}, secret); err != nil {
		return err
	}
	return m.patchWebhookCABundle(ctx, secret.Data["tls.crt"])
}

func (m *TLSManager) patchWebhookCABundle(ctx context.Context, caPEM []byte) error {
	for _, name := range []string{webhookConfigName, wdValidatorName} {
		whc := &admissionv1.ValidatingWebhookConfiguration{}
		if err := m.reader.Get(ctx, types.NamespacedName{Name: name}, whc); err != nil {
			if errors.IsNotFound(err) {
				continue // not installed yet, skip
			}
			return err
		}
		patch := client.MergeFrom(whc.DeepCopy())
		for i := range whc.Webhooks {
			whc.Webhooks[i].ClientConfig.CABundle = caPEM
		}
		if err := m.client.Patch(ctx, whc, patch); err != nil {
			return err
		}
	}
	return nil
}

func (m *TLSManager) certNeedsRenewal(certPEM []byte) bool {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return true
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return true
	}
	return time.Until(cert.NotAfter) < certRenewBefore
}

func (m *TLSManager) generateCert() ([]byte, []byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}

	now := time.Now()
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}

	// DNS SANs for the webhook service
	dnsNames := []string{
		m.svcName,
		fmt.Sprintf("%s.%s", m.svcName, m.namespace),
		fmt.Sprintf("%s.%s.svc", m.svcName, m.namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", m.svcName, m.namespace),
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"klink"},
			CommonName:   dnsNames[2],
		},
		DNSNames:              dnsNames,
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(certValidity),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}

	var certBuf, keyBuf bytes.Buffer
	if err := pem.Encode(&certBuf, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		return nil, nil, err
	}
	if err := pem.Encode(&keyBuf, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}); err != nil {
		return nil, nil, err
	}

	return certBuf.Bytes(), keyBuf.Bytes(), nil
}

// StartRotationLoop runs a background loop that checks and rotates the cert every 12h.
func (m *TLSManager) StartRotationLoop(ctx context.Context) {
	log := logf.FromContext(ctx)
	ticker := time.NewTicker(12 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, _, err := m.EnsureCert(ctx); err != nil {
				log.Error(err, "cert rotation failed")
			} else {
				if err := m.PatchCABundle(ctx); err != nil {
					log.Error(err, "failed to patch caBundle after rotation")
				}
			}
		}
	}
}
