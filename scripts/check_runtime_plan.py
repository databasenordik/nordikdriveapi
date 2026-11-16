"""Refuse runtime changes outside one persistent development rollout window."""
import argparse
import json
from pathlib import Path
import runtime_rollout as r


def legacy_signup_key_revocation(change,plan):
    expected={"project":r.PROJECT,"role":"roles/secretmanager.secretAccessor",
        "member":"serviceAccount:nordikdriveapi-legacy@"+r.PROJECT+".iam.gserviceaccount.com",
        "secret_id":"projects/"+r.PROJECT+"/secrets/nordikdrive-m3-signup-verification-keys"}
    if change.get("address")!='google_secret_manager_secret_iam_member.runtime["legacy:SIGNUP_VERIFICATION_KEYS"]' or change.get("type")!="google_secret_manager_secret_iam_member" or change["change"].get("actions")!=["delete"]:
        return False
    before=change["change"].get("before") or {}
    if any(before.get(k)!=v for k,v in expected.items()):return False
    variables=plan.get("variables",{})
    if any(variables.get(k,{}).get("value") is not True for k in ("enable_identity","enable_identity_consumers","identity_public_enabled","identity_outbox_exclusive","legacy_signup_key_access_retired_verified")):return False
    serving=variables.get("runtime_serving_revisions",{}).get("value",{}).get("legacy")
    if not isinstance(serving,str) or not serving.startswith(r.SERVICES["legacy"]+"-"):return False
    resources=plan.get("planned_values",{}).get("root_module",{}).get("resources",[])
    legacy=[v.get("values",{}) for v in resources if v.get("address")=='google_cloud_run_v2_service.backend["legacy"]' and v.get("type")=="google_cloud_run_v2_service"]
    if len(legacy)!=1:return False
    value=legacy[0];template=value.get("template",[])
    if value.get("name")!=r.SERVICES["legacy"] or value.get("project")!=r.PROJECT or value.get("location")!=r.REGION or len(template)!=1 or not serving or template[0].get("revision")!=serving:return False
    containers=template[0].get("containers",[])
    if len(containers)!=1:return False
    env={v["name"]:v for v in containers[0].get("env",[])}
    return "SIGNUP_VERIFICATION_KEYS" not in env and env.get("LEGACY_IDENTITY_PUBLIC_DISABLED",{}).get("value")=="true"


def verify(plan,target,*,metadata_only=False,job_only=False,allow_legacy_signup_key_revocation=False):
    if target not in r.SERVICES:
        raise r.Refused("Unsupported development rollout target")
    if metadata_only and job_only:raise r.Refused("Choose metadata-only or a finite-job configuration window")
    if allow_legacy_signup_key_revocation and (not metadata_only or job_only or target!="legacy"):
        raise r.Refused("Legacy signup-key revocation requires its separate metadata-only window")
    runtime=[]
    jobs=[]
    revocations=0
    for change in plan.get("resource_changes",[]):
        actions=change.get("change",{}).get("actions",[])
        if actions in ([],["no-op"],["read"]):
            continue
        if "delete" in actions:
            if allow_legacy_signup_key_revocation and legacy_signup_key_revocation(change,plan):
                revocations+=1
                continue
            raise r.Refused("Deletion/replacement is outside this rollout")
        kind=change["type"]
        if "billing" in kind:
            raise r.Refused("Billing changes are outside Milestone 4")
        after=change["change"].get("after") or {}
        if "iam" in kind and any(principal in json.dumps(after) for principal in ("allUsers", "allAuthenticatedUsers")):
            raise r.Refused("New public IAM grants are outside this private-service rollout")
        if kind=="google_cloud_run_v2_service":
            if metadata_only or job_only or after.get("name")!=r.SERVICES[target] or after.get("project",r.PROJECT)!=r.PROJECT or after.get("location")!=r.REGION:
                raise r.Refused("Plan changes another runtime or environment")
            runtime.append(change["address"])
        elif kind=="google_cloud_run_v2_job":
            after=change["change"].get("after") or {}
            expected={"identity":"nordikdriveapi-signup-cleanup","gateway":"nordikdriveapi-limit-cleanup"}.get(target)
            if metadata_only or (not job_only and target!="identity") or after.get("name")!=expected or actions!=["update"] or after.get("project",r.PROJECT)!=r.PROJECT or after.get("location")!=r.REGION:
                raise r.Refused("Only an existing finite cleanup job may update with its owning runtime")
            jobs.append(change["address"])
    if allow_legacy_signup_key_revocation and revocations!=1:
        raise r.Refused("Review exactly the former Legacy signup-key accessor revocation")
    if job_only:
        if runtime or len(jobs)!=1:raise r.Refused("A finite-job window updates exactly one existing cleanup job and no service")
    elif len(runtime)>1 or (not metadata_only and len(runtime)!=1):
        raise r.Refused("Plan must mutate exactly one runtime; metadata windows mutate none")
    return runtime


if __name__=="__main__":
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument("plan")
    parser.add_argument("--target",required=True,choices=list(r.SERVICES))
    parser.add_argument("--metadata-only",action="store_true")
    parser.add_argument("--job-only",action="store_true")
    parser.add_argument("--allow-legacy-signup-key-revocation",action="store_true",help="Separate metadata-only retirement after keyless serving/rollback revisions and drain are verified")
    args=parser.parse_args()
    try:
        result=verify(json.loads(Path(args.plan).read_text(encoding="utf-8-sig")),args.target,metadata_only=args.metadata_only,job_only=args.job_only,allow_legacy_signup_key_revocation=args.allow_legacy_signup_key_revocation)
    except r.Refused as error:
        raise SystemExit(str(error)) from None
    suffix="; one verified Legacy signup-key accessor revocation" if args.allow_legacy_signup_key_revocation else "; no deletion or billing changes"
    print("Reviewed runtime plan: "+str(len(result))+" service mutation(s)"+suffix)
