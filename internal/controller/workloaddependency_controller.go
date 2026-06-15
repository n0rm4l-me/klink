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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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
// +kubebuilder:rbac:groups=argoproj.io,resources=rollouts,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *WorkloadDependencyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	wd := &depsv1alpha1.WorkloadDependency{}
	if err := r.Get(ctx, req.NamespacedName, wd); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("reconciling", "phase", wd.Status.Phase, "mode", wd.Spec.Mode)

	// Handle deletion: restore replicas before allowing finalizer removal
	if !wd.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, wd)
	}

	// Ensure finalizer is present so we can restore replicas on deletion.
	// On first creation also check for dependency cycles.
	if !controllerutil.ContainsFinalizer(wd, depsv1alpha1.FinalizerName) {
		// Cycle detection on first admission
		if cycle, err := DetectCycle(ctx, r.Client, wd); err != nil {
			log.Error(err, "cycle detection failed, proceeding anyway")
		} else if cycle != nil {
			msg := fmt.Sprintf("dependency cycle detected: %s", FormatCycle(cycle))
			log.Error(nil, msg)
			r.Recorder.Eventf(wd, corev1.EventTypeWarning, "CycleDetected", msg)
			return r.setStatus(ctx, wd, depsv1alpha1.PhaseUnknown, msg, &ctrl.Result{})
		}

		controllerutil.AddFinalizer(wd, depsv1alpha1.FinalizerName)
		if err := r.Update(ctx, wd); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// nil-safe annotation check
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

	// Rollout-specific: if dependent is a Rollout currently progressing (canary/blue-green in flight),
	// defer all suspension actions — the stable version is still serving traffic.
	// We still track DegradedSince so the window starts counting, but we don't act.
	if !allOk {
		if ro, ok := dependent.(*rolloutAccessor); ok && ro.isProgressing() {
			log.Info("dependent Rollout is progressing, deferring suspension", "rollout", ro.GetName())
			return r.setStatus(ctx, wd, depsv1alpha1.PhaseDegraded,
				fmt.Sprintf("dependency unhealthy, action deferred — rollout %s is progressing", ro.GetName()),
				&ctrl.Result{RequeueAfter: 15 * time.Second})
		}
	}

	// Observe mode: log what would happen but take no action.
	if wd.Spec.Mode == depsv1alpha1.ModeObserve {
		if allOk {
			return r.setStatus(ctx, wd, depsv1alpha1.PhaseHealthy, "all dependencies healthy (observe mode)", nil)
		}
		observeMsg := fmt.Sprintf("observe mode: would scale %s/%s to 0 — %s", wd.Spec.Dependent.Kind, wd.Spec.Dependent.Name, unhealthyMsg)
		log.Info("observe mode: would act", "action", "ScaleToZero", "dependent", wd.Spec.Dependent.Name, "reason", unhealthyMsg)
		return r.setStatus(ctx, wd, depsv1alpha1.PhaseObserved, observeMsg, nil)
	}

	// Gate mode: never scale dependent — only block scale-up via admission webhook.
	if wd.Spec.Mode == depsv1alpha1.ModeGate {
		if allOk {
			return r.setStatus(ctx, wd, depsv1alpha1.PhaseHealthy, "all dependencies healthy", nil)
		}
		return r.setStatus(ctx, wd, depsv1alpha1.PhaseDegraded, unhealthyMsg, nil)
	}

	// Released: klink force-restored after maxSuspendDuration. Stay hands-off until
	// the dependency genuinely recovers, then resume normal tracking.
	if wd.Status.Phase == depsv1alpha1.PhaseReleased {
		if allOk {
			return r.handleHealthy(ctx, wd, dependent, now)
		}
		// Still unhealthy — do nothing, workload stays running (we gave up on suspending it)
		return r.setStatus(ctx, wd, depsv1alpha1.PhaseReleased,
			"force-restored after maxSuspendDuration; waiting for dependency to recover",
			&ctrl.Result{RequeueAfter: 30 * time.Second})
	}

	// maxSuspendDuration: auto-restore if suspended too long, even if dependency still unhealthy.
	if !allOk && wd.Status.Phase == depsv1alpha1.PhaseSuspended {
		maxDur := wd.Spec.OnDegraded.MaxSuspendDuration.Duration
		if maxDur > 0 && wd.Status.SuspendedAt != nil {
			elapsed := time.Since(wd.Status.SuspendedAt.Time)
			if elapsed >= maxDur {
				log.Info("maxSuspendDuration exceeded, restoring despite unhealthy dependency",
					"dependent", wd.Spec.Dependent.Name,
					"suspendedFor", elapsed.Round(time.Second),
					"maxSuspendDuration", maxDur)
				r.Recorder.Eventf(wd, corev1.EventTypeWarning, "MaxSuspendDurationExceeded",
					"Restoring %s after %s (maxSuspendDuration exceeded) — dependency still unhealthy",
					wd.Spec.Dependent.Name, maxDur)
				if err := r.restoreWorkload(ctx, wd, dependent); err != nil {
					return ctrl.Result{}, err
				}
				wd.Status.SavedReplicas = nil
				wd.Status.HealthySince = nil
				// Enter Released phase — won't re-suspend until dependency truly recovers
				return r.setStatus(ctx, wd, depsv1alpha1.PhaseReleased,
					"force-restored after maxSuspendDuration; waiting for dependency to recover", nil)
			}
			// Requeue precisely when maxSuspendDuration expires
			remaining := maxDur - elapsed
			return ctrl.Result{RequeueAfter: remaining + time.Second}, nil
		}
		return r.handleSuspended(ctx, wd, dependent, unhealthyMsg)
	}

	if allOk {
		return r.handleHealthy(ctx, wd, dependent, now)
	}
	return r.handleDegraded(ctx, wd, dependent, unhealthyMsg, now)
}

