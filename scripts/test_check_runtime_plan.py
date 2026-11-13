import copy
import unittest
import check_runtime_plan as p
import runtime_rollout as r


def change(kind="google_cloud_run_v2_service",name="nordikdriveapi-worker",actions=None):
    return {"type":kind,"address":"fixture","change":{"actions":actions or ["update"],"after":{"name":name,"project":r.PROJECT,"location":r.REGION}}}


class PlanScopeTests(unittest.TestCase):
    def verify(self,changes,target="worker",**kwargs):
        return p.verify({"resource_changes":changes},target,**kwargs)
    def test_one_runtime_and_required_metadata_are_allowed(self):
        self.assertEqual(self.verify([change(),change("google_service_account","identity")]),["fixture"])
        self.assertEqual(self.verify([change("google_service_account","identity")],metadata_only=True),[])
    def test_deletions_billing_other_runtimes_and_production_are_refused(self):
        for changes in ([change(actions=["delete"])],[change("google_billing_project_info")],[change(name="nordikdriveapi-prod")],[change(),change(name="nordikdriveapi-legacy")],[]):
            with self.subTest(changes=changes):
                with self.assertRaises(r.Refused):self.verify(changes)
        for field,value in (("project","production"),("location","us-east1")):
            item=change();item["change"]["after"][field]=value
            with self.assertRaises(r.Refused):self.verify([item])
        with self.assertRaises(r.Refused):self.verify([change()],metadata_only=True)
    def test_only_existing_signup_cleanup_transfers_with_identity(self):
        identity=change(name="nordikdriveapi-identity")
        cleanup=change("google_cloud_run_v2_job","nordikdriveapi-signup-cleanup")
        self.assertEqual(self.verify([identity,cleanup],"identity"),["fixture"])
        with self.assertRaises(r.Refused):self.verify([change(),cleanup])
        with self.assertRaises(r.Refused):self.verify([identity,change("google_cloud_run_v2_job","another-job")],"identity")
    def test_read_only_resources_do_not_require_runtime_mutation(self):
        self.assertEqual(self.verify([change(actions=["no-op"]),change(actions=["read"])],metadata_only=True),[])
        with self.assertRaises(r.Refused):self.verify([],"production",metadata_only=True)
    def test_private_services_cannot_receive_public_iam(self):
        for principal in ("allUsers","allAuthenticatedUsers"):
            grant=change("google_cloud_run_v2_service_iam_member","nordikdriveapi-identity")
            grant["change"]["after"]["member"]=principal
            with self.assertRaises(r.Refused):self.verify([change(),grant])
    def test_finite_job_configuration_is_separate_and_bounded(self):
        cleanup=change("google_cloud_run_v2_job","nordikdriveapi-limit-cleanup")
        self.assertEqual(self.verify([cleanup],"gateway",job_only=True),[])
        for changes in ([cleanup,cleanup],[change(),cleanup],[],[change("google_cloud_run_v2_job","other")],[change("google_cloud_run_v2_job","nordikdriveapi-limit-cleanup",["create"]) ]):
            with self.assertRaises(r.Refused):self.verify(changes,"gateway",job_only=True)
        with self.assertRaises(r.Refused):self.verify([cleanup],"worker",job_only=True)
        with self.assertRaises(r.Refused):self.verify([cleanup],"gateway",job_only=True,metadata_only=True)


if __name__=="__main__":unittest.main()
