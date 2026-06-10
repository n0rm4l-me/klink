package webhook

import "k8s.io/apimachinery/pkg/runtime/schema"

var rolloutGVK = schema.GroupVersionKind{
	Group:   "argoproj.io",
	Version: "v1alpha1",
	Kind:    "Rollout",
}
