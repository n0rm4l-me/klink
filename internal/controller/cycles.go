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
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	depsv1alpha1 "github.com/n0rm4l-me/klink/api/v1alpha1"
)

// workloadKey uniquely identifies a workload: "kind/namespace/name"
type workloadKey struct {
	kind, namespace, name string
}

func (k workloadKey) String() string {
	return fmt.Sprintf("%s/%s/%s", k.kind, k.namespace, k.name)
}

// DetectCycle checks if adding the given WorkloadDependency would create a cycle
// in the dependency graph. Returns the cycle path if found, nil otherwise.
//
// A cycle means: dependent A depends on B, B depends on C, C depends on A —
// i.e. the *dependent* of one WD appears as the *dependent* of another WD that
// is reachable through the dependsOn chain.
//
// Mutual dependencies (A↔B) are NOT cycles — they are explicitly supported via CoSuspended.
// A cycle requires a chain of length ≥ 3: A→B→C→A.
func DetectCycle(ctx context.Context, c client.Client, newWD *depsv1alpha1.WorkloadDependency) ([]string, error) {
	wdList := &depsv1alpha1.WorkloadDependencyList{}
	if err := c.List(ctx, wdList, client.InNamespace(newWD.Namespace)); err != nil {
		return nil, fmt.Errorf("list WorkloadDependencies: %w", err)
	}

	// Build graph: dependent → what it dependsOn (i.e. what must be healthy for it to run)
	// Edge: "dependent needs dep" means dependent → dep
	graph := make(map[workloadKey][]workloadKey)

	for _, wd := range wdList.Items {
		if wd.Name == newWD.Name && wd.Namespace == newWD.Namespace {
			continue
		}
		depKey := resolveKey(wd.Spec.Dependent, wd.Namespace)
		for _, dep := range wd.Spec.DependsOn {
			depOnKey := resolveDependsOnKey(dep, wd.Namespace)
			graph[depKey] = append(graph[depKey], depOnKey)
		}
	}

	// Add the new WD's edges
	newDepKey := resolveKey(newWD.Spec.Dependent, newWD.Namespace)
	for _, dep := range newWD.Spec.DependsOn {
		depOnKey := resolveDependsOnKey(dep, newWD.Namespace)
		graph[newDepKey] = append(graph[newDepKey], depOnKey)
	}

	// Check: can we reach newDepKey starting from any of newDepKey's dependencies
	// via a path of length ≥ 2 (i.e. through at least one intermediate node)?
	// This distinguishes true cycles (A→B→C→A) from mutual deps (A↔B).
	for _, dep := range newWD.Spec.DependsOn {
		startKey := resolveDependsOnKey(dep, newWD.Namespace)
		// Skip direct mutual: if dep itself is newDepKey's dependent, that's mutual dep, not cycle
		if startKey == newDepKey {
			continue
		}
		visited := make(map[workloadKey]bool)
		visited[startKey] = true // don't revisit startKey itself
		for _, neighbor := range graph[startKey] {
			if neighbor == newDepKey {
				// Direct 2-hop: newDep→start→newDep is mutual dep (A↔B), not a cycle
				continue
			}
			path := []workloadKey{newDepKey, startKey, neighbor}
			if cycle := dfs(graph, neighbor, newDepKey, visited, path); cycle != nil {
				strs := make([]string, len(cycle))
				for i, k := range cycle {
					strs[i] = k.String()
				}
				return strs, nil
			}
		}
	}
	return nil, nil
}

// dfs traverses from node looking for target with minimum depth.
// depth tracks how many edges we've traversed from the original start.
// We only report a cycle if depth >= 2 (target found through ≥1 intermediate node).
func dfs(graph map[workloadKey][]workloadKey, node, target workloadKey, visited map[workloadKey]bool, path []workloadKey) []workloadKey {
	if visited[node] {
		return nil
	}
	visited[node] = true
	for _, neighbor := range graph[node] {
		newPath := append(append([]workloadKey{}, path...), neighbor)
		if neighbor == target {
			return newPath
		}
		if cycle := dfs(graph, neighbor, target, visited, newPath); cycle != nil {
			return cycle
		}
	}
	return nil
}

func resolveKey(ref depsv1alpha1.WorkloadRef, defaultNS string) workloadKey {
	ns := ref.Namespace
	if ns == "" {
		ns = defaultNS
	}
	return workloadKey{kind: ref.Kind, namespace: ns, name: ref.Name}
}

func resolveDependsOnKey(dep depsv1alpha1.DependsOnEntry, defaultNS string) workloadKey {
	ns := dep.Namespace
	if ns == "" {
		ns = defaultNS
	}
	return workloadKey{kind: dep.Kind, namespace: ns, name: dep.Name}
}

// FormatCycle returns a human-readable cycle description.
func FormatCycle(cycle []string) string {
	return strings.Join(cycle, " → ")
}
