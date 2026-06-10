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
	"path/filepath"
	"runtime"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	depsv1alpha1 "github.com/n0rm4l-me/klink/api/v1alpha1"
)

func corev1NsObj(name string) corev1.Namespace {
	return corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

var (
	k8sClient client.Client
	testEnv   *envtest.Environment
	ctx       context.Context
	cancel    context.CancelFunc
)

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.Background())

	_, filename, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(filename), "..", "..")

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(root, "testdata", "crd")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: filepath.Join(root, "bin", "k8s",
			"1.33.0-"+runtime.GOOS+"-"+runtime.GOARCH),
	}

	cfg, err := testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	Expect(depsv1alpha1.AddToScheme(scheme.Scheme)).To(Succeed())
	Expect(appsv1.AddToScheme(scheme.Scheme)).To(Succeed())
	Expect(batchv1.AddToScheme(scheme.Scheme)).To(Succeed())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())

	err = (&WorkloadDependencyReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: record.NewFakeRecorder(100),
	}).SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	go func() {
		defer GinkgoRecover()
		Expect(mgr.Start(ctx)).To(Succeed())
	}()
})

var _ = AfterSuite(func() {
	cancel()
	Expect(testEnv.Stop()).To(Succeed())
})

// helpers

func makeDeployment(name, namespace string, replicas int32) *appsv1.Deployment {
	r := replicas
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &r,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: name, Image: "nginx"}}},
			},
		},
	}
}

func setReady(dep *appsv1.Deployment, ready int32) {
	dep.Status.Replicas = *dep.Spec.Replicas
	dep.Status.ReadyReplicas = ready
	Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())
}

func makeWD(name, namespace, dependent string, deps []string, mode depsv1alpha1.EnforcementMode, window, recovery time.Duration) *depsv1alpha1.WorkloadDependency {
	var dependsOn []depsv1alpha1.DependsOnEntry
	for _, d := range deps {
		dependsOn = append(dependsOn, depsv1alpha1.DependsOnEntry{
			Kind: "Deployment",
			Name: d,
			Condition: depsv1alpha1.HealthCondition{
				MinReadyPercent: 100,
				Window:          metav1.Duration{Duration: window},
				RecoveryWindow:  metav1.Duration{Duration: recovery},
			},
		})
	}
	return &depsv1alpha1.WorkloadDependency{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: depsv1alpha1.WorkloadDependencySpec{
			Dependent:  depsv1alpha1.WorkloadRef{Kind: "Deployment", Name: dependent},
			DependsOn:  dependsOn,
			OnDegraded: depsv1alpha1.OnDegradedSpec{Action: depsv1alpha1.ActionScaleToZero},
			Mode:       mode,
		},
	}
}

func getReplicas(name, namespace string) int32 {
	dep := &appsv1.Deployment{}
	Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, dep)).To(Succeed())
	if dep.Spec.Replicas == nil {
		return 0
	}
	return *dep.Spec.Replicas
}

func getPhase(wdName, namespace string) depsv1alpha1.DependencyPhase {
	wd := &depsv1alpha1.WorkloadDependency{}
	Expect(k8sClient.Get(ctx, client.ObjectKey{Name: wdName, Namespace: namespace}, wd)).To(Succeed())
	return wd.Status.Phase
}

func makeStatefulSet(name, namespace string, replicas int32) *appsv1.StatefulSet {
	r := replicas
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &r,
			ServiceName: name,
			Selector:    &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: name, Image: "nginx"}}},
			},
		},
	}
}

func setStatefulSetReady(sts *appsv1.StatefulSet, ready int32) {
	sts.Status.Replicas = *sts.Spec.Replicas
	sts.Status.ReadyReplicas = ready
	Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())
}

func getStatefulSetReplicas(name, namespace string) int32 {
	sts := &appsv1.StatefulSet{}
	Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, sts)).To(Succeed())
	if sts.Spec.Replicas == nil {
		return 0
	}
	return *sts.Spec.Replicas
}

func makeCronJob(name, namespace string) *batchv1.CronJob {
	f := false
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: batchv1.CronJobSpec{
			Schedule: "*/5 * * * *",
			Suspend:  &f,
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyNever,
							Containers:    []corev1.Container{{Name: name, Image: "busybox"}},
						},
					},
				},
			},
		},
	}
}

func isCronJobSuspended(name, namespace string) bool {
	cj := &batchv1.CronJob{}
	Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, cj)).To(Succeed())
	return cj.Spec.Suspend != nil && *cj.Spec.Suspend
}
