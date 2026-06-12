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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WorkloadRef identifies a workload in the cluster.
type WorkloadRef struct {
	// +kubebuilder:validation:Enum=Deployment;StatefulSet;CronJob;Rollout
	Kind string `json:"kind"`
	Name string `json:"name"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// HealthCondition defines thresholds and timing for dependency health checks.
type HealthCondition struct {
	// Minimum percentage of ready replicas to consider the workload healthy.
	// +kubebuilder:default=100
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	MinReadyPercent int32 `json:"minReadyPercent,omitempty"`

	// How long the dependency must be unhealthy before action is taken.
	// +kubebuilder:default="30s"
	Window metav1.Duration `json:"window,omitempty"`

	// How long the dependency must be healthy before restoring replicas.
	// +kubebuilder:default="60s"
	RecoveryWindow metav1.Duration `json:"recoveryWindow,omitempty"`
}

// DependsOnEntry describes a single dependency to watch.
type DependsOnEntry struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// +optional
	Condition HealthCondition `json:"condition,omitempty"`
}

// +kubebuilder:validation:Enum=ScaleToZero
type DegradedAction string

const (
	ActionScaleToZero DegradedAction = "ScaleToZero"
)

// OnDegradedSpec defines what action to take when a dependency is unhealthy.
type OnDegradedSpec struct {
	// +kubebuilder:default=ScaleToZero
	Action DegradedAction `json:"action,omitempty"`

	// MaxSuspendDuration is the maximum time the dependent workload can remain suspended.
	// After this duration klink will restore the workload even if the dependency is still unhealthy.
	// Zero means suspend indefinitely. Example: "4h", "30m".
	// +optional
	MaxSuspendDuration metav1.Duration `json:"maxSuspendDuration,omitempty"`
}

// +kubebuilder:validation:Enum=strict;soft;gate;observe
type EnforcementMode string

const (
	// ModeStrict re-enforces scale-to-zero on every reconcile while dependency is unhealthy.
	// Manual scale-up of dependent is reverted within 15s. Use klink.dev/paused=true to override.
	ModeStrict EnforcementMode = "strict"
	// ModeSoft scales to zero once but does not fight manual changes.
	ModeSoft EnforcementMode = "soft"
	// ModeGate blocks dependent from starting until dependency is healthy via admission webhook.
	ModeGate EnforcementMode = "gate"
	// ModeObserve logs what klink would do but takes no action. Use for dry-run / onboarding.
	ModeObserve EnforcementMode = "observe"
)

const (
	// AnnotationPaused suspends all klink enforcement on this WorkloadDependency.
	AnnotationPaused = "klink.dev/paused"

	// FinalizerName ensures replicas are restored before a WorkloadDependency is deleted.
	FinalizerName = "klink.dev/finalizer"
)

// NotifySpec configures webhook notifications on phase transitions.
type NotifySpec struct {
	// Webhook is the URL to POST phase change notifications to.
	// +optional
	Webhook string `json:"webhook,omitempty"`

	// WebhookSecretRef references a Secret containing the webhook URL under key "url".
	// Takes precedence over Webhook if set.
	// +optional
	WebhookSecretRef *SecretKeyRef `json:"webhookSecretRef,omitempty"`

	// OnPhases lists which phase transitions trigger a notification.
	// Defaults to ["Suspended", "Healthy"] if empty.
	// +optional
	OnPhases []DependencyPhase `json:"onPhases,omitempty"`
}

// SecretKeyRef references a key in a Secret.
type SecretKeyRef struct {
	Name string `json:"name"`
	// +kubebuilder:default=url
	Key string `json:"key,omitempty"`
}

// WorkloadDependencySpec defines the desired behavior of the dependency relationship.
type WorkloadDependencySpec struct {
	Dependent  WorkloadRef      `json:"dependent"`
	DependsOn  []DependsOnEntry `json:"dependsOn"`
	OnDegraded OnDegradedSpec   `json:"onDegraded,omitempty"`
	// +kubebuilder:default=strict
	Mode EnforcementMode `json:"mode,omitempty"`
	// +optional
	Notify *NotifySpec `json:"notify,omitempty"`
}

// +kubebuilder:validation:Enum=Healthy;Degraded;Suspended;Paused;Observed;Unknown
type DependencyPhase string

const (
	PhaseHealthy   DependencyPhase = "Healthy"
	PhaseDegraded  DependencyPhase = "Degraded"
	PhaseSuspended DependencyPhase = "Suspended"
	PhasePaused    DependencyPhase = "Paused"
	PhaseObserved  DependencyPhase = "Observed" // observe mode: would have acted
	PhaseUnknown   DependencyPhase = "Unknown"
)

// WorkloadDependencyStatus is the observed state of a WorkloadDependency.
type WorkloadDependencyStatus struct {
	// Current phase of the dependency.
	Phase DependencyPhase `json:"phase,omitempty"`

	// Saved replica count before scale-to-zero, used for restore.
	// +optional
	SavedReplicas *int32 `json:"savedReplicas,omitempty"`

	// Timestamp when the dependency first became unhealthy (for hysteresis window).
	// +optional
	DegradedSince *metav1.Time `json:"degradedSince,omitempty"`

	// Timestamp when the dependent workload was suspended (for maxSuspendDuration).
	// +optional
	SuspendedAt *metav1.Time `json:"suspendedAt,omitempty"`

	// Timestamp when the dependency first became healthy again (for recovery window).
	// +optional
	HealthySince *metav1.Time `json:"healthySince,omitempty"`

	// Human-readable message about the current state.
	// +optional
	Message string `json:"message,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.savedReplicas`
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.message`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type WorkloadDependency struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec WorkloadDependencySpec `json:"spec"`

	// +optional
	Status WorkloadDependencyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true
type WorkloadDependencyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []WorkloadDependency `json:"items"`
}

func init() {
	SchemeBuilder.Register(&WorkloadDependency{}, &WorkloadDependencyList{})
}
