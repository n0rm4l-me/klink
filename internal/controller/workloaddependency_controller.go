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
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	depsv1alpha1 "github.com/n0rm4l-me/klink/api/v1alpha1"
)


// WorkloadDependencyReconciler reconciles a WorkloadDependency object
type WorkloadDependencyReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=deps.klink.dev,resources=workloaddependencies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=deps.klink.dev,resources=workloaddependencies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=deps.klink.dev,resources=workloaddependencies/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *WorkloadDependencyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	wd := &depsv1alpha1.WorkloadDependency{}
	if err := r.Get(ctx, req.NamespacedName, wd); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("reconciling", "phase", wd.Status.Phase, "mode", wd.Spec.Mode)

	// Paused via annotation — stop reconciling, just update status
	if wd.Annotations[depsv1alpha1.AnnotationPaused] == "true" {
		return r.setStatus(ctx, wd, depsv1alpha1.PhasePaused, "manually paused via klink.dev/paused annotation", nil)
	}

	dependentNS := wd.Spec.Dependent.Namespace
	if dependentNS == "" {
		dependentNS = wd.Namespace
	}

	dependent := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Name: wd.Spec.Dependent.Name, Namespace: dependentNS}, dependent); err != nil {
		if errors.IsNotFound(err) {
			return r.setStatus(ctx, wd, depsv1alpha1.PhaseUnknown, fmt.Sprintf("dependent deployment %s not found", wd.Spec.Dependent.Name), nil)
		}
		return ctrl.Result{}, err
	}

	// Evaluate all dependencies: each is healthy, co-suspended (klink put it to zero), or truly broken.
	allOk := true
	var unhealthyMsg string

	for _, dep := range wd.Spec.DependsOn {
		ns := dep.Namespace
		if ns == "" {
			ns = wd.Namespace
		}

		depDeploy := &appsv1.Deployment{}
		if err := r.Get(ctx, types.NamespacedName{Name: dep.Name, Namespace: ns}, depDeploy); err != nil {
			if errors.IsNotFound(err) {
				allOk = false
				unhealthyMsg = fmt.Sprintf("dependency %s not found", dep.Name)
				break
			}
			return ctrl.Result{}, err
		}

		status, err := r.depStatus(ctx, depDeploy, dep.Condition, ns)
		if err != nil {
			return ctrl.Result{}, err
		}
		log.Info("dependency status", "dep", dep.Name, "status", status)

		switch status {
		case depHealthy:
			// ok
		case depCoSuspended:
			// klink suspended it — treat as ok for recovery purposes
		case depUnhealthy:
			allOk = false
			unhealthyMsg = fmt.Sprintf("dependency %s is not healthy (%d/%d ready)",
				dep.Name, depDeploy.Status.ReadyReplicas, depDeploy.Status.Replicas)
		}
	}

	now := metav1.Now()

	// If already Suspended and dependency still unhealthy — strict mode re-enforces directly,
	// no need to go through window check again.
	if !allOk && wd.Status.Phase == depsv1alpha1.PhaseSuspended {
		return r.handleSuspended(ctx, wd, dependent, unhealthyMsg)
	}

	if allOk {
		return r.handleHealthy(ctx, wd, dependent, now)
	}
	return r.handleDegraded(ctx, wd, dependent, unhealthyMsg, now)
}

type depHealthStatus string

const (
	depHealthy     depHealthStatus = "Healthy"
	depCoSuspended depHealthStatus = "CoSuspended"
	depUnhealthy   depHealthStatus = "Unhealthy"
)

// depStatus checks if a dependency deployment is healthy, co-suspended by klink, or truly broken.
// CoSuspended means another WorkloadDependency object is in Suspended phase with this deployment as dependent —
// i.e. klink itself scaled it to zero. This resolves mutual dependency deadlock (A→B, B→A).
func (r *WorkloadDependencyReconciler) depStatus(ctx context.Context, d *appsv1.Deployment, cond depsv1alpha1.HealthCondition, ns string) (depHealthStatus, error) {
	desired := d.Spec.Replicas
	if desired == nil || *desired == 0 {
		// Check if another WD is responsible for scaling this deployment to zero
		suspended, err := r.isSuspendedByKlink(ctx, d.Name, ns)
		if err != nil {
			return depUnhealthy, err
		}
		if suspended {
			return depCoSuspended, nil
		}
		return depUnhealthy, nil
	}
	minPercent := cond.MinReadyPercent
	if minPercent == 0 {
		minPercent = 100
	}
	readyPercent := int32(float64(d.Status.ReadyReplicas) / float64(*desired) * 100)
	if readyPercent >= minPercent {
		return depHealthy, nil
	}
	return depUnhealthy, nil
}

