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

// Package e2e runs end-to-end tests against a real Kubernetes cluster.
// Requires: kubectl context pointing to a test cluster (kind recommended),
// klink operator deployed in klink-system namespace.
//
// Run with:
//
//	KUBECONFIG=~/.kube/config go test ./test/e2e/... -v -timeout 10m
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	depsv1alpha1 "github.com/n0rm4l-me/klink/api/v1alpha1"
)

const (
	e2eNamespace   = "klink-e2e"
	operatorNS     = "klink-system"
	pollInterval   = 2 * time.Second
	shortTimeout   = 60 * time.Second
	defaultTimeout = 120 * time.Second
)

var (
	k8sClient  client.Client
	kubeClient kubernetes.Interface
	ctx        = context.Background()
)

func TestMain(m *testing.M) {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = home + "/.kube/config"
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load kubeconfig: %v\n", err)
		os.Exit(1)
	}

	scheme := buildScheme()
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create client: %v\n", err)
		os.Exit(1)
	}

	kubeClient, err = kubernetes.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create kubernetes client: %v\n", err)
		os.Exit(1)
	}

	// Create e2e namespace
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: e2eNamespace}}
	_ = k8sClient.Create(ctx, ns)

	code := m.Run()

	// Cleanup
	_ = k8sClient.Delete(ctx, ns)
	os.Exit(code)
}

func buildScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = appsv1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = depsv1alpha1.AddToScheme(s)
	return s
}

// TestScaleToZeroAndRestore verifies the full scale-to-zero and restore lifecycle.
func TestScaleToZeroAndRestore(t *testing.T) {
	t.Log("creating test deployments")
	dep := makeDeployment("database", 2)
	svc := makeDeployment("payments", 2)
	must(t, k8sClient.Create(ctx, dep))
	must(t, k8sClient.Create(ctx, svc))
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, dep)
		_ = k8sClient.Delete(ctx, svc)
	})

	waitDeploymentReady(t, "database", 2, shortTimeout)
	waitDeploymentReady(t, "payments", 2, shortTimeout)

	wd := makeWD("payments-needs-database", "payments", []string{"database"}, depsv1alpha1.ModeSoft, "10s", "15s")
	must(t, k8sClient.Create(ctx, wd))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, wd) })

	waitWDPhase(t, "payments-needs-database", depsv1alpha1.PhaseHealthy, shortTimeout)

	t.Log("scaling database to 0")
	scaleDeployment(t, "database", 0)

	t.Log("waiting for payments to be suspended")
	waitWDPhase(t, "payments-needs-database", depsv1alpha1.PhaseSuspended, defaultTimeout)
	waitDeploymentReplicas(t, "payments", 0, shortTimeout)

	// Verify savedReplicas
	wdObj := getWD(t, "payments-needs-database")
	if wdObj.Status.SavedReplicas == nil || *wdObj.Status.SavedReplicas != 2 {
		t.Errorf("expected savedReplicas=2, got %v", wdObj.Status.SavedReplicas)
	}

	// Verify conditions
	if len(wdObj.Status.Conditions) == 0 {
		t.Error("expected status.conditions to be populated")
	}

	t.Log("restoring database")
	scaleDeployment(t, "database", 2)
	waitDeploymentReady(t, "database", 2, shortTimeout)

	t.Log("waiting for payments to be restored")
	waitWDPhase(t, "payments-needs-database", depsv1alpha1.PhaseHealthy, defaultTimeout)
	waitDeploymentReplicas(t, "payments", 2, shortTimeout)
}

// TestMutualDependencyNoDeadlock verifies A→B + B→A doesn't deadlock.
func TestMutualDependencyNoDeadlock(t *testing.T) {
	foo := makeDeployment("foo", 2)
	bar := makeDeployment("bar", 2)
	must(t, k8sClient.Create(ctx, foo))
	must(t, k8sClient.Create(ctx, bar))
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, foo)
		_ = k8sClient.Delete(ctx, bar)
	})

	waitDeploymentReady(t, "foo", 2, shortTimeout)
	waitDeploymentReady(t, "bar", 2, shortTimeout)

	wdA := makeWD("bar-needs-foo", "bar", []string{"foo"}, depsv1alpha1.ModeSoft, "10s", "15s")
	wdB := makeWD("foo-needs-bar", "foo", []string{"bar"}, depsv1alpha1.ModeSoft, "10s", "15s")
	must(t, k8sClient.Create(ctx, wdA))
	must(t, k8sClient.Create(ctx, wdB))
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, wdA)
		_ = k8sClient.Delete(ctx, wdB)
	})

	waitWDPhase(t, "bar-needs-foo", depsv1alpha1.PhaseHealthy, shortTimeout)
	waitWDPhase(t, "foo-needs-bar", depsv1alpha1.PhaseHealthy, shortTimeout)

	t.Log("killing foo")
	scaleDeployment(t, "foo", 0)

	waitWDPhase(t, "bar-needs-foo", depsv1alpha1.PhaseSuspended, defaultTimeout)

	// foo-needs-bar should stay Healthy (bar is CoSuspended, not really broken)
	time.Sleep(5 * time.Second)
	wdBObj := getWD(t, "foo-needs-bar")
	if wdBObj.Status.Phase != depsv1alpha1.PhaseHealthy {
		t.Errorf("foo-needs-bar should be Healthy (CoSuspended), got %s", wdBObj.Status.Phase)
	}

	t.Log("restoring foo manually")
	scaleDeployment(t, "foo", 2)
	waitDeploymentReady(t, "foo", 2, shortTimeout)

	t.Log("bar should auto-restore")
	waitWDPhase(t, "bar-needs-foo", depsv1alpha1.PhaseHealthy, defaultTimeout)
	waitDeploymentReplicas(t, "bar", 2, shortTimeout)
}

