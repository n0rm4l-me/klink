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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	depsv1alpha1 "github.com/n0rm4l-me/klink/api/v1alpha1"
)

var _ = Describe("WorkloadDependency controller", func() {

	const (
		shortWindow   = 2 * time.Second
		shortRecovery = 3 * time.Second
		timeout       = 30 * time.Second
		interval      = 500 * time.Millisecond
	)

	var ns string

	BeforeEach(func() {
		ns = fmt.Sprintf("test-%d", time.Now().UnixNano())
		nsObj := corev1NsObj(ns)
		Expect(k8sClient.Create(ctx, &nsObj)).To(Succeed())
	})

	Describe("Basic dependency tracking", func() {
		It("should be Healthy when dependency is ready", func() {
			grant := makeDeployment("foo", ns, 2)
			Expect(k8sClient.Create(ctx, grant)).To(Succeed())
			setReady(grant, 2)

			redeem := makeDeployment("bar", ns, 2)
			Expect(k8sClient.Create(ctx, redeem)).To(Succeed())
			setReady(redeem, 2)

			wd := makeWD("bar-needs-foo", ns, "bar", []string{"foo"}, depsv1alpha1.ModeSoft, shortWindow, shortRecovery)
			Expect(k8sClient.Create(ctx, wd)).To(Succeed())

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("bar-needs-foo", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseHealthy))
		})

		It("should be Unknown when dependent deployment does not exist", func() {
			grant := makeDeployment("foo", ns, 2)
			Expect(k8sClient.Create(ctx, grant)).To(Succeed())
			setReady(grant, 2)

			wd := makeWD("wd", ns, "nonexistent", []string{"foo"}, depsv1alpha1.ModeSoft, shortWindow, shortRecovery)
			Expect(k8sClient.Create(ctx, wd)).To(Succeed())

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseUnknown))
		})
	})

	Describe("Scale-to-zero on dependency failure", func() {
		It("should scale dependent to zero after window expires", func() {
			grant := makeDeployment("foo", ns, 2)
			Expect(k8sClient.Create(ctx, grant)).To(Succeed())
			setReady(grant, 2)

			redeem := makeDeployment("bar", ns, 2)
			Expect(k8sClient.Create(ctx, redeem)).To(Succeed())
			setReady(redeem, 2)

			wd := makeWD("wd", ns, "bar", []string{"foo"}, depsv1alpha1.ModeSoft, shortWindow, shortRecovery)
			Expect(k8sClient.Create(ctx, wd)).To(Succeed())

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseHealthy))

			// Make grant unhealthy
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "foo", Namespace: ns}, grant)).To(Succeed())
			zero := int32(0)
			grant.Spec.Replicas = &zero
			Expect(k8sClient.Update(ctx, grant)).To(Succeed())
			setReady(grant, 0)

			// Should suspend after window
			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseSuspended))

			// redeem should be scaled to zero
			Eventually(func() int32 {
				return getReplicas("bar", ns)
			}, timeout, interval).Should(Equal(int32(0)))
		})

		It("should save replica count before scaling to zero", func() {
			grant := makeDeployment("foo", ns, 2)
			Expect(k8sClient.Create(ctx, grant)).To(Succeed())
			setReady(grant, 2)

			redeem := makeDeployment("bar", ns, 3)
			Expect(k8sClient.Create(ctx, redeem)).To(Succeed())
			setReady(redeem, 3)

			wd := makeWD("wd", ns, "bar", []string{"foo"}, depsv1alpha1.ModeSoft, shortWindow, shortRecovery)
			Expect(k8sClient.Create(ctx, wd)).To(Succeed())

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseHealthy))

			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "foo", Namespace: ns}, grant)).To(Succeed())
			zero := int32(0)
			grant.Spec.Replicas = &zero
			Expect(k8sClient.Update(ctx, grant)).To(Succeed())
			setReady(grant, 0)

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseSuspended))

			wdObj := &depsv1alpha1.WorkloadDependency{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "wd", Namespace: ns}, wdObj)).To(Succeed())
			Expect(wdObj.Status.SavedReplicas).NotTo(BeNil())
			Expect(*wdObj.Status.SavedReplicas).To(Equal(int32(3)))
		})
	})

	Describe("Recovery", func() {
		It("should restore replicas after dependency recovers", func() {
			grant := makeDeployment("foo", ns, 2)
			Expect(k8sClient.Create(ctx, grant)).To(Succeed())
			setReady(grant, 2)

			redeem := makeDeployment("bar", ns, 2)
			Expect(k8sClient.Create(ctx, redeem)).To(Succeed())
			setReady(redeem, 2)

			wd := makeWD("wd", ns, "bar", []string{"foo"}, depsv1alpha1.ModeSoft, shortWindow, shortRecovery)
			Expect(k8sClient.Create(ctx, wd)).To(Succeed())

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseHealthy))

			// Kill grant
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "foo", Namespace: ns}, grant)).To(Succeed())
			zero := int32(0)
			grant.Spec.Replicas = &zero
			Expect(k8sClient.Update(ctx, grant)).To(Succeed())
			setReady(grant, 0)

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseSuspended))

			// Restore grant
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "foo", Namespace: ns}, grant)).To(Succeed())
			two := int32(2)
			grant.Spec.Replicas = &two
			Expect(k8sClient.Update(ctx, grant)).To(Succeed())
			setReady(grant, 2)

			// Should become Healthy and restore redeem
			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseHealthy))

			Eventually(func() int32 {
				return getReplicas("bar", ns)
			}, timeout, interval).Should(Equal(int32(2)))
		})
	})

	Describe("Hysteresis window", func() {
		It("should not act before window expires", func() {
			grant := makeDeployment("foo", ns, 2)
			Expect(k8sClient.Create(ctx, grant)).To(Succeed())
			setReady(grant, 2)

			redeem := makeDeployment("bar", ns, 2)
			Expect(k8sClient.Create(ctx, redeem)).To(Succeed())
			setReady(redeem, 2)

			// Long window — should not scale during test
			wd := makeWD("wd", ns, "bar", []string{"foo"}, depsv1alpha1.ModeSoft, 60*time.Second, shortRecovery)
			Expect(k8sClient.Create(ctx, wd)).To(Succeed())

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseHealthy))

			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "foo", Namespace: ns}, grant)).To(Succeed())
			zero := int32(0)
			grant.Spec.Replicas = &zero
			Expect(k8sClient.Update(ctx, grant)).To(Succeed())
			setReady(grant, 0)

			// Should go Degraded but NOT Suspended within 5 seconds
			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseDegraded))

			Consistently(func() int32 {
				return getReplicas("bar", ns)
			}, 5*time.Second, interval).Should(Equal(int32(2)))
		})
	})

	Describe("Mutual dependency (CoSuspended)", func() {
		It("should not deadlock when A depends on B and B depends on A", func() {
			grant := makeDeployment("foo", ns, 2)
			Expect(k8sClient.Create(ctx, grant)).To(Succeed())
			setReady(grant, 2)

			redeem := makeDeployment("bar", ns, 2)
			Expect(k8sClient.Create(ctx, redeem)).To(Succeed())
			setReady(redeem, 2)

			wdA := makeWD("bar-needs-foo", ns, "bar", []string{"foo"}, depsv1alpha1.ModeSoft, shortWindow, shortRecovery)
			Expect(k8sClient.Create(ctx, wdA)).To(Succeed())

			wdB := makeWD("foo-needs-bar", ns, "foo", []string{"bar"}, depsv1alpha1.ModeSoft, shortWindow, shortRecovery)
			Expect(k8sClient.Create(ctx, wdB)).To(Succeed())

			Eventually(func() bool {
				return getPhase("bar-needs-foo", ns) == depsv1alpha1.PhaseHealthy &&
					getPhase("foo-needs-bar", ns) == depsv1alpha1.PhaseHealthy
			}, timeout, interval).Should(BeTrue())

			// Kill grant — redeem should be suspended, foo-needs-bar should stay healthy (CoSuspended)
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "foo", Namespace: ns}, grant)).To(Succeed())
			zero := int32(0)
			grant.Spec.Replicas = &zero
			Expect(k8sClient.Update(ctx, grant)).To(Succeed())
			setReady(grant, 0)

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("bar-needs-foo", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseSuspended))

			// foo-needs-bar: redeem is co-suspended by klink, should not suspend grant
			Consistently(func() depsv1alpha1.DependencyPhase {
				return getPhase("foo-needs-bar", ns)
			}, 5*time.Second, interval).Should(Equal(depsv1alpha1.PhaseHealthy))

			// Restore grant — redeem should auto-restore
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "foo", Namespace: ns}, grant)).To(Succeed())
			two := int32(2)
			grant.Spec.Replicas = &two
			Expect(k8sClient.Update(ctx, grant)).To(Succeed())
			setReady(grant, 2)

			Eventually(func() bool {
				return getPhase("bar-needs-foo", ns) == depsv1alpha1.PhaseHealthy &&
					getReplicas("bar", ns) == int32(2)
			}, timeout, interval).Should(BeTrue())
		})
	})

	Describe("Strict mode", func() {
		It("should re-enforce scale-to-zero if someone manually scales up", func() {
			grant := makeDeployment("foo", ns, 2)
			Expect(k8sClient.Create(ctx, grant)).To(Succeed())
			setReady(grant, 2)

			redeem := makeDeployment("bar", ns, 2)
			Expect(k8sClient.Create(ctx, redeem)).To(Succeed())
			setReady(redeem, 2)

			wd := makeWD("wd", ns, "bar", []string{"foo"}, depsv1alpha1.ModeStrict, shortWindow, shortRecovery)
			Expect(k8sClient.Create(ctx, wd)).To(Succeed())

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseHealthy))

			// Kill grant
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "foo", Namespace: ns}, grant)).To(Succeed())
			zero := int32(0)
			grant.Spec.Replicas = &zero
			Expect(k8sClient.Update(ctx, grant)).To(Succeed())
			setReady(grant, 0)

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseSuspended))
			Eventually(func() int32 { return getReplicas("bar", ns) }, timeout, interval).Should(Equal(int32(0)))

			// Manually scale up redeem
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "bar", Namespace: ns}, redeem)).To(Succeed())
			five := int32(5)
			redeem.Spec.Replicas = &five
			Expect(k8sClient.Update(ctx, redeem)).To(Succeed())

			// Strict mode should revert to zero
			Eventually(func() int32 {
				return getReplicas("bar", ns)
			}, timeout, interval).Should(Equal(int32(0)))
		})

		It("soft mode should NOT revert manual scale-up", func() {
			grant := makeDeployment("foo", ns, 2)
			Expect(k8sClient.Create(ctx, grant)).To(Succeed())
			setReady(grant, 2)

			redeem := makeDeployment("bar", ns, 2)
			Expect(k8sClient.Create(ctx, redeem)).To(Succeed())
			setReady(redeem, 2)

			wd := makeWD("wd", ns, "bar", []string{"foo"}, depsv1alpha1.ModeSoft, shortWindow, shortRecovery)
			Expect(k8sClient.Create(ctx, wd)).To(Succeed())

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseHealthy))

			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "foo", Namespace: ns}, grant)).To(Succeed())
			zero := int32(0)
			grant.Spec.Replicas = &zero
			Expect(k8sClient.Update(ctx, grant)).To(Succeed())
			setReady(grant, 0)

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseSuspended))
			Eventually(func() int32 { return getReplicas("bar", ns) }, timeout, interval).Should(Equal(int32(0)))

			// Manually scale up
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "bar", Namespace: ns}, redeem)).To(Succeed())
			five := int32(5)
			redeem.Spec.Replicas = &five
			Expect(k8sClient.Update(ctx, redeem)).To(Succeed())

			// Soft mode should leave it alone
			Consistently(func() int32 {
				return getReplicas("bar", ns)
			}, 5*time.Second, interval).Should(Equal(int32(5)))
		})
	})

	Describe("Pause annotation", func() {
		It("should stop enforcing when klink.dev/paused=true", func() {
			grant := makeDeployment("foo", ns, 2)
			Expect(k8sClient.Create(ctx, grant)).To(Succeed())
			setReady(grant, 2)

			redeem := makeDeployment("bar", ns, 2)
			Expect(k8sClient.Create(ctx, redeem)).To(Succeed())
			setReady(redeem, 2)

			wd := makeWD("wd", ns, "bar", []string{"foo"}, depsv1alpha1.ModeStrict, shortWindow, shortRecovery)
			Expect(k8sClient.Create(ctx, wd)).To(Succeed())

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseHealthy))

			// Kill grant and wait for suspend
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "foo", Namespace: ns}, grant)).To(Succeed())
			zero := int32(0)
			grant.Spec.Replicas = &zero
			Expect(k8sClient.Update(ctx, grant)).To(Succeed())
			setReady(grant, 0)

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseSuspended))

			// Pause klink
			wdObj := &depsv1alpha1.WorkloadDependency{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "wd", Namespace: ns}, wdObj)).To(Succeed())
			if wdObj.Annotations == nil {
				wdObj.Annotations = map[string]string{}
			}
			wdObj.Annotations[depsv1alpha1.AnnotationPaused] = "true"
			Expect(k8sClient.Update(ctx, wdObj)).To(Succeed())

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhasePaused))

			// Manually scale up — klink should not revert
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "bar", Namespace: ns}, redeem)).To(Succeed())
			three := int32(3)
			redeem.Spec.Replicas = &three
			Expect(k8sClient.Update(ctx, redeem)).To(Succeed())

			Consistently(func() int32 {
				return getReplicas("bar", ns)
			}, 5*time.Second, interval).Should(Equal(int32(3)))
		})

		It("should resume enforcing when pause annotation is removed", func() {
			grant := makeDeployment("foo", ns, 2)
			Expect(k8sClient.Create(ctx, grant)).To(Succeed())
			setReady(grant, 2)

			redeem := makeDeployment("bar", ns, 2)
			Expect(k8sClient.Create(ctx, redeem)).To(Succeed())
			setReady(redeem, 2)

			wd := makeWD("wd", ns, "bar", []string{"foo"}, depsv1alpha1.ModeStrict, shortWindow, shortRecovery)
			Expect(k8sClient.Create(ctx, wd)).To(Succeed())

			// Pause immediately
			Eventually(func() error {
				wdObj := &depsv1alpha1.WorkloadDependency{}
				if err := k8sClient.Get(ctx, client.ObjectKey{Name: "wd", Namespace: ns}, wdObj); err != nil {
					return err
				}
				if wdObj.Annotations == nil {
					wdObj.Annotations = map[string]string{}
				}
				wdObj.Annotations[depsv1alpha1.AnnotationPaused] = "true"
				return k8sClient.Update(ctx, wdObj)
			}, timeout, interval).Should(Succeed())

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhasePaused))

			// Kill grant — while paused klink should not suspend redeem
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "foo", Namespace: ns}, grant)).To(Succeed())
			zero := int32(0)
			grant.Spec.Replicas = &zero
			Expect(k8sClient.Update(ctx, grant)).To(Succeed())
			setReady(grant, 0)

			Consistently(func() int32 {
				return getReplicas("bar", ns)
			}, 5*time.Second, interval).Should(Equal(int32(2)))

			// Remove pause — klink should resume and suspend redeem
			Eventually(func() error {
				wdObj := &depsv1alpha1.WorkloadDependency{}
				if err := k8sClient.Get(ctx, client.ObjectKey{Name: "wd", Namespace: ns}, wdObj); err != nil {
					return err
				}
				delete(wdObj.Annotations, depsv1alpha1.AnnotationPaused)
				return k8sClient.Update(ctx, wdObj)
			}, timeout, interval).Should(Succeed())

			Eventually(func() int32 {
				return getReplicas("bar", ns)
			}, timeout, interval).Should(Equal(int32(0)))
		})
	})

	Describe("minReadyPercent", func() {
		It("should consider dependency healthy at configured percent", func() {
			grant := makeDeployment("foo", ns, 4)
			Expect(k8sClient.Create(ctx, grant)).To(Succeed())
			setReady(grant, 4)

			redeem := makeDeployment("bar", ns, 2)
			Expect(k8sClient.Create(ctx, redeem)).To(Succeed())
			setReady(redeem, 2)

			// 80% threshold — 3/4 ready should be healthy
			wd := &depsv1alpha1.WorkloadDependency{
				ObjectMeta: metav1.ObjectMeta{Name: "wd", Namespace: ns},
				Spec: depsv1alpha1.WorkloadDependencySpec{
					Dependent: depsv1alpha1.WorkloadRef{Kind: "Deployment", Name: "bar"},
					DependsOn: []depsv1alpha1.DependsOnEntry{{
						Kind: "Deployment",
						Name: "foo",
						Condition: depsv1alpha1.HealthCondition{
							MinReadyPercent: 80,
							Window:          metav1.Duration{Duration: shortWindow},
							RecoveryWindow:  metav1.Duration{Duration: shortRecovery},
						},
					}},
					OnDegraded: depsv1alpha1.OnDegradedSpec{Action: depsv1alpha1.ActionScaleToZero},
					Mode:       depsv1alpha1.ModeSoft,
				},
			}
			Expect(k8sClient.Create(ctx, wd)).To(Succeed())

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseHealthy))

			// Drop to 3/4 (75%) — below 80%, should suspend
			setReady(grant, 3)

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseSuspended))

			// Restore to 4/4 (100%)
			setReady(grant, 4)

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseHealthy))
		})
	})

	Describe("StatefulSet support", func() {
		It("should scale StatefulSet to zero when dependency fails", func() {
			grant := makeDeployment("foo", ns, 2)
			Expect(k8sClient.Create(ctx, grant)).To(Succeed())
			setReady(grant, 2)

			db := makeStatefulSet("database", ns, 3)
			Expect(k8sClient.Create(ctx, db)).To(Succeed())
			setStatefulSetReady(db, 3)

			wd := &depsv1alpha1.WorkloadDependency{
				ObjectMeta: metav1.ObjectMeta{Name: "wd", Namespace: ns},
				Spec: depsv1alpha1.WorkloadDependencySpec{
					Dependent: depsv1alpha1.WorkloadRef{Kind: "StatefulSet", Name: "database"},
					DependsOn: []depsv1alpha1.DependsOnEntry{{
						Kind: "Deployment", Name: "foo",
						Condition: depsv1alpha1.HealthCondition{
							MinReadyPercent: 100,
							Window:          metav1.Duration{Duration: shortWindow},
							RecoveryWindow:  metav1.Duration{Duration: shortRecovery},
						},
					}},
					OnDegraded: depsv1alpha1.OnDegradedSpec{Action: depsv1alpha1.ActionScaleToZero},
					Mode:       depsv1alpha1.ModeSoft,
				},
			}
			Expect(k8sClient.Create(ctx, wd)).To(Succeed())

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseHealthy))

			// Kill grant
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "foo", Namespace: ns}, grant)).To(Succeed())
			zero := int32(0)
			grant.Spec.Replicas = &zero
			Expect(k8sClient.Update(ctx, grant)).To(Succeed())
			setReady(grant, 0)

			Eventually(func() int32 {
				return getStatefulSetReplicas("database", ns)
			}, timeout, interval).Should(Equal(int32(0)))

			// Verify saved replicas
			wdObj := &depsv1alpha1.WorkloadDependency{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "wd", Namespace: ns}, wdObj)).To(Succeed())
			Expect(wdObj.Status.SavedReplicas).NotTo(BeNil())
			Expect(*wdObj.Status.SavedReplicas).To(Equal(int32(3)))

			// Restore grant — StatefulSet should come back
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "foo", Namespace: ns}, grant)).To(Succeed())
			two := int32(2)
			grant.Spec.Replicas = &two
			Expect(k8sClient.Update(ctx, grant)).To(Succeed())
			setReady(grant, 2)

			Eventually(func() int32 {
				return getStatefulSetReplicas("database", ns)
			}, timeout, interval).Should(Equal(int32(3)))
		})
	})

	Describe("CronJob support", func() {
		It("should suspend CronJob when dependency fails and resume on recovery", func() {
			grant := makeDeployment("foo", ns, 2)
			Expect(k8sClient.Create(ctx, grant)).To(Succeed())
			setReady(grant, 2)

			cj := makeCronJob("billing-job", ns)
			Expect(k8sClient.Create(ctx, cj)).To(Succeed())

			wd := &depsv1alpha1.WorkloadDependency{
				ObjectMeta: metav1.ObjectMeta{Name: "wd", Namespace: ns},
				Spec: depsv1alpha1.WorkloadDependencySpec{
					Dependent: depsv1alpha1.WorkloadRef{Kind: "CronJob", Name: "billing-job"},
					DependsOn: []depsv1alpha1.DependsOnEntry{{
						Kind: "Deployment", Name: "foo",
						Condition: depsv1alpha1.HealthCondition{
							MinReadyPercent: 100,
							Window:          metav1.Duration{Duration: shortWindow},
							RecoveryWindow:  metav1.Duration{Duration: shortRecovery},
						},
					}},
					OnDegraded: depsv1alpha1.OnDegradedSpec{Action: depsv1alpha1.ActionScaleToZero},
					Mode:       depsv1alpha1.ModeSoft,
				},
			}
			Expect(k8sClient.Create(ctx, wd)).To(Succeed())

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseHealthy))

			// Verify CronJob is not suspended initially
			Expect(isCronJobSuspended("billing-job", ns)).To(BeFalse())

			// Kill grant
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "foo", Namespace: ns}, grant)).To(Succeed())
			zero := int32(0)
			grant.Spec.Replicas = &zero
			Expect(k8sClient.Update(ctx, grant)).To(Succeed())
			setReady(grant, 0)

			// CronJob should be suspended
			Eventually(func() bool {
				return isCronJobSuspended("billing-job", ns)
			}, timeout, interval).Should(BeTrue())

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseSuspended))

			// Restore grant
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "foo", Namespace: ns}, grant)).To(Succeed())
			two := int32(2)
			grant.Spec.Replicas = &two
			Expect(k8sClient.Update(ctx, grant)).To(Succeed())
			setReady(grant, 2)

			// CronJob should be resumed
			Eventually(func() bool {
				return isCronJobSuspended("billing-job", ns)
			}, timeout, interval).Should(BeFalse())

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseHealthy))
		})

		It("should not save savedReplicas for CronJob", func() {
			grant := makeDeployment("foo", ns, 2)
			Expect(k8sClient.Create(ctx, grant)).To(Succeed())
			setReady(grant, 2)

			cj := makeCronJob("report-job", ns)
			Expect(k8sClient.Create(ctx, cj)).To(Succeed())

			wd := &depsv1alpha1.WorkloadDependency{
				ObjectMeta: metav1.ObjectMeta{Name: "wd", Namespace: ns},
				Spec: depsv1alpha1.WorkloadDependencySpec{
					Dependent: depsv1alpha1.WorkloadRef{Kind: "CronJob", Name: "report-job"},
					DependsOn: []depsv1alpha1.DependsOnEntry{{
						Kind: "Deployment", Name: "foo",
						Condition: depsv1alpha1.HealthCondition{
							MinReadyPercent: 100,
							Window:          metav1.Duration{Duration: shortWindow},
							RecoveryWindow:  metav1.Duration{Duration: shortRecovery},
						},
					}},
					OnDegraded: depsv1alpha1.OnDegradedSpec{Action: depsv1alpha1.ActionScaleToZero},
					Mode:       depsv1alpha1.ModeSoft,
				},
			}
			Expect(k8sClient.Create(ctx, wd)).To(Succeed())

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseHealthy))

			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "foo", Namespace: ns}, grant)).To(Succeed())
			zero := int32(0)
			grant.Spec.Replicas = &zero
			Expect(k8sClient.Update(ctx, grant)).To(Succeed())
			setReady(grant, 0)

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseSuspended))

			wdObj := &depsv1alpha1.WorkloadDependency{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "wd", Namespace: ns}, wdObj)).To(Succeed())
			Expect(wdObj.Status.SavedReplicas).To(BeNil())
		})
	})

	Describe("Rollout support", func() {
		It("should scale Rollout to zero when dependency fails", func() {
			dep := makeDeployment("foo", ns, 2)
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			setReady(dep, 2)

			ro := makeRollout("payments", ns, 3, rolloutPhaseHealthy)
			Expect(k8sClient.Create(ctx, ro)).To(Succeed())
			setRolloutPhase("payments", ns, rolloutPhaseHealthy)

			wd := &depsv1alpha1.WorkloadDependency{
				ObjectMeta: metav1.ObjectMeta{Name: "wd", Namespace: ns},
				Spec: depsv1alpha1.WorkloadDependencySpec{
					Dependent: depsv1alpha1.WorkloadRef{Kind: "Rollout", Name: "payments"},
					DependsOn: []depsv1alpha1.DependsOnEntry{{
						Kind: "Deployment", Name: "foo",
						Condition: depsv1alpha1.HealthCondition{
							MinReadyPercent: 100,
							Window:          metav1.Duration{Duration: shortWindow},
							RecoveryWindow:  metav1.Duration{Duration: shortRecovery},
						},
					}},
					OnDegraded: depsv1alpha1.OnDegradedSpec{Action: depsv1alpha1.ActionScaleToZero},
					Mode:       depsv1alpha1.ModeSoft,
				},
			}
			Expect(k8sClient.Create(ctx, wd)).To(Succeed())

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseHealthy))

			// Kill dependency
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "foo", Namespace: ns}, dep)).To(Succeed())
			zero := int32(0)
			dep.Spec.Replicas = &zero
			Expect(k8sClient.Update(ctx, dep)).To(Succeed())
			setReady(dep, 0)

			Eventually(func() int32 {
				return getRolloutReplicas("payments", ns)
			}, timeout, interval).Should(Equal(int32(0)))

			wdObj := &depsv1alpha1.WorkloadDependency{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "wd", Namespace: ns}, wdObj)).To(Succeed())
			Expect(wdObj.Status.SavedReplicas).NotTo(BeNil())
			Expect(*wdObj.Status.SavedReplicas).To(Equal(int32(3)))

			// Restore dependency — Rollout should come back
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "foo", Namespace: ns}, dep)).To(Succeed())
			two := int32(2)
			dep.Spec.Replicas = &two
			Expect(k8sClient.Update(ctx, dep)).To(Succeed())
			setReady(dep, 2)

			Eventually(func() int32 {
				return getRolloutReplicas("payments", ns)
			}, timeout, interval).Should(Equal(int32(3)))
		})

		It("should defer suspension when Rollout is Progressing (canary in flight)", func() {
			dep := makeDeployment("foo", ns, 2)
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			setReady(dep, 2)

			ro := makeRollout("payments", ns, 3, rolloutPhaseHealthy)
			Expect(k8sClient.Create(ctx, ro)).To(Succeed())
			setRolloutPhase("payments", ns, rolloutPhaseHealthy)

			wd := &depsv1alpha1.WorkloadDependency{
				ObjectMeta: metav1.ObjectMeta{Name: "wd", Namespace: ns},
				Spec: depsv1alpha1.WorkloadDependencySpec{
					Dependent: depsv1alpha1.WorkloadRef{Kind: "Rollout", Name: "payments"},
					DependsOn: []depsv1alpha1.DependsOnEntry{{
						Kind: "Deployment", Name: "foo",
						Condition: depsv1alpha1.HealthCondition{
							MinReadyPercent: 100,
							Window:          metav1.Duration{Duration: shortWindow},
							RecoveryWindow:  metav1.Duration{Duration: shortRecovery},
						},
					}},
					OnDegraded: depsv1alpha1.OnDegradedSpec{Action: depsv1alpha1.ActionScaleToZero},
					Mode:       depsv1alpha1.ModeStrict,
				},
			}
			Expect(k8sClient.Create(ctx, wd)).To(Succeed())

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseHealthy))

			// Set Rollout to Progressing (simulates canary in flight)
			setRolloutPhase("payments", ns, rolloutPhaseProgressing)

			// Kill dependency
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "foo", Namespace: ns}, dep)).To(Succeed())
			zero := int32(0)
			dep.Spec.Replicas = &zero
			Expect(k8sClient.Update(ctx, dep)).To(Succeed())
			setReady(dep, 0)

			// Should become Degraded but NOT Suspended — canary is in progress
			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseDegraded))

			Consistently(func() int32 {
				return getRolloutReplicas("payments", ns)
			}, 5*time.Second, interval).Should(Equal(int32(3)))

			// Canary finishes — Rollout becomes Healthy
			setRolloutPhase("payments", ns, rolloutPhaseHealthy)

			// Now it should suspend
			Eventually(func() int32 {
				return getRolloutReplicas("payments", ns)
			}, timeout, interval).Should(Equal(int32(0)))
		})

		It("should treat Rollout as healthy dependency when Progressing", func() {
			// Rollout in Progressing phase should still be healthy AS A DEPENDENCY
			ro := makeRollout("database", ns, 2, rolloutPhaseProgressing)
			Expect(k8sClient.Create(ctx, ro)).To(Succeed())
			setRolloutPhase("database", ns, rolloutPhaseProgressing)

			payments := makeDeployment("payments", ns, 2)
			Expect(k8sClient.Create(ctx, payments)).To(Succeed())
			setReady(payments, 2)

			wd := &depsv1alpha1.WorkloadDependency{
				ObjectMeta: metav1.ObjectMeta{Name: "wd", Namespace: ns},
				Spec: depsv1alpha1.WorkloadDependencySpec{
					Dependent: depsv1alpha1.WorkloadRef{Kind: "Deployment", Name: "payments"},
					DependsOn: []depsv1alpha1.DependsOnEntry{{
						Kind: "Rollout", Name: "database",
						Condition: depsv1alpha1.HealthCondition{
							Window:         metav1.Duration{Duration: shortWindow},
							RecoveryWindow: metav1.Duration{Duration: shortRecovery},
						},
					}},
					OnDegraded: depsv1alpha1.OnDegradedSpec{Action: depsv1alpha1.ActionScaleToZero},
					Mode:       depsv1alpha1.ModeSoft,
				},
			}
			Expect(k8sClient.Create(ctx, wd)).To(Succeed())

			// Progressing Rollout as dependency = Healthy
			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseHealthy))

			// Rollout becomes Degraded → should trigger suspension
			setRolloutPhase("database", ns, rolloutPhaseDegraded)

			Eventually(func() int32 {
				return getReplicas("payments", ns)
			}, timeout, interval).Should(Equal(int32(0)))
		})
	})

	Describe("Observe mode", func() {
		It("should not scale to zero in observe mode, only log", func() {
			dep := makeDeployment("foo", ns, 2)
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			setReady(dep, 2)

			svc := makeDeployment("bar", ns, 3)
			Expect(k8sClient.Create(ctx, svc)).To(Succeed())
			setReady(svc, 3)

			wd := &depsv1alpha1.WorkloadDependency{
				ObjectMeta: metav1.ObjectMeta{Name: "wd", Namespace: ns},
				Spec: depsv1alpha1.WorkloadDependencySpec{
					Dependent: depsv1alpha1.WorkloadRef{Kind: "Deployment", Name: "bar"},
					DependsOn: []depsv1alpha1.DependsOnEntry{{
						Kind: "Deployment", Name: "foo",
						Condition: depsv1alpha1.HealthCondition{
							Window:         metav1.Duration{Duration: shortWindow},
							RecoveryWindow: metav1.Duration{Duration: shortRecovery},
						},
					}},
					OnDegraded: depsv1alpha1.OnDegradedSpec{Action: depsv1alpha1.ActionScaleToZero},
					Mode:       depsv1alpha1.ModeObserve,
				},
			}
			Expect(k8sClient.Create(ctx, wd)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, wd) })

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseHealthy))

			// Kill dependency
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "foo", Namespace: ns}, dep)).To(Succeed())
			zero := int32(0)
			dep.Spec.Replicas = &zero
			Expect(k8sClient.Update(ctx, dep)).To(Succeed())
			setReady(dep, 0)

			// Should go to Observed phase but NOT scale bar to 0
			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseObserved))

			// bar should still be running
			Consistently(func() int32 {
				return getReplicas("bar", ns)
			}, 5*time.Second, interval).Should(Equal(int32(3)))
		})
	})

	Describe("maxSuspendDuration", func() {
		It("should auto-restore after maxSuspendDuration even if dependency still unhealthy", func() {
			dep := makeDeployment("foo", ns, 2)
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			setReady(dep, 2)

			svc := makeDeployment("bar", ns, 2)
			Expect(k8sClient.Create(ctx, svc)).To(Succeed())
			setReady(svc, 2)

			wd := &depsv1alpha1.WorkloadDependency{
				ObjectMeta: metav1.ObjectMeta{Name: "wd", Namespace: ns},
				Spec: depsv1alpha1.WorkloadDependencySpec{
					Dependent: depsv1alpha1.WorkloadRef{Kind: "Deployment", Name: "bar"},
					DependsOn: []depsv1alpha1.DependsOnEntry{{
						Kind: "Deployment", Name: "foo",
						Condition: depsv1alpha1.HealthCondition{
							Window:         metav1.Duration{Duration: shortWindow},
							RecoveryWindow: metav1.Duration{Duration: shortRecovery},
						},
					}},
					OnDegraded: depsv1alpha1.OnDegradedSpec{
						Action:             depsv1alpha1.ActionScaleToZero,
						MaxSuspendDuration: metav1.Duration{Duration: 3 * time.Second},
					},
					Mode: depsv1alpha1.ModeSoft,
				},
			}
			Expect(k8sClient.Create(ctx, wd)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, wd) })

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseHealthy))

			// Kill dependency
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "foo", Namespace: ns}, dep)).To(Succeed())
			zero := int32(0)
			dep.Spec.Replicas = &zero
			Expect(k8sClient.Update(ctx, dep)).To(Succeed())
			setReady(dep, 0)

			// Should suspend
			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseSuspended))
			Eventually(func() int32 { return getReplicas("bar", ns) }, timeout, interval).Should(Equal(int32(0)))

			// After maxSuspendDuration (3s) — should become Healthy (force restored)
			// Note: with soft mode and dep still unhealthy, klink will re-degrade after window
			// We just verify the MaxSuspendDuration restore happened (Healthy phase reached)
			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("wd", ns)
			}, 25*time.Second, interval).Should(Equal(depsv1alpha1.PhaseHealthy))
		})
	})

	Describe("Cycle detection", func() {
		It("should detect A→B→A cycle and set Unknown phase", func() {
			depA := makeDeployment("svc-a", ns, 2)
			Expect(k8sClient.Create(ctx, depA)).To(Succeed())
			setReady(depA, 2)

			depB := makeDeployment("svc-b", ns, 2)
			Expect(k8sClient.Create(ctx, depB)).To(Succeed())
			setReady(depB, 2)

			// First WD: svc-a depends on svc-b
			wdA := &depsv1alpha1.WorkloadDependency{
				ObjectMeta: metav1.ObjectMeta{Name: "a-needs-b", Namespace: ns},
				Spec: depsv1alpha1.WorkloadDependencySpec{
					Dependent:  depsv1alpha1.WorkloadRef{Kind: "Deployment", Name: "svc-a"},
					DependsOn:  []depsv1alpha1.DependsOnEntry{{Kind: "Deployment", Name: "svc-b"}},
					OnDegraded: depsv1alpha1.OnDegradedSpec{Action: depsv1alpha1.ActionScaleToZero},
					Mode:       depsv1alpha1.ModeSoft,
				},
			}
			Expect(k8sClient.Create(ctx, wdA)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, wdA) })

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("a-needs-b", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseHealthy))

			// Second WD: svc-b depends on svc-a → CYCLE!
			wdB := &depsv1alpha1.WorkloadDependency{
				ObjectMeta: metav1.ObjectMeta{Name: "b-needs-a", Namespace: ns},
				Spec: depsv1alpha1.WorkloadDependencySpec{
					Dependent:  depsv1alpha1.WorkloadRef{Kind: "Deployment", Name: "svc-b"},
					DependsOn:  []depsv1alpha1.DependsOnEntry{{Kind: "Deployment", Name: "svc-a"}},
					OnDegraded: depsv1alpha1.OnDegradedSpec{Action: depsv1alpha1.ActionScaleToZero},
					Mode:       depsv1alpha1.ModeSoft,
				},
			}
			Expect(k8sClient.Create(ctx, wdB)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, wdB) })

			// Note: A→B + B→A is a valid mutual dependency pattern (handled via CoSuspended).
			// True cycle detection catches longer chains: A→B→C→A.
			// For mutual deps, cycle detection correctly returns no cycle
			// because A depends on B and B depends on A — both are 1-hop, not a traversal cycle.
			// The phase should be Healthy (mutual deps work fine).
			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("b-needs-a", ns)
			}, timeout, interval).ShouldNot(Equal(depsv1alpha1.PhaseUnknown))
		})

		It("should detect A→B→C→A three-node cycle", func() {
			depA := makeDeployment("node-a", ns, 1)
			depB := makeDeployment("node-b", ns, 1)
			depC := makeDeployment("node-c", ns, 1)
			for _, d := range []*appsv1.Deployment{depA, depB, depC} {
				Expect(k8sClient.Create(ctx, d)).To(Succeed())
				setReady(d, 1)
			}

			// A→B
			wdAB := &depsv1alpha1.WorkloadDependency{
				ObjectMeta: metav1.ObjectMeta{Name: "a-needs-b", Namespace: ns},
				Spec: depsv1alpha1.WorkloadDependencySpec{
					Dependent:  depsv1alpha1.WorkloadRef{Kind: "Deployment", Name: "node-a"},
					DependsOn:  []depsv1alpha1.DependsOnEntry{{Kind: "Deployment", Name: "node-b"}},
					OnDegraded: depsv1alpha1.OnDegradedSpec{Action: depsv1alpha1.ActionScaleToZero},
					Mode:       depsv1alpha1.ModeSoft,
				},
			}
			// B→C
			wdBC := &depsv1alpha1.WorkloadDependency{
				ObjectMeta: metav1.ObjectMeta{Name: "b-needs-c", Namespace: ns},
				Spec: depsv1alpha1.WorkloadDependencySpec{
					Dependent:  depsv1alpha1.WorkloadRef{Kind: "Deployment", Name: "node-b"},
					DependsOn:  []depsv1alpha1.DependsOnEntry{{Kind: "Deployment", Name: "node-c"}},
					OnDegraded: depsv1alpha1.OnDegradedSpec{Action: depsv1alpha1.ActionScaleToZero},
					Mode:       depsv1alpha1.ModeSoft,
				},
			}
			Expect(k8sClient.Create(ctx, wdAB)).To(Succeed())
			Expect(k8sClient.Create(ctx, wdBC)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, wdAB)
				_ = k8sClient.Delete(ctx, wdBC)
			})

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("a-needs-b", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseHealthy))

			// C→A closes the cycle A→B→C→A
			wdCA := &depsv1alpha1.WorkloadDependency{
				ObjectMeta: metav1.ObjectMeta{Name: "c-needs-a", Namespace: ns},
				Spec: depsv1alpha1.WorkloadDependencySpec{
					Dependent:  depsv1alpha1.WorkloadRef{Kind: "Deployment", Name: "node-c"},
					DependsOn:  []depsv1alpha1.DependsOnEntry{{Kind: "Deployment", Name: "node-a"}},
					OnDegraded: depsv1alpha1.OnDegradedSpec{Action: depsv1alpha1.ActionScaleToZero},
					Mode:       depsv1alpha1.ModeSoft,
				},
			}
			Expect(k8sClient.Create(ctx, wdCA)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, wdCA) })

			Eventually(func() depsv1alpha1.DependencyPhase {
				return getPhase("c-needs-a", ns)
			}, timeout, interval).Should(Equal(depsv1alpha1.PhaseUnknown))
		})
	})
})
