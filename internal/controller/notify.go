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

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	depsv1alpha1 "github.com/n0rm4l-me/klink/api/v1alpha1"
)

// notifyPayload is the JSON body sent to the webhook.
type notifyPayload struct {
	WorkloadDependency string `json:"workloadDependency"`
	Namespace          string `json:"namespace"`
	Phase              string `json:"phase"`
	PreviousPhase      string `json:"previousPhase,omitempty"`
	Message            string `json:"message"`
	Dependent          string `json:"dependent"`
	DependentKind      string `json:"dependentKind"`
	Timestamp          string `json:"timestamp"`
}

var httpClient = &http.Client{Timeout: 10 * time.Second}

// maybeNotify sends a webhook notification if configured and the new phase is in the OnPhases list.
func maybeNotify(ctx context.Context, c client.Client, wd *depsv1alpha1.WorkloadDependency, prevPhase, newPhase depsv1alpha1.DependencyPhase) {
	if wd.Spec.Notify == nil {
		return
	}
	if !shouldNotify(wd.Spec.Notify.OnPhases, newPhase) {
		return
	}

	url, err := resolveWebhookURL(ctx, c, wd)
	if err != nil || url == "" {
		return
	}

	payload := notifyPayload{
		WorkloadDependency: wd.Name,
		Namespace:          wd.Namespace,
		Phase:              string(newPhase),
		PreviousPhase:      string(prevPhase),
		Message:            wd.Status.Message,
		Dependent:          wd.Spec.Dependent.Name,
		DependentKind:      wd.Spec.Dependent.Kind,
		Timestamp:          time.Now().UTC().Format(time.RFC3339),
	}

	go sendNotification(ctx, url, payload)
}

func shouldNotify(onPhases []depsv1alpha1.DependencyPhase, phase depsv1alpha1.DependencyPhase) bool {
	if len(onPhases) == 0 {
		// Default: notify on Suspended and Healthy transitions
		return phase == depsv1alpha1.PhaseSuspended || phase == depsv1alpha1.PhaseHealthy
	}
	for _, p := range onPhases {
		if p == phase {
			return true
		}
	}
	return false
}

func resolveWebhookURL(ctx context.Context, c client.Client, wd *depsv1alpha1.WorkloadDependency) (string, error) {
	if wd.Spec.Notify.WebhookSecretRef != nil {
		ref := wd.Spec.Notify.WebhookSecretRef
		secret := &corev1.Secret{}
		if err := c.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: wd.Namespace}, secret); err != nil {
			return "", fmt.Errorf("get webhook secret %s: %w", ref.Name, err)
		}
		key := ref.Key
		if key == "" {
			key = "url"
		}
		url, ok := secret.Data[key]
		if !ok {
			return "", fmt.Errorf("webhook secret %s has no key %q", ref.Name, key)
		}
		return string(url), nil
	}
	return wd.Spec.Notify.Webhook, nil
}

// notifyMaxRetries is the number of delivery attempts before giving up.
const notifyMaxRetries = 3

func sendNotification(ctx context.Context, url string, payload notifyPayload) {
	log := logf.FromContext(ctx)

	body, err := json.Marshal(payload)
	if err != nil {
		log.Error(err, "failed to marshal notification payload")
		return
	}

	// Retry with exponential backoff: 1s, 2s, 4s
	var lastErr error
	for attempt := 0; attempt < notifyMaxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<(attempt-1)) * time.Second
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			log.Error(err, "failed to create notification request")
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "klink-operator/"+version())

		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		_ = resp.Body.Close()

		if resp.StatusCode >= 500 {
			// Server error — retry
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			continue
		}
		if resp.StatusCode >= 400 {
			// Client error — don't retry, the request is bad
			log.Error(fmt.Errorf("HTTP %d", resp.StatusCode), "notification webhook rejected request", "url", url)
			return
		}

		log.Info("notification sent", "url", url, "phase", payload.Phase, "status", resp.StatusCode, "attempt", attempt+1)
		return
	}

	log.Error(lastErr, "notification failed after retries", "url", url, "attempts", notifyMaxRetries)
}

func version() string {
	return "0.2.0"
}
