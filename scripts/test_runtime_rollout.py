import copy
import datetime as dt
import json
import unittest
import urllib.parse
from unittest.mock import patch

import runtime_rollout as r

NOW=dt.datetime(2026,11,12,18,tzinfo=dt.timezone.utc)
IMAGE='us-west1-docker.pkg.dev/planar-ray-472112-e8/nordikdriveapi/nordik_worker_go@sha256:'+'a'*64

class FakeAPI:
    def __init__(self):
        self.version=1
        self.data={'name':r.GATEWAY,'uid':'gateway','etag':'1','annotations':{'keep':'value'},
                   'template':{'containers':[{'image':'existing'}]},'traffic':[{'revision':'gateway-old','percent':100}],
                   'latestCreatedRevision':'gateway-old'}
        self.ignore_etag=False
        self.mutate_template=False
    def get(self,name):
        assert name==r.GATEWAY
        return copy.deepcopy(self.data)
    def patch(self,current,fields,mask):
        assert mask=='annotations'
        assert set(fields)=={'annotations'}
        if not self.ignore_etag and current['etag']!=self.data['etag']:
            raise r.Refused('Concurrent modification; rollout refused')
        self.version+=1
        self.data.update(copy.deepcopy(fields),etag=str(self.version))
        if self.mutate_template:self.data['latestCreatedRevision']='unexpected-revision'
        return self.get(r.GATEWAY)