// handleDeletion restores workload replicas when a WorkloadDependency is deleted while
// its dependent is suspended. This prevents workloads from being stuck at 0 replicas forever.
func (r *WorkloadDependencyReconciler) handleDeletion(ctx context.Context, wd *depsv1alpha1.WorkloadDependency) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if wd.Status.Phase == depsv1alpha1.PhaseSuspended && wd.Status.SavedReplicas != nil && *wd.Status.SavedReplicas > 0 {
		dependentNS := wd.Spec.Dependent.Namespace
		if dependentNS == "" {
			dependentNS = wd.Namespace
		}
		dependent, err := r.getWorkload(ctx, wd.Spec.Dependent.Kind, wd.Spec.Dependent.Name, dependentNS)
		if err != nil {
			if !errors.IsNotFound(err) {
				// Transient API error — retry so we don't remove the finalizer
				// without restoring replicas. NotFound is acceptable (workload
				// already deleted), anything else we must retry.
				log.Error(err, "failed to get dependent workload for restore on deletion, retrying")
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			// Workload not found — nothing to restore, proceed with finalizer removal.
		} else {
			if restoreErr := r.restoreWorkload(ctx, wd, dependent); restoreErr != nil {
				log.Error(restoreErr, "failed to restore replicas on deletion, retrying")
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			log.Info("restored replicas before deletion", "dependent", wd.Spec.Dependent.Name)
		}
	}

	controllerutil.RemoveFinalizer(wd, depsv1alpha1.FinalizerName)
	if err := r.Update(ctx, wd); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
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
	case "Rollout":
		return getRollout(ctx, r.Client, name, ns)
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
	// Rollout as dependency: healthy if Healthy/Progressing/Paused, unhealthy if Degraded
	if ro, ok := w.(*rolloutAccessor); ok {
		if ro.isHealthyAsDependency() {
			return depHealthy, nil
		}
		// Rollout is Degraded — check if klink suspended it
		suspended, err := r.isSuspendedByKlink(ctx, ro.GetName(), ns)
		if err != nil {
			return depUnhealthy, err
		}
		if suspended {
			return depCoSuspended, nil
		}
		return depUnhealthy, nil
	}

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

// isSuspendedByKlink returns true if any Suspended WorkloadDependency has this workload
// as its dependent — meaning klink itself scaled it to zero.
// Uses a field index for O(1) lookup instead of cluster-wide List().
func (r *WorkloadDependencyReconciler) isSuspendedByKlink(ctx context.Context, name, ns string) (bool, error) {
	wdList := &depsv1alpha1.WorkloadDependencyList{}
	if err := r.List(ctx, wdList, client.MatchingFields{indexDependentName: ns + "/" + name}); err != nil {
		return false, err
	}
	for _, wd := range wdList.Items {
		if wd.Status.Phase == depsv1alpha1.PhaseSuspended {
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
		// Never re-enforce during an active canary — wait for it to complete
		if ro, ok := dependent.(*rolloutAccessor); ok && ro.isProgressing() {
			log.Info("strict mode: deferring re-enforcement, rollout is progressing", "rollout", ro.GetName())
			return r.setStatus(ctx, wd, depsv1alpha1.PhaseSuspended, msg, &ctrl.Result{RequeueAfter: 15 * time.Second})
		}

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
		recordScaleToZero(wd.Namespace, "CronJob", obj.GetName())

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
		recordScaleToZero(wd.Namespace, "Deployment", obj.GetName())

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
		recordScaleToZero(wd.Namespace, "StatefulSet", obj.GetName())

	case *rolloutAccessor:
		replicas := obj.getReplicas()
		if replicas == 0 {
			return nil
		}
		saved := replicas
		wd.Status.SavedReplicas = &saved
		base := obj.Unstructured.DeepCopy()
		obj.setReplicas(0)
		if err := r.Patch(ctx, obj.Unstructured, client.MergeFrom(base)); err != nil {
			return err
		}
		log.Info("scaled rollout to zero", "name", obj.GetName(), "savedReplicas", saved)
		r.Recorder.Eventf(wd, corev1.EventTypeWarning, "ScaledToZero",
			"Scaled Rollout %s to 0 (saved %d replicas): %s", obj.GetName(), saved, msg)
		recordScaleToZero(wd.Namespace, "Rollout", obj.GetName())
	}

	return nil
}

// restoreWorkload restores Deployment/StatefulSet/Rollout replicas or unsuspends CronJob.
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
		recordReplicasRestored(wd.Namespace, "CronJob", obj.GetName())

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
		recordReplicasRestored(wd.Namespace, "Deployment", obj.GetName())

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
		recordReplicasRestored(wd.Namespace, "StatefulSet", obj.GetName())

	case *rolloutAccessor:
		if wd.Status.SavedReplicas == nil || *wd.Status.SavedReplicas == 0 {
			return nil
		}
		replicas := *wd.Status.SavedReplicas
		base := obj.Unstructured.DeepCopy()
		obj.setReplicas(replicas)
		if err := r.Patch(ctx, obj.Unstructured, client.MergeFrom(base)); err != nil {
			return err
		}
		log.Info("restored rollout replicas", "name", obj.GetName(), "replicas", replicas)
		r.Recorder.Eventf(wd, corev1.EventTypeNormal, "ReplicasRestored",
			"Restored Rollout %s to %d replicas after dependency recovery", obj.GetName(), replicas)
		recordReplicasRestored(wd.Namespace, "Rollout", obj.GetName())
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
	// Snapshot the fields we intend to write so we can re-apply them after a
	// conflict re-read.  A 409 Conflict means another reconcile loop updated
	// the server object; we must re-read to get the latest resourceVersion and
	// then re-apply our changes, otherwise critical fields (SavedReplicas,
	// DegradedSince, SuspendedAt) are silently lost and workloads can get stuck
	// at 0 replicas with no automated recovery.
	savedReplicas := wd.Status.SavedReplicas
	degradedSince := wd.Status.DegradedSince
	healthySince := wd.Status.HealthySince

	prevPhase := wd.Status.Phase

	applyFields := func() {
		if phase == depsv1alpha1.PhaseSuspended {
			if wd.Status.SuspendedAt == nil {
				now := metav1.Now()
				wd.Status.SuspendedAt = &now
			}
		} else {
			wd.Status.SuspendedAt = nil
		}
		wd.Status.Phase = phase
		wd.Status.Message = msg
		// Restore the fields that were set by the caller before invoking setStatus.
		// On a re-read these come back as nil/zero from the server.
		if savedReplicas != nil {
			wd.Status.SavedReplicas = savedReplicas
		}
		if degradedSince != nil {
			wd.Status.DegradedSince = degradedSince
		}
		if healthySince != nil {
			wd.Status.HealthySince = healthySince
		}
		setCondition(wd, phase, msg)
	}

	applyFields()

	const maxRetries = 3
	var updateErr error
	for i := 0; i < maxRetries; i++ {
		updateErr = r.Status().Update(ctx, wd)
		if updateErr == nil {
			break
		}
		if !errors.IsConflict(updateErr) {
			recordReconcileError(wd.Namespace, "status_update")
			return ctrl.Result{}, updateErr
		}
		// Re-read the latest version and re-apply our desired fields.
		if getErr := r.Get(ctx, types.NamespacedName{Name: wd.Name, Namespace: wd.Namespace}, wd); getErr != nil {
			return ctrl.Result{}, getErr
		}
		applyFields()
	}
	if updateErr != nil {
		// Exhausted retries — requeue so the next reconcile starts fresh.
		return ctrl.Result{Requeue: true}, nil
	}

	recordPhase(wd.Namespace, wd.Name, string(phase))

	// Send webhook notification on phase transition
	if prevPhase != phase {
		maybeNotify(ctx, r.Client, wd, prevPhase, phase)
	}

	if result != nil {
		return *result, nil
	}
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

// setCondition updates the standard status.conditions[] array to reflect the current phase.
// Follows the Kubernetes API conventions for condition management.
func setCondition(wd *depsv1alpha1.WorkloadDependency, phase depsv1alpha1.DependencyPhase, msg string) {
	now := metav1.Now()

	// Map phase to condition type/status/reason
	type condSpec struct {
		condType string
		status   metav1.ConditionStatus
		reason   string
	}

	specs := map[depsv1alpha1.DependencyPhase]condSpec{
		depsv1alpha1.PhaseHealthy:   {"Ready", metav1.ConditionTrue, "DependenciesHealthy"},
		depsv1alpha1.PhaseDegraded:  {"Ready", metav1.ConditionFalse, "DependencyDegraded"},
		depsv1alpha1.PhaseSuspended: {"Ready", metav1.ConditionFalse, "DependentSuspended"},
		depsv1alpha1.PhasePaused:    {"Ready", metav1.ConditionUnknown, "Paused"},
		depsv1alpha1.PhaseUnknown:   {"Ready", metav1.ConditionUnknown, "DependentNotFound"},
	}

	spec, ok := specs[phase]
	if !ok {
		return
	}

	// Find existing condition
	for i, c := range wd.Status.Conditions {
		if c.Type == spec.condType {
			if c.Status == spec.status && c.Reason == spec.reason {
				// No transition — just update message and observed generation
				wd.Status.Conditions[i].Message = msg
				wd.Status.Conditions[i].ObservedGeneration = wd.Generation
				return
			}
			// Transition — update all fields
			wd.Status.Conditions[i] = metav1.Condition{
				Type:               spec.condType,
				Status:             spec.status,
				Reason:             spec.reason,
				Message:            msg,
				LastTransitionTime: now,
				ObservedGeneration: wd.Generation,
			}
			return
		}
	}

	// Add new condition
	wd.Status.Conditions = append(wd.Status.Conditions, metav1.Condition{
		Type:               spec.condType,
		Status:             spec.status,
		Reason:             spec.reason,
		Message:            msg,
		LastTransitionTime: now,
		ObservedGeneration: wd.Generation,
	})
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

// indexDependentName is the field index key for WD.spec.dependent.name + namespace.
const indexDependentName = ".spec.dependent.namespacedName"

// indexDependsOnName is the field index key for WD.spec.dependsOn[*] name+namespace.
// A single WD can have multiple dependsOn entries so this index returns multiple values.
const indexDependsOnName = ".spec.dependsOn.namespacedNames"

// SetupWithManager sets up the controller with the Manager.
func (r *WorkloadDependencyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Index by spec.dependent — used by isSuspendedByKlink (O(1) lookup).
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&depsv1alpha1.WorkloadDependency{},
		indexDependentName,
		func(obj client.Object) []string {
			wd := obj.(*depsv1alpha1.WorkloadDependency)
			ns := wd.Spec.Dependent.Namespace
			if ns == "" {
				ns = wd.Namespace
			}
			return []string{ns + "/" + wd.Spec.Dependent.Name}
		},
	); err != nil {
		return fmt.Errorf("register field index %s: %w", indexDependentName, err)
	}

	// Index by spec.dependsOn[*] — used by findWDsForWorkload to avoid
	// cluster-wide List() when a watched workload changes.
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&depsv1alpha1.WorkloadDependency{},
		indexDependsOnName,
		func(obj client.Object) []string {
			wd := obj.(*depsv1alpha1.WorkloadDependency)
			keys := make([]string, 0, len(wd.Spec.DependsOn))
			for _, dep := range wd.Spec.DependsOn {
				ns := dep.Namespace
				if ns == "" {
					ns = wd.Namespace
				}
				keys = append(keys, ns+"/"+dep.Name)
			}
			return keys
		},
	); err != nil {
		return fmt.Errorf("register field index %s: %w", indexDependsOnName, err)
	}

	rolloutObj := &unstructured.Unstructured{}
	rolloutObj.SetGroupVersionKind(rolloutGVK)

	return ctrl.NewControllerManagedBy(mgr).
		For(&depsv1alpha1.WorkloadDependency{}).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.findWDsForWorkload)).
		Watches(&appsv1.StatefulSet{}, handler.EnqueueRequestsFromMapFunc(r.findWDsForWorkload)).
		Watches(&batchv1.CronJob{}, handler.EnqueueRequestsFromMapFunc(r.findWDsForWorkload)).
		Watches(rolloutObj, handler.EnqueueRequestsFromMapFunc(r.findWDsForWorkload)).
		Named("workloaddependency").
		Complete(r)
}

