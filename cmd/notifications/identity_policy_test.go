package main

import "testing"

func TestIdentityNotificationsPolicyIsExplicitAndExclusiveOnlyAfterCutover(t *testing.T) {
	identity := "identity@p.iam.gserviceaccount.com"
	for _, test := range []struct {
		identity, raw    string
		valid, exclusive bool
	}{{"", "", true, false}, {"", "false", true, false}, {"", "true", false, false}, {identity, "bad", false, false}, {"not-a-service-account", "true", false, false}, {"legacy@p.iam.gserviceaccount.com", "true", false, false}, {"tasks@p.iam.gserviceaccount.com", "true", false, false}, {identity, "false", true, false}, {identity, "true", true, true}} {
		t.Run(test.identity+test.raw, func(t *testing.T) {
			grants := map[string]map[string]bool{"producer:identity": {"legacy@p.iam.gserviceaccount.com": true, "worker@p.iam.gserviceaccount.com": true}, "producer:records": {"legacy@p.iam.gserviceaccount.com": true}, "delivery": {"tasks@p.iam.gserviceaccount.com": true}, "recovery": {"worker@p.iam.gserviceaccount.com": true}}
			err := configureIdentityPolicy(grants, test.identity, test.raw)
			if (err == nil) != test.valid {
				t.Fatal("unexpected policy result", err)
			}
			if !test.valid {
				return
			}
			if grants["producer:identity"]["legacy@p.iam.gserviceaccount.com"] == test.exclusive {
				t.Fatal("legacy producer transition changed")
			}
			if test.identity != "" && (!grants["producer:identity"][identity] || !grants["recovery"][identity]) {
				t.Fatal("owner dispatch/recovery grant missing")
			}
			if !grants["producer:records"]["legacy@p.iam.gserviceaccount.com"] || !grants["recovery"]["worker@p.iam.gserviceaccount.com"] {
				t.Fatal("unrelated producer/recovery policy changed")
			}
		})
	}
}
