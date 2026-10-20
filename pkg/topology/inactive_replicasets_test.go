package topology

import (
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/skyhook-io/radar/pkg/rollouts"
)

func replicaSetChild(name string) ResourceRef {
	return ResourceRef{Group: "apps", Kind: "ReplicaSet", Namespace: "shop", Name: name}
}

// A Deployment's old revisions, which run no Pods, are kept apart from the
// ReplicaSets that do, and from its current revision even at zero replicas.
func TestSplitInactiveReplicaSets(t *testing.T) {
	rs := func(name string, desired, running int32, annotations map[string]string) *appsv1.ReplicaSet {
		return &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name, Annotations: annotations},
			Spec:       appsv1.ReplicaSetSpec{Replicas: &desired},
			Status:     appsv1.ReplicaSetStatus{Replicas: running},
		}
	}
	revision := func(a, r string) map[string]string { return map[string]string{a: r} }
	cached := map[string]*appsv1.ReplicaSet{
		"web-live":     rs("web-live", 3, 3, revision(deploymentRevisionAnnotation, "5")),
		"web-draining": rs("web-draining", 0, 2, revision(deploymentRevisionAnnotation, "4")),
		"web-old":      rs("web-old", 0, 0, revision(deploymentRevisionAnnotation, "3")),
		"web-current":  rs("web-current", 0, 0, revision(deploymentRevisionAnnotation, "5")),
		"roll-old":     rs("roll-old", 0, 0, revision(rollouts.RevisionAnnotation, "1")),
		"roll-current": rs("roll-current", 0, 0, revision(rollouts.RevisionAnnotation, "2")),
	}
	lookup := func(namespace, name string) (*appsv1.ReplicaSet, bool) {
		r, ok := cached[name]
		return r, ok && namespace == "shop"
	}
	owner := func(annotations map[string]string) metav1.Object {
		return &metav1.ObjectMeta{Namespace: "shop", Name: "web", Annotations: annotations}
	}
	pod := ResourceRef{Kind: "Pod", Namespace: "shop", Name: "web-live-a"}
	for _, tc := range []struct {
		name             string
		owner            metav1.Object
		children         []string
		active, inactive []string
	}{
		{"a Deployment rolled out", owner(revision(deploymentRevisionAnnotation, "5")),
			[]string{"web-live", "web-draining", "web-old"}, []string{"web-live", "web-draining"}, []string{"web-old"}},
		{"a Deployment scaled to zero keeps its current revision", owner(revision(deploymentRevisionAnnotation, "5")),
			[]string{"web-current", "web-old"}, []string{"web-current"}, []string{"web-old"}},
		{"a Rollout scaled to zero keeps its current revision", owner(revision(rollouts.RevisionAnnotation, "2")),
			[]string{"roll-current", "roll-old"}, []string{"roll-current"}, []string{"roll-old"}},
		{"an owner that records no revision moves nothing", owner(nil),
			[]string{"web-current", "web-old"}, []string{"web-current", "web-old"}, nil},
		{"a ReplicaSet the caches don't hold stays", owner(revision(deploymentRevisionAnnotation, "5")),
			[]string{"web-gone", "web-old"}, []string{"web-gone"}, []string{"web-old"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rel := &Relationships{Children: []ResourceRef{pod}}
			for _, c := range tc.children {
				rel.Children = append(rel.Children, replicaSetChild(c))
			}
			SplitInactiveReplicaSets(rel, tc.owner, lookup)
			want := []ResourceRef{pod}
			for _, c := range tc.active {
				want = append(want, replicaSetChild(c))
			}
			var wantInactive []ResourceRef
			for _, c := range tc.inactive {
				wantInactive = append(wantInactive, replicaSetChild(c))
			}
			if !slices.Equal(rel.Children, want) || !slices.Equal(rel.InactiveChildren, wantInactive) {
				t.Errorf("children %v inactive %v; want %v and %v", rel.Children, rel.InactiveChildren, want, wantInactive)
			}
		})
	}
}