class RolloutTests(unittest.TestCase):
    def setUp(self):
        self.api=FakeAPI();self.coordinator=r.Coordinator(self.api,lambda:NOW)
    def ready(self):
        self.coordinator.bootstrap()
        return self.coordinator.acquire('worker',IMAGE,'github/run/123')
    def promoted(self):
        token=self.ready()
        self.coordinator.advance(token,'staged',revision='worker-new',previous_revision='worker-old')
        self.coordinator.advance(token,'tested',revision='worker-new',checks_passed=True)
        self.coordinator.advance(token,'promoted',revision='worker-new',checks_passed=True)
        return token
    def evidence(self):
        return {'revision':'worker-old','application':r.application_name('worker-old'),'sql_observed':True,
                'sql_sessions':0,'sql_observed_at':NOW.isoformat(),
                'active':{'count':0,'observed_at':NOW.isoformat()},
                'idle':{'count':0,'observed_at':NOW.isoformat()}}
    def test_bootstrap_proves_conditional_metadata_only_updates(self):
        before=self.api.get(r.GATEWAY)
        after=self.coordinator.bootstrap()
        r.untouched(before,after)
        self.assertEqual(r.marker(after)['state'],'idle')
        self.assertTrue(r.marker(after)['verified'])
        with self.assertRaises(r.Refused):self.coordinator.bootstrap()
    def test_identical_noop_acceptance_does_not_weaken_conflicting_write_proof(self):
        class NoopAPI(FakeAPI):
            def patch(self,current,fields,mask):
                if fields.get('annotations')==self.data.get('annotations'):return self.get(r.GATEWAY)
                return super().patch(current,fields,mask)
        self.assertTrue(r.marker(r.Coordinator(NoopAPI()).bootstrap())['verified'])
    def test_failed_bootstrap_recovery_requires_exact_token_and_reason(self):
        self.api.ignore_etag=True
        with self.assertRaises(r.Refused):self.coordinator.bootstrap()
        token=r.marker(self.api.data)['token'];self.api.ignore_etag=False
        for provided,reason in ((None,'recover'),('wrong','recover'),(token,None)):
            with self.assertRaises(r.Refused):self.coordinator.bootstrap(resume_token=provided,recovery_reason=reason)
        result=self.coordinator.bootstrap(resume_token=token,recovery_reason='verified corrected metadata precondition')
        self.assertTrue(r.marker(result)['verified'])
        with self.assertRaises(r.Refused):self.coordinator.bootstrap(resume_token=token,recovery_reason='cannot overwrite idle coordination')
    def test_actual_http_request_includes_etag_in_update_mask(self):
        api=r.API();current={'name':r.GATEWAY,'etag':'expected-version'}
        for mask in ('annotations','annotations,etag'):
            with patch.object(api,'request',return_value={'done':True}) as request,patch.object(api,'get',return_value=current):
                api.patch(current,{'annotations':{'fixture':'value'}},mask)
                method,path,body=request.call_args.args
                self.assertEqual(method,'PATCH')
                fields=urllib.parse.parse_qs(urllib.parse.urlsplit(path).query)['updateMask'][0].split(',')
                self.assertEqual(fields.count('etag'),1)
                self.assertEqual(body['etag'],'expected-version')
    def test_weak_etag_or_revision_side_effect_blocks_bootstrap(self):
        for attribute in ('ignore_etag','mutate_template'):
            with self.subTest(attribute=attribute):
                api=FakeAPI();setattr(api,attribute,True)
                with self.assertRaises(r.Refused):r.Coordinator(api).bootstrap()
                self.assertFalse(r.marker(api.data)['verified'])
    def test_competing_and_interrupted_runners_cannot_acquire(self):
        token=self.ready()
        other=r.Coordinator(self.api,lambda:NOW+dt.timedelta(days=100))
        with self.assertRaises(r.Refused):other.acquire('worker',IMAGE,'other')
        self.assertEqual(other.read_owned(token)[1]['owner'],'github/run/123')
        with self.assertRaises(r.Refused):other.read_owned('wrong-token')
        with self.assertRaises(r.Refused):other.release(token,self.evidence())
    def test_failed_compare_and_swap_cannot_overwrite_another_window(self):
        self.coordinator.bootstrap()
        stale=self.api.get(r.GATEWAY)
        self.coordinator.acquire('worker',IMAGE,'winner')
        with self.assertRaises(r.Refused):
            self.coordinator._write(stale,{'version':1,'state':'idle','verified':True})
        self.assertEqual(r.marker(self.api.data)['owner'],'winner')
    def test_only_exact_tested_revision_can_be_promoted(self):
        token=self.ready()
        with self.assertRaises(r.Refused):self.coordinator.advance(token,'promoted',revision='worker-new',checks_passed=True)
        self.coordinator.advance(token,'staged',revision='worker-new',previous_revision='worker-old')
        for body in ({'revision':'wrong','checks_passed':True},{'revision':'worker-new','checks_passed':False}):
            with self.assertRaises(r.Refused):self.coordinator.advance(token,'tested',**body)
        with self.assertRaises(r.Refused):self.coordinator.advance(token,'tested',revision='worker-new',checks_passed=True,sql='arbitrary')
    def test_release_requires_all_current_drain_evidence(self):
        token=self.promoted()
        for field,value in [('revision','other'),('application','wrong'),('sql_sessions',1),('sql_observed',False),
                            ('sql_observed_at',(NOW-dt.timedelta(minutes=1)).isoformat()),('active',None),
                            ('idle',{'count':1,'observed_at':NOW.isoformat()}),
                            ('active',{'count':0,'observed_at':(NOW-dt.timedelta(minutes=6)).isoformat()}),
                            ('idle',{'count':0,'observed_at':(NOW+dt.timedelta(minutes=1)).isoformat()})]:
            with self.subTest(field=field,value=value):
                evidence=self.evidence();evidence[field]=value
                with self.assertRaises(r.Refused):self.coordinator.release(token,evidence)
                self.assertNotEqual(r.marker(self.api.data)['state'],'idle')
        after=self.coordinator.release(token,self.evidence())
        self.assertEqual(r.marker(after)['state'],'idle')
        self.assertEqual(after['annotations']['keep'],'value')
    def test_recovery_is_explicit_and_still_requires_test_and_drain(self):
        token=self.ready()
        with self.assertRaises(r.Refused):self.coordinator.advance(token,'recovering')
        self.coordinator.advance(token,'staged',revision='worker-new',previous_revision='worker-old')
        self.coordinator.advance(token,'recovering',recovery_reason='operator inspected interrupted revision')
        with self.assertRaises(r.Refused):self.coordinator.advance(token,'tested',revision=None,checks_passed=True)
        self.coordinator.advance(token,'tested',revision='worker-new',checks_passed=True)
        with self.assertRaises(r.Refused):self.coordinator.release(token,self.evidence())
    def test_prelabel_revision_requires_owner_scoped_sql_evidence(self):
        token=self.promoted()
        gateway,value=self.coordinator.read_owned(token)
        self.coordinator._write(gateway,{**value,'previous_labelled':False})
        evidence=self.evidence()
        with self.assertRaises(r.Refused):self.coordinator.release(token,evidence)
        evidence.update(unlabelled_observed=True,unlabelled_sessions=1)
        with self.assertRaises(r.Refused):self.coordinator.release(token,evidence)
        evidence['unlabelled_sessions']=0
        self.assertEqual(r.marker(self.coordinator.release(token,evidence))['state'],'idle')
    def test_inputs_labels_and_marker_fail_closed(self):
        for target,image,owner in [('production',IMAGE,'owner'),('worker',IMAGE.replace('@sha256:',':tag-'),'owner'),('identity',IMAGE,'owner'),('worker',IMAGE,'')]:
            with self.assertRaises(r.Refused):self.coordinator.acquire(target,image,owner)
        for raw in ('not-json','[]','{"version":2}'):
            with self.assertRaises(r.Refused):r.marker({'annotations':{r.MARKER:raw}})
        self.assertIsNone(r.marker({}))
        a=r.application_name('a'*100+'x');b=r.application_name('a'*100+'y')
        self.assertLessEqual(len(a),63);self.assertNotEqual(a,b)
        self.assertEqual(r.application_name('execution',True),'job:execution')
        with self.assertRaises(r.Refused):r.application_name('bad label')

if __name__=='__main__':unittest.main()
