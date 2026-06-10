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
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var rolloutGVK = schema.GroupVersionKind{
	Group:   "argoproj.io",
	Version: "v1alpha1",
	Kind:    "Rollout",
}

// Argo Rollout phase values from argoproj.io/argo-rollouts API.
const (
	rolloutPhaseHealthy     = "Healthy"
	rolloutPhaseDegraded    = "Degraded"
	rolloutPhaseProgressing = "Progressing"
	rolloutPhasePaused      = "Paused"
)

// rolloutAccessor wraps an unstructured Rollout and implements workloadAccessor.
// Using unstructured avoids importing the entire Argo Rollouts SDK.
// Embedding *unstructured.Unstructured satisfies the client.Object interface automatically.
type rolloutAccessor struct {
	*unstructured.Unstructured
}

// getReplicas returns spec.replicas.
func (a *rolloutAccessor) getReplicas() int32 {
	r, found, err := unstructured.NestedInt64(a.Object, "spec", "replicas")
	if err != nil || !found {
		return 1
	}
	return int32(r)
}

func (a *rolloutAccessor) setReplicas(n int32) {
	_ = unstructured.SetNestedField(a.Object, int64(n), "spec", "replicas")
}

func (a *rolloutAccessor) isSuspended() bool { return false }
func (a *rolloutAccessor) setSuspend(_ bool)  {}

// phase returns the Rollout's status.phase string.
func (a *rolloutAccessor) phase() string {
	p, _, _ := unstructured.NestedString(a.Object, "status", "phase")
	return p
}

// isProgressing returns true when a canary or blue-green rollout is in progress.
func (a *rolloutAccessor) isProgressing() bool {
	return a.phase() == rolloutPhaseProgressing
}

// isHealthyAsDependency returns true when this Rollout should be considered
// healthy from the perspective of other services depending on it.
// Progressing is treated as healthy because the stable version is still serving traffic.
func (a *rolloutAccessor) isHealthyAsDependency() bool {
	switch a.phase() {
	case rolloutPhaseHealthy, rolloutPhaseProgressing, rolloutPhasePaused:
		return true
	default:
		return false
	}
}

func getRollout(ctx context.Context, c client.Client, name, ns string) (*rolloutAccessor, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(rolloutGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, obj); err != nil {
		return nil, err
	}
	return &rolloutAccessor{obj}, nil
}
