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
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// dependencyPhase tracks the current phase of each WorkloadDependency.
	// Labels: namespace, name, phase.
	dependencyPhase = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "klink",
			Name:      "dependency_phase",
			Help:      "Current phase of WorkloadDependency resources (1 = active phase, 0 = not in this phase).",
		},
		[]string{"namespace", "name", "phase"},
	)

	// scaleToZeroTotal counts scale-to-zero actions performed.
	scaleToZeroTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "klink",
			Name:      "scale_to_zero_total",
			Help:      "Total number of scale-to-zero actions performed on dependent workloads.",
		},
		[]string{"namespace", "dependent_kind", "dependent_name"},
	)

	// replicasRestoredTotal counts replica restore actions.
	replicasRestoredTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "klink",
			Name:      "replicas_restored_total",
			Help:      "Total number of replica restore actions performed after dependency recovery.",
		},
		[]string{"namespace", "dependent_kind", "dependent_name"},
	)

	// reconcileErrorsTotal counts reconciliation errors by type.
	reconcileErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "klink",
			Name:      "reconcile_errors_total",
			Help:      "Total number of reconciliation errors.",
		},
		[]string{"namespace", "error_type"},
	)

	// suspendedWorkloads tracks the number of currently suspended workloads.
	suspendedWorkloads = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "klink",
			Name:      "suspended_workloads",
			Help:      "Number of workloads currently suspended by klink.",
		},
		[]string{"namespace", "kind"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		dependencyPhase,
		scaleToZeroTotal,
		replicasRestoredTotal,
		reconcileErrorsTotal,
		suspendedWorkloads,
	)
}

// recordPhase updates the phase gauge for a WorkloadDependency.
// Sets the active phase to 1 and all others to 0.
func recordPhase(namespace, name string, phase string) {
	allPhases := []string{"Healthy", "Degraded", "Suspended", "Paused", "Unknown"}
	for _, p := range allPhases {
		val := float64(0)
		if p == phase {
			val = 1
		}
		dependencyPhase.WithLabelValues(namespace, name, p).Set(val)
	}
}

// recordScaleToZero increments the scale-to-zero counter.
func recordScaleToZero(namespace, kind, name string) {
	scaleToZeroTotal.WithLabelValues(namespace, kind, name).Inc()
	suspendedWorkloads.WithLabelValues(namespace, kind).Inc()
}

// recordReplicasRestored increments the restore counter.
func recordReplicasRestored(namespace, kind, name string) {
	replicasRestoredTotal.WithLabelValues(namespace, kind, name).Inc()
	suspendedWorkloads.WithLabelValues(namespace, kind).Dec()
}

// recordReconcileError increments the error counter.
func recordReconcileError(namespace, errorType string) {
	reconcileErrorsTotal.WithLabelValues(namespace, errorType).Inc()
}
