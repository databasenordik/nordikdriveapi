import datetime as dt
import unittest
from unittest.mock import patch

import runtime_deploy as d
import runtime_rollout as r
import runtime_window_verify as w
from test_runtime_deploy import FakeAPI,IMAGE,metrics


class WIFWindowTests(unittest.TestCase):
    def setUp(self):
        self.api=FakeAPI()
        self.now=dt.datetime.now(dt.timezone.utc)
        self.coordinator=r.Coordinator(self.api,lambda:self.now)
        self.coordinator.bootstrap()
        self.responses={"version":2,"sessions":[],"observed_at":self.now.isoformat()}
        self.runtime=d.Runtime(self.api,self.coordinator,self.fetch)
        self.value=self.runtime.stage("worker",IMAGE,"reviewed-release")
        self.token=self.value["token"]
        self.calls=[]
    def fetch(self,url,token,raw=False):
        if raw:return b"healthy"
        self.calls.append(url)
        if "/rollout/connections" in url:return self.responses
        return metrics("nordikdriveapi-worker-old",self.now)
    def test_exact_candidate_is_verified_without_promoting_or_staging_an_image(self):
        before=len(self.api.revisions)
        result=w.verify(self.runtime,"worker",self.token,"test")
        self.assertTrue(result["verified"])
        self.assertEqual(len(self.api.revisions),before)
        self.assertEqual(d.serving_revision(self.api.get(d.name("worker"))),"nordikdriveapi-worker-old")
        self.assertTrue(self.calls[0].startswith("https://m4-candidate.example/"))
        self.assertEqual(r.marker(self.api.get(r.GATEWAY))["state"],"tested")
    def test_wrong_target_phase_or_token_cannot_use_the_workflow_identity(self):
        for target,token,operation in (("identity",self.token,"test"),("worker","wrong","test"),("worker",self.token,"drain"),("worker",self.token,"stage")):
            with self.assertRaises(r.Refused):w.prepare(self.runtime,target,token,operation)
    def test_missing_stale_or_nonaggregate_inspection_never_marks_candidate_tested(self):
        valid=self.responses
        for value in ({},{**valid,"version":1},{**valid,"observed_at":"invalid"},{**valid,"observed_at":(self.now-dt.timedelta(minutes=1)).isoformat()},{**valid,"sessions":[{"query":"unwanted"}]},{**valid,"sessions":[{"application":"run:old","state":"idle","owner":"worker","count":-1}]}):
            self.responses=value
            with self.assertRaises(r.Refused):w.verify(self.runtime,"worker",self.token,"test")
            self.assertEqual(r.marker(self.api.get(r.GATEWAY))["state"],"staged")
    def test_drain_releases_only_after_complete_evidence(self):
        w.verify(self.runtime,"worker",self.token,"test")
        self.runtime.promote(self.token)
        self.responses={**self.responses,"sessions":[{"application":"run:nordikdriveapi-worker-old","state":"idle","owner":"worker","count":1}]}
        with self.assertRaises(r.Refused):w.verify(self.runtime,"worker",self.token,"drain",timeout=0)
        self.assertEqual(r.marker(self.api.get(r.GATEWAY))["state"],"promoted")
        self.responses["sessions"]=[]
        result=w.verify(self.runtime,"worker",self.token,"drain",timeout=0)
        self.assertTrue(result["verified"])
        self.assertEqual(r.marker(self.api.get(r.GATEWAY))["state"],"idle")


if __name__=="__main__":unittest.main()
