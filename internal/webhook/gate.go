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
	"encoding/json"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	depsv1alpha1 "github.com/n0rm4l-me/klink/api/v1alpha1"
)

var (
	scheme = runtime.NewScheme()
	codecs = serializer.NewCodecFactory(scheme)
)

func init() {
	_ = admissionv1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
}

// GateHandler is the HTTP handler for the ValidatingAdmissionWebhook.
// It blocks scale-up of workloads whose dependencies are unhealthy (mode=gate).
type GateHandler struct {
	client client.Client
}

func NewGateHandler(c client.Client) *GateHandler {
	return &GateHandler{client: c}
}

func (h *GateHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := logf.FromContext(ctx)

	body := make([]byte, r.ContentLength)
	if _, err := r.Body.Read(body); err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	review := &admissionv1.AdmissionReview{}
	if _, _, err := codecs.UniversalDeserializer().Decode(body, nil, review); err != nil {
		http.Error(w, "failed to decode admission review", http.StatusBadRequest)
		return
	}

	review.Response = h.validate(ctx, review.Request)
	review.Response.UID = review.Request.UID

	if err := json.NewEncoder(w).Encode(review); err != nil {
		log.Error(err, "failed to encode response")
	}
}

func (h *GateHandler) validate(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	log := logf.FromContext(ctx)

	// Only care about scale-up (replicas increasing or CronJob unsuspend)
	newReplicas, err := extractReplicas(req.Object.Raw, req.Kind.Kind)
	if err != nil {
		log.Error(err, "failed to extract replicas from request")
		return allow()
	}

	oldReplicas, _ := extractReplicas(req.OldObject.Raw, req.Kind.Kind)

	// Only block if someone is trying to scale UP
	if newReplicas <= oldReplicas {
		return allow()
	}

	// Find WorkloadDependency with mode=gate for this workload
	wdList := &depsv1alpha1.WorkloadDependencyList{}
	if err := h.client.List(ctx, wdList, client.InNamespace(req.Namespace)); err != nil {
		log.Error(err, "failed to list WorkloadDependencies")
		return allow() // fail open — don't block if we can't check
	}

	for _, wd := range wdList.Items {
		if wd.Spec.Mode != depsv1alpha1.ModeGate {
			continue
		}
		depNS := wd.Spec.Dependent.Namespace
		if depNS == "" {
			depNS = wd.Namespace
		}
		if wd.Spec.Dependent.Kind != req.Kind.Kind || wd.Spec.Dependent.Name != req.Name || depNS != req.Namespace {
			continue
		}

		// This WD applies — check all dependencies
		for _, dep := range wd.Spec.DependsOn {
			ns := dep.Namespace
			if ns == "" {
				ns = wd.Namespace
			}

			healthy, msg, checkErr := h.isDependencyHealthy(ctx, dep.Kind, dep.Name, ns, dep.Condition)
			if checkErr != nil {
				log.Error(checkErr, "failed to check dependency health", "dep", dep.Name)
				continue // fail open for individual dep
			}
			if !healthy {
				reason := fmt.Sprintf(
					"klink gate: scale blocked by WorkloadDependency/%s — dependency %s/%s is not healthy: %s",
					wd.Name, dep.Kind, dep.Name, msg,
				)
				log.Info("gate: blocking scale-up", "workload", req.Name, "reason", reason)
				return deny(reason)
			}
		}
	}

	return allow()
}

func (h *GateHandler) isDependencyHealthy(ctx context.Context, kind, name, ns string, cond depsv1alpha1.HealthCondition) (bool, string, error) {
	switch kind {
	case "Rollout":
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(rolloutGVK)
		if err := h.client.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, obj); err != nil {
			return false, "not found", err
		}
		phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
		switch phase {
		case "Healthy", "Progressing", "Paused":
			return true, "", nil
		default:
			return false, fmt.Sprintf("phase=%s", phase), nil
		}

	case "StatefulSet":
		sts := &appsv1.StatefulSet{}
		if err := h.client.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, sts); err != nil {
			return false, "not found", err
		}
		return checkReplicaHealth(sts.Spec.Replicas, sts.Status.ReadyReplicas, cond, name)

	default: // Deployment
		dep := &appsv1.Deployment{}
		if err := h.client.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, dep); err != nil {
			return false, "not found", err
		}
		return checkReplicaHealth(dep.Spec.Replicas, dep.Status.ReadyReplicas, cond, name)
	}
}

func checkReplicaHealth(desired *int32, ready int32, cond depsv1alpha1.HealthCondition, name string) (bool, string, error) {
	if desired == nil || *desired == 0 {
		return false, fmt.Sprintf("%s has 0 desired replicas", name), nil
	}
	minPercent := cond.MinReadyPercent
	if minPercent == 0 {
		minPercent = 100
	}
	readyPercent := int32(float64(ready) / float64(*desired) * 100)
	if readyPercent < minPercent {
		return false, fmt.Sprintf("%d/%d ready (%d%% < %d%%)", ready, *desired, readyPercent, minPercent), nil
	}
	return true, "", nil
}

func extractReplicas(raw []byte, kind string) (int32, error) {
	if raw == nil {
		return 0, nil
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return 0, err
	}

	// CronJob: suspended=false means "running" = replicas=1, suspended=true = replicas=0
	if kind == "CronJob" {
		spec, _ := obj["spec"].(map[string]interface{})
		if suspended, ok := spec["suspend"].(bool); ok && suspended {
			return 0, nil
		}
		return 1, nil
	}

	// Deployment / StatefulSet / Rollout: spec.replicas
	spec, _ := obj["spec"].(map[string]interface{})
	if r, ok := spec["replicas"].(float64); ok {
		return int32(r), nil
	}
	return 1, nil // default replicas=1 if not set
}

func allow() *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{
		Allowed: true,
		Result:  &metav1.Status{Code: http.StatusOK},
	}
}

func deny(reason string) *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{
		Allowed: false,
		Result: &metav1.Status{
			Code:    http.StatusForbidden,
			Message: reason,
			Reason:  metav1.StatusReasonForbidden,
		},
	}
}
