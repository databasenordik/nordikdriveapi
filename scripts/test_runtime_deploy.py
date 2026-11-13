import copy
import datetime as dt
import os
import unittest
from unittest.mock import patch

import runtime_deploy as d
import runtime_rollout as r

IMAGE="us-west1-docker.pkg.dev/planar-ray-472112-e8/nordikdriveapi/nordik_worker_go@sha256:"+"a"*64
OLD=IMAGE.replace("a"*64,"b"*64)


class FakeAPI:
    def __init__(self):
        self.resources={}
        self.revisions={}
        for target in ("gateway","worker"):
            name=d.name(target);revision=r.SERVICES[target]+"-old"
            container={"image":OLD,"env":[{"name":"KEEP","value":"unchanged"},{"name":"DB_APPLICATION_LABEL_VERSION","value":"1"}]}
            if target=="gateway":container["name"]="authorization"
            template={"containers":[container],"scaling":{"maxInstanceCount":1}}
            self.resources[name]={"name":name,"uid":target,"etag":"1","annotations":{"keep":"value"},"template":template,"scaling":{"maxInstanceCount":1},"latestCreatedRevision":name+"/revisions/"+revision,"latestReadyRevision":name+"/revisions/"+revision,"traffic":[{"type":"TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST","percent":100}],"trafficStatuses":[{"revision":revision,"percent":100}],"uri":"https://"+target+".example"}
            self.revisions[name+"/revisions/"+revision]=copy.deepcopy(template)
    def get(self,name):
        if name not in self.resources and name not in self.revisions:raise r.Missing("Cloud Run request failed HTTP 404")
        return copy.deepcopy(self.resources.get(name) or self.revisions[name])
    def patch(self,current,fields,mask):
        actual=self.resources[current["name"]]
        if current["etag"]!=actual["etag"]:raise r.Refused("Concurrent modification; rollout refused")
        actual.update(copy.deepcopy(fields));actual["etag"]=str(int(actual["etag"])+1)
        if "template" in fields:
            revision=fields["template"]["revision"]
            actual["latestCreatedRevision"]=current["name"]+"/revisions/"+revision
            actual["latestReadyRevision"]=actual["latestCreatedRevision"]
            self.revisions[actual["latestCreatedRevision"]]=copy.deepcopy(fields["template"])
        if "traffic" in fields:
            actual["trafficStatuses"]=[{**item,"uri":"https://"+item.get("tag","serving")+".example"} for item in fields["traffic"]]
        return self.get(current["name"])


def metrics(revision,now,states=("active","idle"),count=0):
    return {"timeSeries":[{"resource":{"type":"cloud_run_revision","labels":{"service_name":"nordikdriveapi-worker","revision_name":revision}},"metric":{"labels":{"state":state}},"points":[{"interval":{"endTime":now.isoformat()},"value":{"int64Value":str(count)}}]} for state in states]}


