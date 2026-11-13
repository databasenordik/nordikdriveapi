"""Coordinate development runtime changes using a persistent, conditional marker.

The gateway marker survives runner cancellation. Locks never expire automatically.
Image publication does not use this lock; revision/configuration mutations do.
"""
from __future__ import annotations

import copy
import datetime as dt
import hashlib
import json
import os
import re
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

MARKER = "nordikdrive.io/runtime-rollout"
PROJECT = "planar-ray-472112-e8"
REGION = "us-west1"
GATEWAY = f"projects/{PROJECT}/locations/{REGION}/services/nordikdriveapi"
SERVICES = {"legacy": "nordikdriveapi-legacy", "worker": "nordikdriveapi-worker",
            "gateway": "nordikdriveapi", "notifications": "nordikdriveapi-notifications",
            "identity": "nordikdriveapi-identity", "audit": "nordikdriveapi-audit"}
IMAGES = {"legacy":"nordik_drive_go", "worker":"nordik_worker_go", "gateway":"nordik_gateway_go",
          "notifications":"nordik_notifications_go", "identity":"nordik_identity_go", "audit":"nordik_audit_go"}


class Refused(RuntimeError):
    pass

class Missing(Refused):
    pass


def application_name(revision, execution=False):
    if not re.fullmatch(r"[A-Za-z0-9_.-]{1,128}", revision):
        raise Refused("Invalid revision/execution label")
    prefix = "job:" if execution else "run:"
    return prefix + revision if len(prefix + revision) <= 63 else prefix + revision[:32] + ":" + hashlib.sha256(revision.encode()).hexdigest()[:24]


class API:
    def request(self, method, path, body=None):
        token = os.environ.get("GOOGLE_OAUTH_ACCESS_TOKEN", "")
        if not token:
            raise Refused("Existing deployment access credentials are required")
        req = urllib.request.Request("https://run.googleapis.com/v2/" + path,
            data=None if body is None else json.dumps(body).encode(), method=method,
            headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=30) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            if error.code in (409, 412):
                raise Refused("Concurrent modification; rollout refused") from None
            if error.code==404:
                raise Missing("Cloud Run request failed HTTP 404") from None
            raise Refused("Cloud Run request failed HTTP " + str(error.code)) from None

    def get(self, name):
        return self.request("GET", name)

    def patch(self, current, fields, mask):
        if not current.get("etag"):
            raise Refused("Conditional update requires a resource etag")
        operation = self.request("PATCH", current["name"] + "?updateMask=" + urllib.parse.quote(mask),
                                 {"name": current["name"], "etag": current["etag"], **fields})
        deadline = time.monotonic() + 180
        while not operation.get("done"):
            if time.monotonic() >= deadline:
                raise Refused("Operation is unfinished; rollout marker remains held")
            time.sleep(1)
            operation = self.get(operation["name"])
        if operation.get("error"):
            raise Refused("Cloud Run update failed; rollout marker remains held")
        return self.get(current["name"])


def marker(service):
    raw = (service.get("annotations") or {}).get(MARKER)
    if raw is None:
        return None
    try:
        value = json.loads(raw)
    except (ValueError, TypeError):
        raise Refused("Invalid persistent rollout marker") from None
    if not isinstance(value, dict) or value.get("version") != 1:
        raise Refused("Unsupported persistent rollout marker")
    return value


def untouched(before, after):
    for field in ("uid", "template", "traffic", "latestCreatedRevision"):
        if before.get(field) != after.get(field):
            raise Refused("Metadata update changed " + field + "; deployment blocked")
    for key, value in (before.get("annotations") or {}).items():
        if key != MARKER and (after.get("annotations") or {}).get(key) != value:
            raise Refused("Metadata update changed an unrelated annotation")


