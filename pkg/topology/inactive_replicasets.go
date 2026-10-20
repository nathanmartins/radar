package topology

import (
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/skyhook-io/radar/pkg/rollouts"
)

// deploymentRevisionAnnotation is where a Deployment, and each of its
// ReplicaSets, records a revision.
const deploymentRevisionAnnotation = "deployment.kubernetes.io/revision"

// SplitInactiveReplicaSets moves the ReplicaSets among rel's Children that only
// keep history into InactiveChildren: those that run no Pods, are meant to run
// none, and aren't owner's current revision. A Deployment keeps up to ten old
// revisions for rollback, which would bury the ReplicaSet that runs its Pods.
// owner is the object rel describes; nothing moves unless it records its
// current revision, as a Deployment and an Argo Rollout do. replicaSet reads a
// child from the caches, and a child it can't read stays among Children.
func SplitInactiveReplicaSets(rel *Relationships, owner metav1.Object, replicaSet func(namespace, name string) (*appsv1.ReplicaSet, bool)) {
	if rel == nil || len(rel.Children) == 0 || replicaSet == nil {
		return
	}
	annotation, current := currentRevision(owner)
	if current == "" {
		return
	}
	var children []ResourceRef
	for _, c := range rel.Children {
		if c.Group == "apps" && c.Kind == "ReplicaSet" {
			if rs, ok := replicaSet(c.Namespace, c.Name); ok && inactiveReplicaSet(rs, annotation, current) {
				rel.InactiveChildren = append(rel.InactiveChildren, c)
				continue
			}
		}
		children = append(children, c)
	}
	rel.Children = children
}

// currentRevision is the revision owner records as current, if it is a
// Deployment or an Argo Rollout, and the annotation its ReplicaSets record
// theirs under.
func currentRevision(owner metav1.Object) (annotation, revision string) {
	if owner == nil {
		return "", ""
	}
	for _, a := range []string{deploymentRevisionAnnotation, rollouts.RevisionAnnotation} {
		if r := owner.GetAnnotations()[a]; r != "" {
			return a, r
		}
	}
	return "", ""
}

// inactiveReplicaSet reports whether rs runs no Pods, is meant to run none, and
// isn't its owner's current revision.
func inactiveReplicaSet(rs *appsv1.ReplicaSet, annotation, current string) bool {
	if rs.Spec.Replicas == nil || *rs.Spec.Replicas != 0 || rs.Status.Replicas != 0 {
		return false
	}
	return rs.Annotations[annotation] != current
}
