import copy
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
import check_runtime_plan as p
import runtime_rollout as r


def change(kind="google_cloud_run_v2_service",name="nordikdriveapi-worker",actions=None):
    return {"type":kind,"address":"fixture","change":{"actions":actions or ["update"],"after":{"name":name,"project":r.PROJECT,"location":r.REGION}}}


def signup_key_retirement():
    revision="nordikdriveapi-legacy-keyless"
    grant={"address":'google_secret_manager_secret_iam_member.runtime["legacy:SIGNUP_VERIFICATION_KEYS"]',"type":"google_secret_manager_secret_iam_member","change":{"actions":["delete"],"after":None,"before":{
        "project":r.PROJECT,"role":"roles/secretmanager.secretAccessor",
        "member":"serviceAccount:nordikdriveapi-legacy@"+r.PROJECT+".iam.gserviceaccount.com",
        "secret_id":"projects/"+r.PROJECT+"/secrets/nordikdrive-m3-signup-verification-keys"}}}
    variables={name:{"value":True} for name in ("enable_identity","enable_identity_consumers","identity_public_enabled","identity_outbox_exclusive","legacy_signup_key_access_retired_verified")}
    variables["runtime_serving_revisions"]={"value":{"legacy":revision}}
    legacy={"address":'google_cloud_run_v2_service.backend["legacy"]',"type":"google_cloud_run_v2_service","values":{
        "name":r.SERVICES["legacy"],"project":r.PROJECT,"location":r.REGION,
        "template":[{"revision":revision,"containers":[{"env":[{"name":"LEGACY_IDENTITY_PUBLIC_DISABLED","value":"true"}]}]}]}}
    return {"resource_changes":[grant],"variables":variables,"planned_values":{"root_module":{"resources":[legacy]}}}


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

    def test_signup_accessor_retirement_is_explicit_and_metadata_only(self):
        plan=signup_key_retirement()
        with self.assertRaises(r.Refused):p.verify(plan,"legacy",metadata_only=True)
        self.assertEqual(p.verify(plan,"legacy",metadata_only=True,allow_legacy_signup_key_revocation=True),[])
        for target,options in (("notifications",{"metadata_only":True}),("legacy",{}),("legacy",{"job_only":True})):
            with self.subTest(target=target,options=options):
                with self.assertRaises(r.Refused):p.verify(plan,target,allow_legacy_signup_key_revocation=True,**options)

    def test_cli_requires_the_explicit_retirement_option(self):
        with tempfile.TemporaryDirectory() as directory:
            plan=Path(directory)/"retirement plan.json";plan.write_text(json.dumps(signup_key_retirement()),encoding="utf-8")
            command=[sys.executable,str(Path(p.__file__)),str(plan),"--target","legacy","--metadata-only"]
            denied=subprocess.run(command,capture_output=True,text=True,check=False)
            self.assertNotEqual(denied.returncode,0)
            allowed=subprocess.run(command+["--allow-legacy-signup-key-revocation"],capture_output=True,text=True,check=False)
            self.assertEqual(allowed.returncode,0,allowed.stderr)
            self.assertIn("one verified Legacy signup-key accessor revocation",allowed.stdout)

    def test_retirement_refuses_other_grants_replacement_or_extra_deletion(self):
        for field,value in (("project","production"),("role","roles/owner"),("member","allUsers"),("member","serviceAccount:nordikdriveapi-identity@"+r.PROJECT+".iam.gserviceaccount.com"),("secret_id","projects/"+r.PROJECT+"/secrets/nordikdrive-m2-jwt")):
            plan=signup_key_retirement();plan["resource_changes"][0]["change"]["before"][field]=value
            with self.subTest(field=field,value=value):
                with self.assertRaises(r.Refused):p.verify(plan,"legacy",metadata_only=True,allow_legacy_signup_key_revocation=True)
        for field,value in (("address","unrelated"),("type","google_secret_manager_secret")):
            plan=signup_key_retirement();plan["resource_changes"][0][field]=value
            with self.assertRaises(r.Refused):p.verify(plan,"legacy",metadata_only=True,allow_legacy_signup_key_revocation=True)
        for actions in (["delete","create"],["create","delete"],["update"]):
            plan=signup_key_retirement();plan["resource_changes"][0]["change"]["actions"]=actions
            with self.assertRaises(r.Refused):p.verify(plan,"legacy",metadata_only=True,allow_legacy_signup_key_revocation=True)
        for extra in (change(actions=["delete"]),copy.deepcopy(signup_key_retirement()["resource_changes"][0]),change(name=r.SERVICES["legacy"]),change("google_billing_project_info")):
            plan=signup_key_retirement();plan["resource_changes"].append(extra)
            with self.assertRaises(r.Refused):p.verify(plan,"legacy",metadata_only=True,allow_legacy_signup_key_revocation=True)

    def test_retirement_requires_keyless_accepted_legacy_and_cutover(self):
        for name in signup_key_retirement()["variables"]:
            plan=signup_key_retirement();del plan["variables"][name]
            with self.subTest(missing=name):
                with self.assertRaises(r.Refused):p.verify(plan,"legacy",metadata_only=True,allow_legacy_signup_key_revocation=True)
        for field,value in (("name","nordikdriveapi-prod"),("project","production"),("location","us-east1"),("template",[])):
            plan=signup_key_retirement();plan["planned_values"]["root_module"]["resources"][0]["values"][field]=value
            with self.assertRaises(r.Refused):p.verify(plan,"legacy",metadata_only=True,allow_legacy_signup_key_revocation=True)
        for alteration in ("missing_legacy","wrong_revision","key_reference","auth_enabled","extra_container"):
            plan=signup_key_retirement();resources=plan["planned_values"]["root_module"]["resources"];template=resources[0]["values"]["template"][0]
            if alteration=="missing_legacy":resources.clear()
            elif alteration=="wrong_revision":template["revision"]="nordikdriveapi-legacy-unaccepted"
            elif alteration=="key_reference":template["containers"][0]["env"].append({"name":"SIGNUP_VERIFICATION_KEYS","value_source":[{"secret_key_ref":[{"secret":"existing","version":"1"}]}]})
            elif alteration=="auth_enabled":template["containers"][0]["env"]=[]
            else:template["containers"].append({"env":[]})
            with self.subTest(alteration=alteration):
                with self.assertRaises(r.Refused):p.verify(plan,"legacy",metadata_only=True,allow_legacy_signup_key_revocation=True)


if __name__=="__main__":unittest.main()
