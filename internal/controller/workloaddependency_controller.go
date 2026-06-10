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
	batchv1 "k8s.io/api/batch/v1"
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
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *WorkloadDependencyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	wd := &depsv1alpha1.WorkloadDependency{}
	if err := r.Get(ctx, req.NamespacedName, wd); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("reconciling", "phase", wd.Status.Phase, "mode", wd.Spec.Mode)

	if wd.Annotations[depsv1alpha1.AnnotationPaused] == "true" {
		return r.setStatus(ctx, wd, depsv1alpha1.PhasePaused, "manually paused via klink.dev/paused annotation", nil)
	}

	dependentNS := wd.Spec.Dependent.Namespace
	if dependentNS == "" {
		dependentNS = wd.Namespace
	}

	dependent, err := r.getWorkload(ctx, wd.Spec.Dependent.Kind, wd.Spec.Dependent.Name, dependentNS)
	if err != nil {
		if errors.IsNotFound(err) {
			return r.setStatus(ctx, wd, depsv1alpha1.PhaseUnknown,
				fmt.Sprintf("dependent %s/%s not found", wd.Spec.Dependent.Kind, wd.Spec.Dependent.Name), nil)
		}
		return ctrl.Result{}, err
	}

	// Evaluate all dependencies
	allOk := true
	var unhealthyMsg string

	for _, dep := range wd.Spec.DependsOn {
		ns := dep.Namespace
		if ns == "" {
			ns = wd.Namespace
		}

		depWorkload, err := r.getWorkload(ctx, dep.Kind, dep.Name, ns)
		if err != nil {
			if errors.IsNotFound(err) {
				allOk = false
				unhealthyMsg = fmt.Sprintf("dependency %s/%s not found", dep.Kind, dep.Name)
				break
			}
			return ctrl.Result{}, err
		}

		status, statusErr := r.depStatus(ctx, depWorkload, dep.Condition, ns)
		if statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		log.Info("dependency status", "dep", dep.Name, "kind", dep.Kind, "status", status)

		switch status {
		case depHealthy, depCoSuspended:
			// ok
		case depUnhealthy:
			allOk = false
			unhealthyMsg = fmt.Sprintf("dependency %s %s is not healthy", dep.Kind, dep.Name)
		}
	}

	now := metav1.Now()

	if !allOk && wd.Status.Phase == depsv1alpha1.PhaseSuspended {
		return r.handleSuspended(ctx, wd, dependent, unhealthyMsg)
	}

	if allOk {
		return r.handleHealthy(ctx, wd, dependent, now)
	}
	return r.handleDegraded(ctx, wd, dependent, unhealthyMsg, now)
}

// workloadAccessor abstracts Deployment / StatefulSet / CronJob behind a common interface.
type workloadAccessor interface {
	client.Object
	// getReplicas returns desired replica count. -1 for CronJob (not applicable).
	getReplicas() int32
	// setReplicas updates the spec replica count. No-op for CronJob.
	setReplicas(n int32)
	// isSuspended returns true if CronJob.spec.suspend is true. Always false for Deployment/StatefulSet.
	isSuspended() bool
	// setSuspend sets CronJob.spec.suspend. No-op for Deployment/StatefulSet.
	setSuspend(v bool)
}

type deploymentAccessor struct{ *appsv1.Deployment }

func (a *deploymentAccessor) getReplicas() int32 {
	if a.Spec.Replicas == nil {
		return 1
	}
	return *a.Spec.Replicas
}
func (a *deploymentAccessor) setReplicas(n int32)   { a.Spec.Replicas = &n }
func (a *deploymentAccessor) isSuspended() bool      { return false }
func (a *deploymentAccessor) setSuspend(_ bool)      {}

type statefulSetAccessor struct{ *appsv1.StatefulSet }

func (a *statefulSetAccessor) getReplicas() int32 {
	if a.Spec.Replicas == nil {
		return 1
	}
	return *a.Spec.Replicas
}
func (a *statefulSetAccessor) setReplicas(n int32)   { a.Spec.Replicas = &n }
func (a *statefulSetAccessor) isSuspended() bool      { return false }
func (a *statefulSetAccessor) setSuspend(_ bool)      {}

type cronJobAccessor struct{ *batchv1.CronJob }

func (a *cronJobAccessor) getReplicas() int32 { return -1 }
func (a *cronJobAccessor) setReplicas(_ int32) {}
func (a *cronJobAccessor) isSuspended() bool {
	return a.Spec.Suspend != nil && *a.Spec.Suspend
}
func (a *cronJobAccessor) setSuspend(v bool) { a.Spec.Suspend = &v }

func (r *WorkloadDependencyReconciler) getWorkload(ctx context.Context, kind, name, ns string) (workloadAccessor, error) {
	key := types.NamespacedName{Name: name, Namespace: ns}
	switch kind {
	case "StatefulSet":
		obj := &appsv1.StatefulSet{}
		return &statefulSetAccessor{obj}, r.Get(ctx, key, obj)
	case "CronJob":
		obj := &batchv1.CronJob{}
		return &cronJobAccessor{obj}, r.Get(ctx, key, obj)
	default: // Deployment
		obj := &appsv1.Deployment{}
		return &deploymentAccessor{obj}, r.Get(ctx, key, obj)
	}
}