// isSuspendedByKlink returns true if any WorkloadDependency in Suspended phase
// has this deployment as its dependent — meaning klink scaled it to zero intentionally.
func (r *WorkloadDependencyReconciler) isSuspendedByKlink(ctx context.Context, deployName, ns string) (bool, error) {
	wdList := &depsv1alpha1.WorkloadDependencyList{}
	if err := r.List(ctx, wdList); err != nil {
		return false, err
	}
	for _, wd := range wdList.Items {
		if wd.Status.Phase != depsv1alpha1.PhaseSuspended {
			continue
		}
		depNS := wd.Spec.Dependent.Namespace
		if depNS == "" {
			depNS = wd.Namespace
		}
		if wd.Spec.Dependent.Name == deployName && depNS == ns {
			return true, nil
		}
	}
	return false, nil
}

func (r *WorkloadDependencyReconciler) handleDegraded(
	ctx context.Context,
	wd *depsv1alpha1.WorkloadDependency,
	dependent *appsv1.Deployment,
	msg string,
	now metav1.Time,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if wd.Status.DegradedSince == nil {
		wd.Status.DegradedSince = &now
		wd.Status.HealthySince = nil
		return r.setStatus(ctx, wd, depsv1alpha1.PhaseDegraded, msg, &ctrl.Result{RequeueAfter: degradedWindow(wd)})
	}

	window := degradedWindow(wd)
	if time.Since(wd.Status.DegradedSince.Time) < window {
		remaining := window - time.Since(wd.Status.DegradedSince.Time)
		log.Info("dependency unhealthy but within window, waiting", "remaining", remaining)
		return ctrl.Result{RequeueAfter: remaining + time.Second}, nil
	}

	// Scale dependent to zero and save replicas
	currentReplicas := dependent.Spec.Replicas
	if currentReplicas != nil && *currentReplicas > 0 {
		saved := *currentReplicas
		wd.Status.SavedReplicas = &saved

		zero := int32(0)
		patch := client.MergeFrom(dependent.DeepCopy())
		dependent.Spec.Replicas = &zero
		if err := r.Patch(ctx, dependent, patch); err != nil {
			return ctrl.Result{}, err
		}

		log.Info("scaled dependent to zero", "deployment", dependent.Name, "savedReplicas", saved)
		r.Recorder.Eventf(wd, corev1.EventTypeWarning, "ScaledToZero",
			"Scaled %s to 0 (saved %d replicas): %s", dependent.Name, saved, msg)
	}

	return r.setStatus(ctx, wd, depsv1alpha1.PhaseSuspended, msg, &ctrl.Result{RequeueAfter: 15 * time.Second})
}

// handleSuspended is called when WD is already Suspended and dependency is still unhealthy.
// In strict mode it re-enforces scale-to-zero if someone manually scaled up.
func (r *WorkloadDependencyReconciler) handleSuspended(
	ctx context.Context,
	wd *depsv1alpha1.WorkloadDependency,
	dependent *appsv1.Deployment,
	msg string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if wd.Spec.Mode == depsv1alpha1.ModeStrict {
		currentReplicas := dependent.Spec.Replicas
		if currentReplicas != nil && *currentReplicas > 0 {
			log.Info("strict mode: re-enforcing scale-to-zero", "deployment", dependent.Name, "currentReplicas", *currentReplicas)
			zero := int32(0)
			patch := client.MergeFrom(dependent.DeepCopy())
			dependent.Spec.Replicas = &zero
			if err := r.Patch(ctx, dependent, patch); err != nil {
				return ctrl.Result{}, err
			}
			r.Recorder.Eventf(wd, corev1.EventTypeWarning, "StrictEnforced",
				"Re-enforced scale-to-zero on %s (strict mode): dependency still unhealthy", dependent.Name)
		}
	}

	return r.setStatus(ctx, wd, depsv1alpha1.PhaseSuspended, msg, &ctrl.Result{RequeueAfter: 15 * time.Second})
}