// findWDsForWorkload maps a changed workload to all WorkloadDependency objects
// that reference it — either as a dependency (dependsOn) or as the dependent
// itself (for strict mode re-enforcement).
//
// Uses two field indexes (indexDependsOnName, indexDependentName) for O(1)
// lookups instead of a cluster-wide List() O(n) scan. This matters because
// this function is called on every Deployment/StatefulSet/CronJob/Rollout
// change in the cluster.
func (r *WorkloadDependencyReconciler) findWDsForWorkload(ctx context.Context, obj client.Object) []ctrl.Request {
	key := obj.GetNamespace() + "/" + obj.GetName()
	seen := map[types.NamespacedName]bool{}
	var requests []ctrl.Request

	enqueue := func(wd depsv1alpha1.WorkloadDependency) {
		k := types.NamespacedName{Name: wd.Name, Namespace: wd.Namespace}
		if !seen[k] {
			seen[k] = true
			requests = append(requests, ctrl.Request{NamespacedName: k})
		}
	}

	// 1. WDs that list this workload in spec.dependsOn
	depOnList := &depsv1alpha1.WorkloadDependencyList{}
	if err := r.List(ctx, depOnList, client.MatchingFields{indexDependsOnName: key}); err == nil {
		for _, wd := range depOnList.Items {
			enqueue(wd)
		}
	}

	// 2. WDs whose spec.dependent IS this workload (strict re-enforcement)
	dependentList := &depsv1alpha1.WorkloadDependencyList{}
	if err := r.List(ctx, dependentList, client.MatchingFields{indexDependentName: key}); err == nil {
		for _, wd := range dependentList.Items {
			enqueue(wd)
		}
	}

	return requests
}