class Coordinator:
    def __init__(self, api, now=None):
        self.api = api
        self.now = now or (lambda: dt.datetime.now(dt.timezone.utc))

    def _write(self, current, value):
        annotations = {**(current.get("annotations") or {}), MARKER: json.dumps(value, separators=(",", ":"), sort_keys=True)}
        updated = self.api.patch(current, {"annotations": annotations}, "annotations")
        untouched(current, updated)
        if marker(updated) != value:
            raise Refused("Persistent marker was not retained")
        return updated

    def bootstrap(self):
        before = self.api.get(GATEWAY)
        previous = marker(before)
        if previous is not None:
            raise Refused("Marker already exists; inspect/recover the existing state")
        value = {"version": 1, "state": "bootstrap", "token": uuid.uuid4().hex, "verified": False}
        updated = self._write(before, value)
        # A deliberately stale write contains metadata only. If it is accepted,
        # conditional locking is unproven and the marker stays blocked.
        try:
            self.api.patch(before, {"annotations": updated["annotations"]}, "annotations")
        except Refused as error:
            if "Concurrent modification" not in str(error):
                raise
        else:
            raise Refused("Stale etag accepted; marker is blocked in bootstrap")
        untouched(before, self.api.get(GATEWAY))
        return self._write(updated, {"version": 1, "state": "idle", "verified": True,
                                    "verified_at": self.now().isoformat()})

    def acquire(self, target, image, owner):
        if target not in SERVICES or not isinstance(image,str) or not re.fullmatch(r"us-west1-docker\.pkg\.dev/planar-ray-472112-e8/nordikdriveapi/[A-Za-z0-9_-]+@sha256:[a-f0-9]{64}", image):
            raise Refused("Reviewed development service and immutable image required")
        if image.rsplit('/',1)[-1].split('@',1)[0] != IMAGES[target] or not owner:
            raise Refused("Image must belong to the selected service and owner must be recorded")
        current = self.api.get(GATEWAY)
        existing = marker(current)
        if not existing or not existing.get("verified") or existing.get("state") != "idle":
            raise Refused("Another or interrupted rollout is blocking this deployment")
        token = uuid.uuid4().hex
        self._write(current, {"version": 1, "verified": True, "state": "held", "token": token,
            "target": target, "image": image, "owner": owner, "parked_revisions":existing.get("parked_revisions",{}),"acquired_at": self.now().isoformat()})
        return token

    def read_owned(self, token):
        current = self.api.get(GATEWAY)
        value = marker(current)
        if not value or value.get("state") == "idle" or value.get("token") != token:
            raise Refused("This runner does not own the rollout window")
        return current, value

    def advance(self, token, phase, **evidence):
        if phase not in {"staged", "tested", "promoted", "recovering"}:
            raise Refused("Unsupported rollout phase")
        current, value = self.read_owned(token)
        allowed = {"held": {"staged", "recovering"}, "staged": {"tested", "recovering"},
                   "tested": {"promoted", "recovering"}, "promoted": {"recovering"},
                   "recovering": {"tested", "promoted"}}
        if phase not in allowed.get(value["state"], set()):
            raise Refused("Invalid rollout phase transition")
        if phase in {"staged", "tested", "promoted"}:
            revision=evidence.get("revision")
            if not isinstance(revision,str) or not re.fullmatch(r"[a-z][a-z0-9-]{0,62}",revision):
                raise Refused("A concrete revision is required")
        if phase=="recovering" and not evidence.get("recovery_reason"):
            raise Refused("Explicit recovery requires a recorded reason")
        if phase in {"tested", "promoted"}:
            if evidence.get("revision") != value.get("revision") or not evidence.get("checks_passed"):
                raise Refused("Exact tested revision evidence required")
        for key in evidence:
            if key not in {"revision", "previous_revision", "checks_passed", "recovery_reason"}:
                raise Refused("Unsupported rollout evidence")
        return self._write(current, {**value, **evidence, "state": phase, "updated_at": self.now().isoformat()})

    def release(self, token, evidence):
        current, value = self.read_owned(token)
        if value["state"] != "promoted":
            raise Refused("Unpromoted/interrupted window requires explicit recovery")
        if evidence.get("revision") != value.get("previous_revision"):
            raise Refused("Drain evidence belongs to another revision")
        label = application_name(evidence["revision"])
        if not evidence.get("sql_observed") or evidence.get("sql_sessions") != 0:
            raise Refused("PostgreSQL sessions have not been observed drained")
        if value.get("previous_labelled") is False and (not evidence.get("unlabelled_observed") or evidence.get("unlabelled_sessions")!=0):
            raise Refused("Pre-label runtime PostgreSQL sessions have not been observed drained")
        if evidence.get("application") != label:
            raise Refused("Wrong database revision label")
        try:
            sql_time=dt.datetime.fromisoformat(evidence["sql_observed_at"].replace("Z","+00:00"))
            promoted=dt.datetime.fromisoformat(value["updated_at"])
        except (KeyError,ValueError,TypeError):
            raise Refused("Timestamped PostgreSQL/promotion evidence required") from None
        if not 0 <= (self.now()-sql_time).total_seconds() <= 30 or sql_time < promoted:
            raise Refused("PostgreSQL evidence is stale or precedes promotion")
        for state in ("active", "idle"):
            point = evidence.get(state)
            if not isinstance(point, dict) or point.get("count") != 0:
                raise Refused("Missing/nonzero Cloud Run " + state + " evidence")
            try:
                observed = dt.datetime.fromisoformat(point["observed_at"].replace("Z", "+00:00"))
            except (KeyError, ValueError, TypeError):
                raise Refused("Invalid instance telemetry timestamp") from None
            if not 0 <= (self.now() - observed).total_seconds() <= 300 or observed < promoted:
                raise Refused("Instance telemetry is stale or from the future")
        parked=dict(value.get("parked_revisions",{}))
        if value.get("rollback"):
            parked[value["target"]]={"latest_created_revision":value["previous_revision"],"serving_revision":value["revision"],"uid":value["service_uid"],"template_shape":value["rollback_template_shape"],"template_options":value["rollback_template_options"]}
        else:
            parked.pop(value["target"],None)
        return self._write(current, {"version": 1, "state": "idle", "verified": True,
            "completed_at": self.now().isoformat(), "last_revision": value.get("revision"),"parked_revisions":parked})