class RuntimeTests(unittest.TestCase):
    def setUp(self):
        self.api=FakeAPI();self.now=dt.datetime.now(dt.timezone.utc)
        self.coordinator=r.Coordinator(self.api,lambda:self.now)
        self.coordinator.bootstrap();self.fetches=[]
        self.runtime=d.Runtime(self.api,self.coordinator,self.fetch)
    def fetch(self,url,token,raw=False):
        self.fetches.append((url,token))
        if raw:return b"healthy"
        if "/rollout/connections" in url:return {"version":2,"sessions":[{"application":"run:another","state":"active","owner":"external","count":5}],"observed_at":self.now.isoformat()}
        return metrics("nordikdriveapi-worker-old",self.now)
    def staged(self):return self.runtime.stage("worker",IMAGE,"github/run/123")
    def test_stage_preserves_serving_revision_and_unrelated_configuration(self):
        before=self.api.get(d.name("worker"));result=self.staged();after=self.api.get(d.name("worker"))
        self.assertEqual(d.serving_revision(after),d.serving_revision(before))
        self.assertEqual(after["template"]["containers"][0]["env"],before["template"]["containers"][0]["env"])
        self.assertEqual(after["annotations"],before["annotations"])
        window=r.marker(self.api.get(r.GATEWAY));self.assertEqual(window["state"],"staged");self.assertEqual(window["revision"],result["revision"])
        with self.assertRaises(r.Refused):self.coordinator.acquire("worker",IMAGE,"other")
    def test_exact_revision_test_promotion_and_drain(self):
        value=self.staged();token=value["token"]
        with self.assertRaises(r.Refused):self.runtime.promote(token)
        self.runtime.test(token);self.runtime.promote(token)
        self.assertEqual(d.serving_revision(self.api.get(d.name("worker"))),value["revision"])
        self.assertFalse(any(item.get("tag")==d.TAG for item in self.api.get(d.name("worker"))["traffic"]))
        evidence=self.runtime.observe_drain(token);self.coordinator.release(token,evidence)
        self.assertEqual(r.marker(self.api.get(r.GATEWAY))["state"],"idle")
    def test_old_m3_acceptance_tag_can_retire_but_unknown_serving_tags_block(self):
        for tag,allowed in (("m3-completion",True),("unreviewed-client-url",False)):
            with self.subTest(tag=tag):
                self.setUp()
                self.api.resources[d.name("worker")]["traffic"][0]["tag"]=tag
                value=self.staged();self.runtime.test(value["token"])
                if allowed:
                    self.runtime.promote(value["token"])
                    self.assertFalse(any(item.get("tag")==tag for item in self.api.get(d.name("worker"))["traffic"]))
                else:
                    with self.assertRaises(r.Refused):self.runtime.promote(value["token"])

    def test_unexpected_changes_after_testing_block_promotion_and_release(self):
        value=self.staged();self.runtime.test(value["token"])
        self.api.resources[r.GATEWAY]["template"]["timeout"]="900s"
        with self.assertRaises(r.Refused):self.runtime.promote(value["token"])
        del self.api.resources[r.GATEWAY]["template"]["timeout"]
        self.runtime.promote(value["token"])
        self.api.resources[d.name("worker")]["trafficStatuses"]=[{"revision":"unexpected","percent":100}]
        with self.assertRaises(r.Refused):self.runtime.observe_drain(value["token"])
    def test_wrong_test_image_and_outstanding_revision_remain_blocked(self):
        value=self.staged();key=d.name("worker")+"/revisions/"+value["revision"]
        self.api.revisions[key]["containers"][0]["image"]=OLD
        with self.assertRaises(r.Refused):self.runtime.test(value["token"])
        self.assertEqual(r.marker(self.api.get(r.GATEWAY))["state"],"staged")
        with self.assertRaises(r.Refused):self.runtime.stage("worker",IMAGE,"other")
    def test_failed_probe_never_moves_traffic(self):
        value=self.staged();before=self.api.get(d.name("worker"))["traffic"]
        def fail(*args,**kwargs):raise r.Refused("probe failed")
        self.runtime.fetch=fail
        with self.assertRaises(r.Refused):self.runtime.test(value["token"])
        self.assertEqual(self.api.get(d.name("worker"))["traffic"],before)
        with self.assertRaises(r.Refused):self.runtime.observe_drain(value["token"])
    def test_instance_evidence_never_infers_zero_from_missing_data(self):
        revision="nordikdriveapi-worker-old"
        for payload in ({},metrics(revision,self.now,states=("active",)),{**metrics(revision,self.now),"nextPageToken":"more"},metrics("other",self.now),metrics(revision,self.now,states=("unknown",))):
            with self.subTest(payload=payload):
                with self.assertRaises(r.Refused):d.instance_points(payload,"nordikdriveapi-worker",revision)
        points=d.instance_points(metrics(revision,self.now),"nordikdriveapi-worker",revision)
        self.assertEqual(points["active"]["count"],0);self.assertEqual(points["idle"]["count"],0)
    def test_invalid_service_and_ambiguous_runtime_are_refused(self):
        with self.assertRaises(r.Refused):d.name("production")
        for data in ({},{"trafficStatuses":[{"revision":"a","percent":50},{"revision":"b","percent":50}]}):
            with self.assertRaises(r.Refused):d.serving_revision(data)
        with self.assertRaises(r.Refused):d.image_container({"containers":[{},{}]},"worker")
        with self.assertRaises(r.Refused):d.image_container({"containers":[{}]},"gateway")
        with self.assertRaises(r.Refused):d.request_json("https://unused.example","")

    def test_prepared_metadata_and_uncreated_windows_need_explicit_no_change_proof(self):
        value=self.runtime.prepare("worker",IMAGE,"operator")
        with self.assertRaises(r.Refused):self.runtime.close_no_replacement(value["token"],"")
        worker=self.api.resources[d.name("worker")]
        worker["template"]["timeout"]="900s"
        with self.assertRaises(r.Refused):self.runtime.close_no_replacement(value["token"],"reviewed no replacement")
        del worker["template"]["timeout"]
        self.runtime.close_no_replacement(value["token"],"reviewed metadata-only plan; no runtime replaced")
        self.assertEqual(r.marker(self.api.get(r.GATEWAY))["state"],"idle")

    def test_first_private_provisioning_requires_prior_absence_exact_image_and_test(self):
        image=IMAGE.replace("nordik_worker_go","nordik_identity_go")
        with self.assertRaises(r.Refused):self.runtime.prepare("worker",IMAGE,"operator",new_service=True)
        value=self.runtime.prepare("identity",image,"operator",new_service=True)
        with self.assertRaises(r.Refused):self.runtime.finish_provisioning(value["token"])
        service=copy.deepcopy(self.api.resources[d.name("worker")]);service.update(name=d.name("identity"),uid="identity",uri="https://identity.example")
        revision=value["revision"];service["latestCreatedRevision"]=d.name("identity")+"/revisions/"+revision;service["latestReadyRevision"]=service["latestCreatedRevision"]
        service["template"]["containers"][0]["image"]=image;service["template"]["revision"]=revision
        service["traffic"]=[{"type":"TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION","revision":revision,"percent":100}];service["trafficStatuses"]=copy.deepcopy(service["traffic"])
        self.api.resources[d.name("identity")]=service;self.api.revisions[service["latestCreatedRevision"]]=copy.deepcopy(service["template"])
        self.runtime.record_stage(value["token"]);self.runtime.test(value["token"]);self.runtime.finish_provisioning(value["token"])
        final=r.marker(self.api.get(r.GATEWAY));self.assertTrue(final["first_private_revision"]);self.assertTrue(final["previous_service_absent"])

    def test_rollback_reuses_only_compatible_revision_then_drains_failed_candidate(self):
        for promoted in (False,True):
            with self.subTest(promoted=promoted):
                self.setUp();value=self.staged();token=value["token"]
                if promoted:self.runtime.test(token);self.runtime.promote(token)
                evidence={"target":"worker","revision":"nordikdriveapi-worker-old","image":OLD,"schema_phase":"m3","public_contract_verified":True,"cookies_verified":True,"rollback_checks_passed":True}
                before=len(self.api.revisions)
                with self.assertRaises(r.Refused):self.runtime.rollback(token,{**evidence,"cookies_verified":False},"recover")
                self.runtime.rollback(token,evidence,"recover tested accepted image")
                self.runtime.test(token);self.runtime.promote(token)
                self.assertEqual(d.serving_revision(self.api.get(d.name("worker"))),"nordikdriveapi-worker-old")
                self.assertEqual(len(self.api.revisions),before)
                drain={"revision":value["revision"],"application":r.application_name(value["revision"]),"sql_observed":True,"sql_sessions":0,"sql_observed_at":self.now.isoformat(),"active":{"count":0,"observed_at":self.now.isoformat()},"idle":{"count":0,"observed_at":self.now.isoformat()}}
                self.coordinator.release(token,drain)
                parked=r.marker(self.api.get(r.GATEWAY))["parked_revisions"]["worker"]
                self.assertEqual(parked["latest_created_revision"],value["revision"])
                # Subsequent releases use the accepted configuration and create
                # only one new revision after the failed one has drained.
                again=self.runtime.stage("worker",IMAGE,"next release")
                self.assertNotEqual(again["revision"],value["revision"])
                self.assertEqual(self.api.get(d.name("worker"))["template"]["containers"][0]["env"][0],{"name":"KEEP","value":"unchanged"})

    def test_rollback_cannot_restore_cross_owner_sql_after_migration(self):
        revision={"containers":[{"env":[]}]}
        for target,phase in (("worker","identity_active"),("legacy","identity_relocated"),("notifications","identity_active"),("gateway","identity_active"),("identity","audit_active")):
            if target=="gateway":revision["containers"][0]["name"]="authorization"
            with self.assertRaises(r.Refused):d.rollback_ownership(target,revision,phase)
            revision["containers"][0].pop("name",None)
        with self.assertRaises(r.Refused):d.accepted_template({}, {"fields":["unknown"]},{})


if __name__=="__main__":unittest.main()