// TestFinalizerRestoresOnDeletion verifies replicas are restored when WD is deleted.
func TestFinalizerRestoresOnDeletion(t *testing.T) {
	dep := makeDeployment("dep-src", 2)
	svc := makeDeployment("dep-svc", 2)
	must(t, k8sClient.Create(ctx, dep))
	must(t, k8sClient.Create(ctx, svc))
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, dep)
		_ = k8sClient.Delete(ctx, svc)
	})

	waitDeploymentReady(t, "dep-src", 2, shortTimeout)
	waitDeploymentReady(t, "dep-svc", 2, shortTimeout)

	wd := makeWD("finalizer-test", "dep-svc", []string{"dep-src"}, depsv1alpha1.ModeSoft, "10s", "15s")
	must(t, k8sClient.Create(ctx, wd))

	waitWDPhase(t, "finalizer-test", depsv1alpha1.PhaseHealthy, shortTimeout)

	// Suspend dep-svc
	scaleDeployment(t, "dep-src", 0)
	waitWDPhase(t, "finalizer-test", depsv1alpha1.PhaseSuspended, defaultTimeout)
	waitDeploymentReplicas(t, "dep-svc", 0, shortTimeout)

	// Delete the WD — finalizer should restore dep-svc replicas
	t.Log("deleting WorkloadDependency")
	must(t, k8sClient.Delete(ctx, wd))

	// Wait for WD to disappear and dep-svc to be restored
	waitDeploymentReplicas(t, "dep-svc", 2, defaultTimeout)
	t.Log("replicas restored by finalizer")
}

// TestPauseAnnotation verifies pausing disables enforcement.
func TestPauseAnnotation(t *testing.T) {
	dep := makeDeployment("pause-src", 2)
	svc := makeDeployment("pause-svc", 2)
	must(t, k8sClient.Create(ctx, dep))
	must(t, k8sClient.Create(ctx, svc))
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, dep)
		_ = k8sClient.Delete(ctx, svc)
	})

	waitDeploymentReady(t, "pause-src", 2, shortTimeout)

	wd := makeWD("pause-test", "pause-svc", []string{"pause-src"}, depsv1alpha1.ModeStrict, "10s", "15s")
	must(t, k8sClient.Create(ctx, wd))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, wd) })

	waitWDPhase(t, "pause-test", depsv1alpha1.PhaseHealthy, shortTimeout)

	// Kill dependency
	scaleDeployment(t, "pause-src", 0)
	waitWDPhase(t, "pause-test", depsv1alpha1.PhaseSuspended, defaultTimeout)

	// Pause klink
	kubectl(t, "annotate", "workloaddependency", "pause-test",
		"-n", e2eNamespace, "klink.dev/paused=true", "--overwrite")
	waitWDPhase(t, "pause-test", depsv1alpha1.PhasePaused, shortTimeout)

	// Manually scale up — should stay up (klink paused)
	scaleDeployment(t, "pause-svc", 3)
	time.Sleep(10 * time.Second)
	waitDeploymentReplicas(t, "pause-svc", 3, shortTimeout)
	t.Log("pause is working — manual scale not reverted")
}