type depHealthStatus string

const (
	depHealthy     depHealthStatus = "Healthy"
	depCoSuspended depHealthStatus = "CoSuspended"
	depUnhealthy   depHealthStatus = "Unhealthy"
)

func (r *WorkloadDependencyReconciler) depStatus(ctx context.Context, w workloadAccessor, cond depsv1alpha1.HealthCondition, ns string) (depHealthStatus, error) {
	// CronJob as dependency: healthy if not suspended
	if cj, ok := w.(*cronJobAccessor); ok {
		if cj.isSuspended() {
			suspended, err := r.isSuspendedByKlink(ctx, cj.Name, ns)
			if err != nil {
				return depUnhealthy, err
			}
			if suspended {
				return depCoSuspended, nil
			}
			return depUnhealthy, nil
		}
		return depHealthy, nil
	}

	// Deployment / StatefulSet: check ready replicas
	replicas := w.getReplicas()
	if replicas == 0 {
		suspended, err := r.isSuspendedByKlink(ctx, w.GetName(), ns)
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

	var readyReplicas int32
	switch obj := w.(type) {
	case *deploymentAccessor:
		readyReplicas = obj.Status.ReadyReplicas
	case *statefulSetAccessor:
		readyReplicas = obj.Status.ReadyReplicas
	}

	readyPercent := int32(float64(readyReplicas) / float64(replicas) * 100)
	if readyPercent >= minPercent {
		return depHealthy, nil
	}
	return depUnhealthy, nil
}

func (r *WorkloadDependencyReconciler) isSuspendedByKlink(ctx context.Context, name, ns string) (bool, error) {
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
		if wd.Spec.Dependent.Name == name && depNS == ns {
			return true, nil
		}
	}
	return false, nil
}

func (r *WorkloadDependencyReconciler) handleDegraded(
	ctx context.Context,
	wd *depsv1alpha1.WorkloadDependency,
	dependent workloadAccessor,
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

	if err := r.suspendWorkload(ctx, wd, dependent, msg); err != nil {
		return ctrl.Result{}, err
	}

	return r.setStatus(ctx, wd, depsv1alpha1.PhaseSuspended, msg, &ctrl.Result{RequeueAfter: 15 * time.Second})
}

func (r *WorkloadDependencyReconciler) handleSuspended(
	ctx context.Context,
	wd *depsv1alpha1.WorkloadDependency,
	dependent workloadAccessor,
	msg string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if wd.Spec.Mode == depsv1alpha1.ModeStrict {
		var needsEnforce bool
		switch obj := dependent.(type) {
		case *cronJobAccessor:
			needsEnforce = !obj.isSuspended()
		default:
			needsEnforce = dependent.getReplicas() > 0
		}

		if needsEnforce {
			log.Info("strict mode: re-enforcing suspension", "name", dependent.GetName())
			if err := r.suspendWorkload(ctx, wd, dependent, msg); err != nil {
				return ctrl.Result{}, err
			}
			r.Recorder.Eventf(wd, corev1.EventTypeWarning, "StrictEnforced",
				"Re-enforced suspension on %s %s (strict mode): dependency still unhealthy",
				wd.Spec.Dependent.Kind, dependent.GetName())
		}
	}

	return r.setStatus(ctx, wd, depsv1alpha1.PhaseSuspended, msg, &ctrl.Result{RequeueAfter: 15 * time.Second})
}

func (r *WorkloadDependencyReconciler) handleHealthy(
	ctx context.Context,
	wd *depsv1alpha1.WorkloadDependency,
	dependent workloadAccessor,
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
		return r.setStatus(ctx, wd, depsv1alpha1.PhaseSuspended, "dependencies recovered, waiting recovery window",
			&ctrl.Result{RequeueAfter: recoveryWindow(wd)})
	}

	window := recoveryWindow(wd)
	if time.Since(wd.Status.HealthySince.Time) < window {
		remaining := window - time.Since(wd.Status.HealthySince.Time)
		log.Info("dependency healthy but within recovery window, waiting", "remaining", remaining)
		return ctrl.Result{RequeueAfter: remaining + time.Second}, nil
	}

	if err := r.restoreWorkload(ctx, wd, dependent); err != nil {
		return ctrl.Result{}, err
	}

	wd.Status.SavedReplicas = nil
	wd.Status.HealthySince = nil
	return r.setStatus(ctx, wd, depsv1alpha1.PhaseHealthy, "all dependencies healthy", nil)
}

