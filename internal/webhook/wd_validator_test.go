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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	depsv1alpha1 "github.com/n0rm4l-me/klink/api/v1alpha1"
)

func validWD() *depsv1alpha1.WorkloadDependency {
	return &depsv1alpha1.WorkloadDependency{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: depsv1alpha1.WorkloadDependencySpec{
			Dependent: depsv1alpha1.WorkloadRef{Kind: "Deployment", Name: "payments"},
			DependsOn: []depsv1alpha1.DependsOnEntry{{
				Kind: "Deployment",
				Name: "database",
			}},
			OnDegraded: depsv1alpha1.OnDegradedSpec{Action: depsv1alpha1.ActionScaleToZero},
			Mode:       depsv1alpha1.ModeStrict,
		},
	}
}

func TestValidateWD_Valid(t *testing.T) {
	errs := validateWD(validWD())
	if len(errs) != 0 {
		t.Errorf("expected no errors, got: %v", errs)
	}
}

func TestValidateWD_MissingDependentKind(t *testing.T) {
	wd := validWD()
	wd.Spec.Dependent.Kind = ""
	errs := validateWD(wd)
	if len(errs) == 0 {
		t.Error("expected error for missing dependent kind")
	}
}

func TestValidateWD_MissingDependentName(t *testing.T) {
	wd := validWD()
	wd.Spec.Dependent.Name = ""
	errs := validateWD(wd)
	if len(errs) == 0 {
		t.Error("expected error for missing dependent name")
	}
}

func TestValidateWD_EmptyDependsOn(t *testing.T) {
	wd := validWD()
	wd.Spec.DependsOn = nil
	errs := validateWD(wd)
	if len(errs) == 0 {
		t.Error("expected error for empty dependsOn")
	}
}

func TestValidateWD_CronJobAsDependency(t *testing.T) {
	wd := validWD()
	wd.Spec.DependsOn = []depsv1alpha1.DependsOnEntry{{
		Kind: "CronJob",
		Name: "my-job",
	}}
	errs := validateWD(wd)
	if len(errs) == 0 {
		t.Error("expected error: CronJob cannot be used as dependency")
	}
}

func TestValidateWD_NegativeMaxSuspendDuration(t *testing.T) {
	wd := validWD()
	wd.Spec.OnDegraded.MaxSuspendDuration = metav1.Duration{Duration: -1 * time.Second}
	errs := validateWD(wd)
	if len(errs) == 0 {
		t.Error("expected error for negative maxSuspendDuration")
	}
}

func TestValidateWD_MaxSuspendDurationValid(t *testing.T) {
	wd := validWD()
	wd.Spec.OnDegraded.MaxSuspendDuration = metav1.Duration{Duration: 2 * time.Hour}
	errs := validateWD(wd)
	if len(errs) != 0 {
		t.Errorf("expected no errors for valid maxSuspendDuration, got: %v", errs)
	}
}

func TestValidateWD_NotifyBothWebhooks(t *testing.T) {
	wd := validWD()
	wd.Spec.Notify = &depsv1alpha1.NotifySpec{
		Webhook:          "https://hooks.slack.com/xxx",
		WebhookSecretRef: &depsv1alpha1.SecretKeyRef{Name: "my-secret"},
	}
	errs := validateWD(wd)
	if len(errs) == 0 {
		t.Error("expected error: cannot set both webhook and webhookSecretRef")
	}
}

func TestValidateWD_NotifyMissingBoth(t *testing.T) {
	wd := validWD()
	wd.Spec.Notify = &depsv1alpha1.NotifySpec{}
	errs := validateWD(wd)
	if len(errs) == 0 {
		t.Error("expected error: must set either webhook or webhookSecretRef")
	}
}

func TestValidateWD_NotifyValidWebhook(t *testing.T) {
	wd := validWD()
	wd.Spec.Notify = &depsv1alpha1.NotifySpec{
		Webhook: "https://hooks.slack.com/xxx",
	}
	errs := validateWD(wd)
	if len(errs) != 0 {
		t.Errorf("expected no errors for valid notify config, got: %v", errs)
	}
}