// TestStrictModeRevertsManualScale verifies strict mode reverts manual scale-ups.
func TestStrictModeRevertsManualScale(t *testing.T) {
	dep := makeDeployment("strict-src", 2)
	svc := makeDeployment("strict-svc", 2)
	must(t, k8sClient.Create(ctx, dep))
	must(t, k8sClient.Create(ctx, svc))
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, dep)
		_ = k8sClient.Delete(ctx, svc)
	})

	waitDeploymentReady(t, "strict-src", 2, shortTimeout)

	wd := makeWD("strict-test", "strict-svc", []string{"strict-src"}, depsv1alpha1.ModeStrict, "10s", "15s")
	must(t, k8sClient.Create(ctx, wd))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, wd) })

	waitWDPhase(t, "strict-test", depsv1alpha1.PhaseHealthy, shortTimeout)
	scaleDeployment(t, "strict-src", 0)
	waitWDPhase(t, "strict-test", depsv1alpha1.PhaseSuspended, defaultTimeout)
	waitDeploymentReplicas(t, "strict-svc", 0, shortTimeout)

	// Manually scale up
	scaleDeployment(t, "strict-svc", 5)

	// Strict mode should revert within 15+poll seconds
	if err := wait.PollUntilContextTimeout(ctx, pollInterval, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		d := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "strict-svc", Namespace: e2eNamespace}, d); err != nil {
			return false, nil
		}
		if d.Spec.Replicas == nil {
			return false, nil
		}
		return *d.Spec.Replicas == 0, nil
	}); err != nil {
		t.Error("strict mode did not revert manual scale-up to 0")
	}
}

// --- Helpers ---

func makeDeployment(name string, replicas int32) *appsv1.Deployment {
	r := replicas
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: e2eNamespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &r,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: name, Image: "nginx:alpine"}},
				},
			},
		},
	}
}

func makeWD(name, dependent string, deps []string, mode depsv1alpha1.EnforcementMode, window, recovery string) *depsv1alpha1.WorkloadDependency {
	var dependsOn []depsv1alpha1.DependsOnEntry
	for _, d := range deps {
		dependsOn = append(dependsOn, depsv1alpha1.DependsOnEntry{
			Kind: "Deployment", Name: d,
			Condition: depsv1alpha1.HealthCondition{
				MinReadyPercent: 100,
				Window:          metav1.Duration{Duration: mustParseDuration(window)},
				RecoveryWindow:  metav1.Duration{Duration: mustParseDuration(recovery)},
			},
		})
	}
	return &depsv1alpha1.WorkloadDependency{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: e2eNamespace},
		Spec: depsv1alpha1.WorkloadDependencySpec{
			Dependent:  depsv1alpha1.WorkloadRef{Kind: "Deployment", Name: dependent},
			DependsOn:  dependsOn,
			OnDegraded: depsv1alpha1.OnDegradedSpec{Action: depsv1alpha1.ActionScaleToZero},
			Mode:       mode,
		},
	}
}

func mustParseDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		panic(err)
	}
	return d
}

func scaleDeployment(t *testing.T, name string, replicas int32) {
	t.Helper()
	dep := &appsv1.Deployment{}
	must(t, k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: e2eNamespace}, dep))
	dep.Spec.Replicas = &replicas
	must(t, k8sClient.Update(ctx, dep))
}

func getWD(t *testing.T, name string) *depsv1alpha1.WorkloadDependency {
	t.Helper()
	wd := &depsv1alpha1.WorkloadDependency{}
	must(t, k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: e2eNamespace}, wd))
	return wd
}

func waitWDPhase(t *testing.T, name string, phase depsv1alpha1.DependencyPhase, timeout time.Duration) {
	t.Helper()
	t.Logf("waiting for WD %s phase=%s", name, phase)
	if err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		wd := &depsv1alpha1.WorkloadDependency{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: e2eNamespace}, wd); err != nil {
			return false, nil
		}
		return wd.Status.Phase == phase, nil
	}); err != nil {
		wd := getWD(t, name)
		t.Errorf("timeout waiting for WD %s phase=%s, current=%s: %s", name, phase, wd.Status.Phase, wd.Status.Message)
	}
}

func waitDeploymentReady(t *testing.T, name string, replicas int32, timeout time.Duration) {
	t.Helper()
	if err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		d := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: e2eNamespace}, d); err != nil {
			return false, nil
		}
		return d.Status.ReadyReplicas == replicas, nil
	}); err != nil {
		t.Logf("timeout waiting for deployment %s ready=%d", name, replicas)
	}
}

func waitDeploymentReplicas(t *testing.T, name string, replicas int32, timeout time.Duration) {
	t.Helper()
	t.Logf("waiting for deployment %s replicas=%d", name, replicas)
	if err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		d := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: e2eNamespace}, d); err != nil {
			return false, nil
		}
		if d.Spec.Replicas == nil {
			return replicas == 1, nil
		}
		return *d.Spec.Replicas == replicas, nil
	}); err != nil {
		d := &appsv1.Deployment{}
		_ = k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: e2eNamespace}, d)
		current := int32(0)
		if d.Spec.Replicas != nil {
			current = *d.Spec.Replicas
		}
		t.Errorf("timeout waiting for deployment %s replicas=%d, current=%d", name, replicas, current)
	}
}

func kubectl(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("kubectl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("kubectl %s: %s", strings.Join(args, " "), out)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
