package main

import (
	"errors"
	"strconv"
	"strings"
)

func configureIdentityPolicy(grants map[string]map[string]bool, identity, exclusiveRaw string) error {
	exclusive := false
	if exclusiveRaw != "" {
		var err error
		exclusive, err = strconv.ParseBool(exclusiveRaw)
		if err != nil {
			return errors.New("NOTIFICATIONS_IDENTITY_EXCLUSIVE must be a boolean")
		}
	}
	if identity == "" {
		if exclusive {
			return errors.New("Identity-exclusive notifications require the Identity runtime account")
		}
		return nil
	}
	if !strings.HasSuffix(identity, ".iam.gserviceaccount.com") || strings.Count(identity, "@") != 1 || strings.ContainsAny(identity, " \r\n/") || grants["producer:identity"][identity] || grants["delivery"][identity] {
		return errors.New("Identity notifications require a distinct runtime service account")
	}
	grants["producer:identity"][identity] = true
	grants["recovery"][identity] = true
	if exclusive {
		grants["producer:identity"] = map[string]bool{identity: true}
	}
	return nil
}
