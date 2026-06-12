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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	depsv1alpha1 "github.com/n0rm4l-me/klink/api/v1alpha1"
)

func newTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = depsv1alpha1.AddToScheme(s)
	return s
}

func makeDeploymentRaw(name, ns string, replicas int32) []byte {
	r := replicas
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: &r,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
		},
	}
	b, _ := json.Marshal(dep)
	return b
}

func makeAdmissionRequest(kind, name, ns string, oldReplicas, newReplicas int32) []byte {
	review := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Request: &admissionv1.AdmissionRequest{
			UID:         "test-uid",
			Kind:        metav1.GroupVersionKind{Group: "apps", Version: "v1", Kind: kind},
			Resource:    metav1.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			Name:        name,
			Namespace:   ns,
			Operation:   admissionv1.Update,
			Object:      runtime.RawExtension{Raw: makeDeploymentRaw(name, ns, newReplicas)},
			OldObject:   runtime.RawExtension{Raw: makeDeploymentRaw(name, ns, oldReplicas)},
		},
	}
	b, _ := json.Marshal(review)
	return b
}

func callWebhook(t *testing.T, handler *GateHandler, body []byte) *admissionv1.AdmissionReview {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/validate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var review admissionv1.AdmissionReview
	if err := json.Unmarshal(w.Body.Bytes(), &review); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	return &review
}

func makeWD(name, ns, depKind, depName, depNS, dependentKind, dependentName string, mode depsv1alpha1.EnforcementMode) *depsv1alpha1.WorkloadDependency {
	return &depsv1alpha1.WorkloadDependency{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: depsv1alpha1.WorkloadDependencySpec{
			Dependent: depsv1alpha1.WorkloadRef{Kind: dependentKind, Name: dependentName},
			DependsOn: []depsv1alpha1.DependsOnEntry{{
				Kind:      depKind,
				Name:      depName,
				Namespace: depNS,
				Condition: depsv1alpha1.HealthCondition{MinReadyPercent: 100},
			}},
			OnDegraded: depsv1alpha1.OnDegradedSpec{Action: depsv1alpha1.ActionScaleToZero},
			Mode:       mode,
		},
	}
}

func makeHealthyDeployment(name, ns string, replicas int32) *appsv1.Deployment {
	r := replicas
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       appsv1.DeploymentSpec{Replicas: &r},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: replicas, Replicas: replicas},
	}
}

func makeUnhealthyDeployment(name, ns string) *appsv1.Deployment {
	zero := int32(0)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       appsv1.DeploymentSpec{Replicas: &zero},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 0, Replicas: 0},
	}
}

// Test: no WD matches → allow
func TestGateWebhook_NoMatchingWD_Allows(t *testing.T) {
	s := newTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).Build()
	h := NewGateHandler(c)

	body := makeAdmissionRequest("Deployment", "payments", "default", 0, 3)
	review := callWebhook(t, h, body)

	if !review.Response.Allowed {
		t.Errorf("expected allow, got deny: %s", review.Response.Result.Message)
	}
}

// Test: WD mode=strict → webhook is not the gate, allow anyway
func TestGateWebhook_StrictMode_Allows(t *testing.T) {
	s := newTestScheme()
	wd := makeWD("wd", "default", "Deployment", "database", "", "Deployment", "payments", depsv1alpha1.ModeStrict)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(wd).Build()
	h := NewGateHandler(c)

	body := makeAdmissionRequest("Deployment", "payments", "default", 0, 3)
	review := callWebhook(t, h, body)

	if !review.Response.Allowed {
		t.Errorf("expected allow for strict mode (webhook handles gate only), got deny")
	}
}

// Test: gate mode + healthy dependency → allow
func TestGateWebhook_GateMode_HealthyDep_Allows(t *testing.T) {
	s := newTestScheme()
	wd := makeWD("wd", "default", "Deployment", "database", "default", "Deployment", "payments", depsv1alpha1.ModeGate)
	db := makeHealthyDeployment("database", "default", 2)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(wd, db).WithStatusSubresource(db).Build()
	h := NewGateHandler(c)

	body := makeAdmissionRequest("Deployment", "payments", "default", 0, 3)
	review := callWebhook(t, h, body)

	if !review.Response.Allowed {
		t.Errorf("expected allow (dependency healthy), got deny: %s", review.Response.Result.Message)
	}
}

