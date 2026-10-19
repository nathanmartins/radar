package k8s

import (
	"errors"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/skyhook-io/radar/pkg/ownership"
	"github.com/skyhook-io/radar/pkg/ownership/cachesource"
	"github.com/skyhook-io/radar/pkg/readreason"
	"github.com/skyhook-io/radar/pkg/resourceid"
	"github.com/skyhook-io/radar/pkg/topology"
)

// ownershipSource reads ownership from the caches current when a session
// begins. A session pins them, so a context switch that replaces them fails
// its Validate instead of mixing two clusters' answers.
var ownershipSource = cachesource.New(func() cachesource.Caches {
	var c cachesource.Caches
	if rc := GetResourceCache(); rc != nil {
		c.Typed = rc.ResourceCache
	}
	if dc := GetDynamicResourceCache(); dc != nil {
		c.Dynamic = dc.DynamicResourceCache
	}
	if rd := GetResourceDiscovery(); rd != nil {
		c.Discovery = rd.ResourceDiscovery
	}
	return c
})

// OwnershipSource answers ownership questions from Radar's caches, without
// API calls.
func OwnershipSource() ownership.Source { return ownershipSource }

// AnswerOwnership answers rel's ownership for fetched, the object the caller
// fetched, from the caches, for the caller access describes once the
// session's catalog is known. A store replaced during the read (a
// cluster-wide watch taking over from namespace ones), or an object that
// changed between the view's reads, is worth one more try; caches replaced or
// not ready leave the answer unavailable, as does a retry that fails too.
func AnswerOwnership(rel *topology.Relationships, fetched ownership.Observation, access func(*ownership.Catalog) ownership.Access) {
	answerOwnership(ownershipSource, topology.ApplyOwnership, rel, fetched, access)
}

// AnswerOwner is AnswerOwnership for rel's Owner alone, which is all a
// resource context reads: it searches for no children and reads no member
// list, so it costs a few reads however much the object owns.
func AnswerOwner(rel *topology.Relationships, fetched ownership.Observation, access func(*ownership.Catalog) ownership.Access) {
	answerOwnership(ownershipSource, topology.ApplyOwner, rel, fetched, access)
}

func answerOwnership(source ownership.Source, apply func(*topology.Relationships, *ownership.View, ownership.Observation), rel *topology.Relationships, fetched ownership.Observation, access func(*ownership.Catalog) ownership.Access) {
	reason := readreason.KindSyncPending
	for range 2 {
		session, err := source.Begin()
		if err != nil {
			topology.OwnershipUnavailable(rel, string(readreason.ClusterConnecting))
			return
		}
		v := ownership.NewView(session, access(session.Catalog()))
		apply(rel, v, fetched)
		err = v.Validate()
		switch {
		case err == nil:
			return
		case errors.Is(err, ownership.ErrCachesReplaced):
			topology.OwnershipUnavailable(rel, string(readreason.ConnectionChanged))
			return
		case errors.Is(err, ownership.ErrObjectsChanged):
			// Objects changed while they were read; nothing is loading, and
			// reading again resolves it.
			reason = readreason.ReadError
		default:
			reason = readreason.KindSyncPending
		}
	}
	topology.OwnershipUnavailable(rel, string(reason))
}

// OwnershipObservation is what ownership reads from a fetched object, whose
// type meta must be set.
func OwnershipObservation(obj any) (ownership.Observation, bool) {
	o, ok := obj.(runtime.Object)
	if !ok {
		return ownership.Observation{}, false
	}
	m, err := apimeta.Accessor(o)
	gvk := o.GetObjectKind().GroupVersionKind()
	if err != nil || gvk.Kind == "" {
		return ownership.Observation{}, false
	}
	ref := resourceid.Ref{Group: gvk.Group, Kind: gvk.Kind, Namespace: m.GetNamespace(), Name: m.GetName()}
	return ownership.Observe(ref, gvk.GroupVersion().String(), m), true
}
