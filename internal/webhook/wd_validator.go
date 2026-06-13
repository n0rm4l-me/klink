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
	"io"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	depsv1alpha1 "github.com/n0rm4l-me/klink/api/v1alpha1"
)

// WDValidatorHandler validates WorkloadDependency resources on CREATE/UPDATE.
type WDValidatorHandler struct {
	client client.Client
}

func NewWDValidatorHandler(c client.Client) *WDValidatorHandler {
	return &WDValidatorHandler{client: c}
}

func (h *WDValidatorHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := logf.FromContext(ctx)

	defer func() {
		if rec := recover(); rec != nil {
			log.Error(fmt.Errorf("panic: %v", rec), "recovered from panic in WD validator")
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
	}()

	body, err := io.ReadAll(r.Body)
	if err != nil || len(body) == 0 {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	review := &admissionv1.AdmissionReview{}
	if err := json.Unmarshal(body, review); err != nil || review.Request == nil {
		http.Error(w, "invalid admission review", http.StatusBadRequest)
		return
	}

	review.Response = h.validate(ctx, review.Request)
	review.Response.UID = review.Request.UID

	if err := json.NewEncoder(w).Encode(review); err != nil {
		log.Error(err, "failed to encode response")
	}
}

func (h *WDValidatorHandler) validate(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	wd := &depsv1alpha1.WorkloadDependency{}
	if err := json.Unmarshal(req.Object.Raw, wd); err != nil {
		return wdDeny(fmt.Sprintf("failed to decode WorkloadDependency: %v", err))
	}

	if errs := validateWD(wd); len(errs) > 0 {
		return wdDeny(fmt.Sprintf("validation failed: %s", errs[0]))
	}

	return wdAllow()
}

// validateWD runs all validation rules and returns a list of error messages.
func validateWD(wd *depsv1alpha1.WorkloadDependency) []string {
	var errs []string

	// spec.dependent.kind must be non-empty
	if wd.Spec.Dependent.Kind == "" {
		errs = append(errs, "spec.dependent.kind is required")
	}
	if wd.Spec.Dependent.Name == "" {
		errs = append(errs, "spec.dependent.name is required")
	}

	// spec.dependsOn must not be empty
	if len(wd.Spec.DependsOn) == 0 {
		errs = append(errs, "spec.dependsOn must have at least one entry")
	}

	// CronJob cannot be used as a dependency (no ready replicas to check)
	for i, dep := range wd.Spec.DependsOn {
		if dep.Kind == "CronJob" {
			errs = append(errs, fmt.Sprintf("spec.dependsOn[%d]: CronJob cannot be used as a dependency (no replicas to check); use it as a dependent instead", i))
		}
		if dep.Name == "" {
			errs = append(errs, fmt.Sprintf("spec.dependsOn[%d].name is required", i))
		}
	}

	// maxSuspendDuration must be positive if set
	if d := wd.Spec.OnDegraded.MaxSuspendDuration.Duration; d < 0 {
		errs = append(errs, "spec.onDegraded.maxSuspendDuration must be positive")
	}

	// window and recoveryWindow must be positive if set
	for i, dep := range wd.Spec.DependsOn {
		if dep.Condition.Window.Duration < 0 {
			errs = append(errs, fmt.Sprintf("spec.dependsOn[%d].condition.window must be positive", i))
		}
		if dep.Condition.RecoveryWindow.Duration < 0 {
			errs = append(errs, fmt.Sprintf("spec.dependsOn[%d].condition.recoveryWindow must be positive", i))
		}
	}

	// notify: must have either webhook or webhookSecretRef, not both
	if wd.Spec.Notify != nil {
		if wd.Spec.Notify.Webhook != "" && wd.Spec.Notify.WebhookSecretRef != nil {
			errs = append(errs, "spec.notify: set either webhook or webhookSecretRef, not both")
		}
		if wd.Spec.Notify.Webhook == "" && wd.Spec.Notify.WebhookSecretRef == nil {
			errs = append(errs, "spec.notify: must set either webhook or webhookSecretRef")
		}
	}

	return errs
}

func wdAllow() *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{
		Allowed: true,
		Result:  &metav1.Status{Code: http.StatusOK},
	}
}

func wdDeny(reason string) *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{
		Allowed: false,
		Result: &metav1.Status{
			Code:    http.StatusUnprocessableEntity,
			Message: reason,
			Reason:  metav1.StatusReasonInvalid,
		},
	}
}
