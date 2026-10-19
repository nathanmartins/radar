package k8s

import (
	"errors"
	"testing"

	"github.com/skyhook-io/radar/pkg/ownership"
	"github.com/skyhook-io/radar/pkg/ownership/ownershiptest"
	"github.com/skyhook-io/radar/pkg/readreason"
	"github.com/skyhook-io/radar/pkg/resourceid"
	"github.com/skyhook-io/radar/pkg/topology"
)

// scriptedSource opens sessions over the ownership fixture whose Validate
// returns the next scripted error.
type scriptedSource struct {
	t        *testing.T
	begin    error
	validate []error
	begun    int
}

type scriptedSession struct {
	ownership.Session
	validate error
}

func (s scriptedSession) Validate() error { return s.validate }

func (s *scriptedSource) Begin() (ownership.Session, error) {
	if s.begin != nil {
		return nil, s.begin
	}
	inner, err := ownershiptest.Scan(ownershiptest.Fixture()).Begin()
	if err != nil {
		s.t.Fatal(err)
	}
	var v error
	if s.begun < len(s.validate) {
		v = s.validate[s.begun]
	}
	s.begun++
	return scriptedSession{Session: inner, validate: v}, nil
}

func TestAnswerOwnershipRetriesAReplacedStoreAndRefusesReplacedCaches(t *testing.T) {
	pod := resourceid.Ref{Kind: "Pod", Namespace: "alpha", Name: "web-1-a"}
	var fetched ownership.Observation
	for _, o := range ownershiptest.Fixture().Objects {
		if o.Ref == pod {
			fetched = ownership.Observation{Ref: o.Ref, APIVersion: o.APIVersion, UID: o.UID, OwnerReferences: o.Owners}
		}
	}
	unrestricted := func(*ownership.Catalog) ownership.Access { return ownership.Unrestricted() }
	for _, tc := range []struct {
		name        string
		source      *scriptedSource
		begun       int
		unavailable readreason.Code
	}{
		{"valid at once", &scriptedSource{}, 1, ""},
		{"a store replaced once", &scriptedSource{validate: []error{ownership.ErrStoreReplaced}}, 2, ""},
		{"a store replaced twice", &scriptedSource{validate: []error{ownership.ErrStoreReplaced, ownership.ErrStoreReplaced}}, 2, readreason.KindSyncPending},
		{"objects changed once", &scriptedSource{validate: []error{ownership.ErrObjectsChanged}}, 2, ""},
		{"objects changed twice", &scriptedSource{validate: []error{ownership.ErrObjectsChanged, ownership.ErrObjectsChanged}}, 2, readreason.ReadError},
		{"a store replaced, then objects changed", &scriptedSource{validate: []error{ownership.ErrStoreReplaced, ownership.ErrObjectsChanged}}, 2, readreason.ReadError},
		{"caches replaced", &scriptedSource{validate: []error{ownership.ErrCachesReplaced}}, 1, readreason.ConnectionChanged},
		{"no caches", &scriptedSource{begin: errors.New("not connected")}, 0, readreason.ClusterConnecting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.source.t = t
			rel := &topology.Relationships{}
			answerOwnership(tc.source, topology.ApplyOwnership, rel, fetched, unrestricted)
			if tc.source.begun != tc.begun {
				t.Errorf("began %d sessions, want %d", tc.source.begun, tc.begun)
			}
			if rel.Ownership == nil || rel.Ownership.Unavailable != string(tc.unavailable) {
				t.Fatalf("ownership = %+v, want unavailable %q", rel.Ownership, tc.unavailable)
			}
			if answered := rel.Owner != nil; answered != (tc.unavailable == "") {
				t.Fatalf("owner = %+v; an answer only when the session held", rel.Owner)
			}
		})
	}
}
