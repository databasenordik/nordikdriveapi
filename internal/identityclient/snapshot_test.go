package identityclient

import (
	"context"
	"testing"
)

func TestProfileSnapshotRejectsInvalidOwnerResponsesAndFreezesProfiles(t *testing.T) {
	for _, fixture := range []struct {
		ids      []int64
		profiles map[int64]Profile
	}{
		{[]int64{-1}, nil},
		{[]int64{1}, map[int64]Profile{2: {ID: 2}}},
		{[]int64{1}, map[int64]Profile{1: {ID: 2}}},
	} {
		if _, err := NewProfileSnapshot(fixture.ids, fixture.profiles, false); err == nil {
			t.Fatal("invalid snapshot accepted")
		}
	}
	original := map[int64]Profile{1: {ID: 1, Firstname: "Before", Email: "recipient@example.test"}}
	snapshot, err := NewProfileSnapshot([]int64{1, 2}, original, false)
	if err != nil {
		t.Fatal(err)
	}
	original[1] = Profile{ID: 1, Firstname: "After"}
	result, err := snapshot.Profiles(context.Background(), []int64{1, 2}, false)
	if err != nil || len(result) != 1 || result[1].Firstname != "Before" || result[1].Email != "" {
		t.Fatal("snapshot changed or disclosed email", err)
	}
}