// suspendWorkload scales Deployment/StatefulSet to zero or sets CronJob.suspend=true.
func (r *WorkloadDependencyReconciler) suspendWorkload(
	ctx context.Context,
	wd *depsv1alpha1.WorkloadDependency,
	w workloadAccessor,
	msg string,
) error {
	log := logf.FromContext(ctx)

	switch obj := w.(type) {
	case *cronJobAccessor:
		if obj.isSuspended() {
			return nil
		}
		patch := client.MergeFrom(obj.CronJob.DeepCopy())
		obj.setSuspend(true)
		if err := r.Patch(ctx, obj.CronJob, patch); err != nil {
			return err
		}
		log.Info("suspended cronjob", "name", obj.GetName())
		r.Recorder.Eventf(wd, corev1.EventTypeWarning, "CronJobSuspended",
			"Suspended CronJob %s: %s", obj.GetName(), msg)

	case *deploymentAccessor:
		replicas := obj.getReplicas()
		if replicas == 0 {
			return nil
		}
		saved := replicas
		wd.Status.SavedReplicas = &saved
		patch := client.MergeFrom(obj.Deployment.DeepCopy())
		obj.setReplicas(0)
		if err := r.Patch(ctx, obj.Deployment, patch); err != nil {
			return err
		}
		log.Info("scaled dependent to zero", "name", obj.GetName(), "savedReplicas", saved)
		r.Recorder.Eventf(wd, corev1.EventTypeWarning, "ScaledToZero",
			"Scaled %s %s to 0 (saved %d replicas): %s", wd.Spec.Dependent.Kind, obj.GetName(), saved, msg)

	case *statefulSetAccessor:
		replicas := obj.getReplicas()
		if replicas == 0 {
			return nil
		}
		saved := replicas
		wd.Status.SavedReplicas = &saved
		patch := client.MergeFrom(obj.StatefulSet.DeepCopy())
		obj.setReplicas(0)
		if err := r.Patch(ctx, obj.StatefulSet, patch); err != nil {
			return err
		}
		log.Info("scaled dependent to zero", "name", obj.GetName(), "savedReplicas", saved)
		r.Recorder.Eventf(wd, corev1.EventTypeWarning, "ScaledToZero",
			"Scaled %s %s to 0 (saved %d replicas): %s", wd.Spec.Dependent.Kind, obj.GetName(), saved, msg)
	}

	return nil
}

// restoreWorkload restores Deployment/StatefulSet replicas or unsuspends CronJob.
func (r *WorkloadDependencyReconciler) restoreWorkload(
	ctx context.Context,
	wd *depsv1alpha1.WorkloadDependency,
	w workloadAccessor,
) error {
	log := logf.FromContext(ctx)

	switch obj := w.(type) {
	case *cronJobAccessor:
		if !obj.isSuspended() {
			return nil
		}
		patch := client.MergeFrom(obj.CronJob.DeepCopy())
		obj.setSuspend(false)
		if err := r.Patch(ctx, obj.CronJob, patch); err != nil {
			return err
		}
		log.Info("unsuspended cronjob", "name", obj.GetName())
		r.Recorder.Eventf(wd, corev1.EventTypeNormal, "CronJobResumed",
			"Resumed CronJob %s after dependency recovery", obj.GetName())

	case *deploymentAccessor:
		if wd.Status.SavedReplicas == nil || *wd.Status.SavedReplicas == 0 {
			return nil
		}
		replicas := *wd.Status.SavedReplicas
		patch := client.MergeFrom(obj.Deployment.DeepCopy())
		obj.setReplicas(replicas)
		if err := r.Patch(ctx, obj.Deployment, patch); err != nil {
			return err
		}
		log.Info("restored dependent replicas", "name", obj.GetName(), "replicas", replicas)
		r.Recorder.Eventf(wd, corev1.EventTypeNormal, "ReplicasRestored",
			"Restored %s %s to %d replicas after dependency recovery", wd.Spec.Dependent.Kind, obj.GetName(), replicas)

	case *statefulSetAccessor:
		if wd.Status.SavedReplicas == nil || *wd.Status.SavedReplicas == 0 {
			return nil
		}
		replicas := *wd.Status.SavedReplicas
		patch := client.MergeFrom(obj.StatefulSet.DeepCopy())
		obj.setReplicas(replicas)
		if err := r.Patch(ctx, obj.StatefulSet, patch); err != nil {
			return err
		}
		log.Info("restored dependent replicas", "name", obj.GetName(), "replicas", replicas)
		r.Recorder.Eventf(wd, corev1.EventTypeNormal, "ReplicasRestored",
			"Restored %s %s to %d replicas after dependency recovery", wd.Spec.Dependent.Kind, obj.GetName(), replicas)
	}

	return nil
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
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.findWDsForWorkload)).
		Watches(&appsv1.StatefulSet{}, handler.EnqueueRequestsFromMapFunc(r.findWDsForWorkload)).
		Watches(&batchv1.CronJob{}, handler.EnqueueRequestsFromMapFunc(r.findWDsForWorkload)).
		Named("workloaddependency").
		Complete(r)
}

func (r *WorkloadDependencyReconciler) findWDsForWorkload(ctx context.Context, obj client.Object) []ctrl.Request {
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