// Test: gate mode + unhealthy dependency → deny
func TestGateWebhook_GateMode_UnhealthyDep_Denies(t *testing.T) {
	s := newTestScheme()
	wd := makeWD("wd", "default", "Deployment", "database", "default", "Deployment", "payments", depsv1alpha1.ModeGate)
	db := makeUnhealthyDeployment("database", "default")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(wd, db).Build()
	h := NewGateHandler(c)

	body := makeAdmissionRequest("Deployment", "payments", "default", 0, 3)
	review := callWebhook(t, h, body)

	if review.Response.Allowed {
		t.Error("expected deny (dependency unhealthy), got allow")
	}
	if review.Response.Result == nil || review.Response.Result.Message == "" {
		t.Error("expected denial message")
	}
}

// Test: scale-down always allowed even when dep is unhealthy
func TestGateWebhook_ScaleDown_AlwaysAllows(t *testing.T) {
	s := newTestScheme()
	wd := makeWD("wd", "default", "Deployment", "database", "default", "Deployment", "payments", depsv1alpha1.ModeGate)
	db := makeUnhealthyDeployment("database", "default")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(wd, db).Build()
	h := NewGateHandler(c)

	// Scaling DOWN from 5 to 2 — should always be allowed
	body := makeAdmissionRequest("Deployment", "payments", "default", 5, 2)
	review := callWebhook(t, h, body)

	if !review.Response.Allowed {
		t.Errorf("expected allow for scale-down, got deny: %s", review.Response.Result.Message)
	}
}

// Test: no replica change → allow
func TestGateWebhook_NoReplicaChange_Allows(t *testing.T) {
	s := newTestScheme()
	wd := makeWD("wd", "default", "Deployment", "database", "default", "Deployment", "payments", depsv1alpha1.ModeGate)
	db := makeUnhealthyDeployment("database", "default")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(wd, db).Build()
	h := NewGateHandler(c)

	body := makeAdmissionRequest("Deployment", "payments", "default", 3, 3)
	review := callWebhook(t, h, body)

	if !review.Response.Allowed {
		t.Errorf("expected allow (no change), got deny")
	}
}

// Test: WD in different namespace → not matched → allow
func TestGateWebhook_DifferentNamespace_Allows(t *testing.T) {
	s := newTestScheme()
	// WD is in "other-ns", request is in "default"
	wd := makeWD("wd", "other-ns", "Deployment", "database", "", "Deployment", "payments", depsv1alpha1.ModeGate)
	db := makeUnhealthyDeployment("database", "default")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(wd, db).Build()
	h := NewGateHandler(c)

	body := makeAdmissionRequest("Deployment", "payments", "default", 0, 3)
	review := callWebhook(t, h, body)

	if !review.Response.Allowed {
		t.Errorf("expected allow (WD in different namespace), got deny")
	}
}

// Test: fail open when client.List errors
func TestGateWebhook_ClientError_FailsOpen(t *testing.T) {
	// Use a fake client without the WD scheme registered to simulate list error
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	// Intentionally NOT registering depsv1alpha1 — List will fail
	c := fake.NewClientBuilder().WithScheme(s).Build()
	h := NewGateHandler(c)

	body := makeAdmissionRequest("Deployment", "payments", "default", 0, 3)
	review := callWebhook(t, h, body)

	// Should fail open (allow) rather than block
	if !review.Response.Allowed {
		t.Errorf("expected fail-open (allow) on client error, got deny")
	}
}

// Test: malformed JSON body returns 400
func TestGateWebhook_MalformedBody_Returns400(t *testing.T) {
	s := newTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).Build()
	h := NewGateHandler(c)

	req := httptest.NewRequest(http.MethodPost, "/validate", bytes.NewReader([]byte("not json")))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// Test: empty body returns non-200 (400 or 500)
func TestGateWebhook_EmptyBody_ReturnsError(t *testing.T) {
	s := newTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).Build()
	h := NewGateHandler(c)

	req := httptest.NewRequest(http.MethodPost, "/validate", bytes.NewReader([]byte("")))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Errorf("expected non-200 for empty body, got 200")
	}
}