func (r *WorkloadDependencyReconciler) handleHealthy(
	ctx context.Context,
	wd *depsv1alpha1.WorkloadDependency,
	dependent *appsv1.Deployment,
	now metav1.Time,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	wd.Status.DegradedSince = nil

	if wd.Status.Phase != depsv1alpha1.PhaseSuspended {
		wd.Status.HealthySince = nil
		return r.setStatus(ctx, wd, depsv1alpha1.PhaseHealthy, "all dependencies healthy", nil)
	}

	if wd.Status.HealthySince == nil {
		wd.Status.HealthySince = &now
		return r.setStatus(ctx, wd, depsv1alpha1.PhaseSuspended, "dependencies recovered, waiting recovery window", &ctrl.Result{RequeueAfter: recoveryWindow(wd)})
	}

	window := recoveryWindow(wd)
	if time.Since(wd.Status.HealthySince.Time) < window {
		remaining := window - time.Since(wd.Status.HealthySince.Time)
		log.Info("dependency healthy but within recovery window, waiting", "remaining", remaining)
		return ctrl.Result{RequeueAfter: remaining + time.Second}, nil
	}

	// Restore replicas
	if wd.Status.SavedReplicas != nil && *wd.Status.SavedReplicas > 0 {
		replicas := *wd.Status.SavedReplicas
		patch := client.MergeFrom(dependent.DeepCopy())
		dependent.Spec.Replicas = &replicas
		if err := r.Patch(ctx, dependent, patch); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("restored dependent replicas", "deployment", dependent.Name, "replicas", replicas)
		r.Recorder.Eventf(wd, corev1.EventTypeNormal, "ReplicasRestored",
			"Restored %s to %d replicas after dependency recovery", dependent.Name, replicas)
	}

	wd.Status.SavedReplicas = nil
	wd.Status.HealthySince = nil
	return r.setStatus(ctx, wd, depsv1alpha1.PhaseHealthy, "all dependencies healthy", nil)
}

func (r *WorkloadDependencyReconciler) setStatus(
	ctx context.Context,
	wd *depsv1alpha1.WorkloadDependency,
	phase depsv1alpha1.DependencyPhase,
	msg string,
	result *ctrl.Result,
) (ctrl.Result, error) {
	wd.Status.Phase = phase
	wd.Status.Message = msg

	if err := r.Status().Update(ctx, wd); err != nil {
		return ctrl.Result{}, err
	}

	if result != nil {
		return *result, nil
	}
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

func degradedWindow(wd *depsv1alpha1.WorkloadDependency) time.Duration {
	for _, dep := range wd.Spec.DependsOn {
		if dep.Condition.Window.Duration > 0 {
			return dep.Condition.Window.Duration
		}
	}
	return 30 * time.Second
}

func recoveryWindow(wd *depsv1alpha1.WorkloadDependency) time.Duration {
	for _, dep := range wd.Spec.DependsOn {
		if dep.Condition.RecoveryWindow.Duration > 0 {
			return dep.Condition.RecoveryWindow.Duration
		}
	}
	return 60 * time.Second
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkloadDependencyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&depsv1alpha1.WorkloadDependency{}).
		Watches(
			&appsv1.Deployment{},
			handler.EnqueueRequestsFromMapFunc(r.findWDsForDeployment),
		).
		Named("workloaddependency").
		Complete(r)
}

// findWDsForDeployment maps a Deployment change to all WorkloadDependency objects that reference it —
// either as a dependency (dependsOn) or as the dependent itself (for strict mode re-enforcement).
func (r *WorkloadDependencyReconciler) findWDsForDeployment(ctx context.Context, obj client.Object) []ctrl.Request {
	wdList := &depsv1alpha1.WorkloadDependencyList{}
	if err := r.List(ctx, wdList); err != nil {
		return nil
	}

	seen := map[types.NamespacedName]bool{}
	var requests []ctrl.Request

	enqueue := func(wd depsv1alpha1.WorkloadDependency) {
		key := types.NamespacedName{Name: wd.Name, Namespace: wd.Namespace}
		if !seen[key] {
			seen[key] = true
			requests = append(requests, ctrl.Request{NamespacedName: key})
		}
	}

	for _, wd := range wdList.Items {
		// Watch dependsOn deployments
		for _, dep := range wd.Spec.DependsOn {
			ns := dep.Namespace
			if ns == "" {
				ns = wd.Namespace
			}
			if dep.Name == obj.GetName() && ns == obj.GetNamespace() {
				enqueue(wd)
				break
			}
		}

		// Watch dependent deployment too (for strict mode re-enforcement)
		depNS := wd.Spec.Dependent.Namespace
		if depNS == "" {
			depNS = wd.Namespace
		}
		if wd.Spec.Dependent.Name == obj.GetName() && depNS == obj.GetNamespace() {
			enqueue(wd)
		}
	}
	return requests
}
