"""Refuse runtime changes outside one persistent development rollout window."""
import argparse
import json
from pathlib import Path
import runtime_rollout as r


def verify(plan,target,*,metadata_only=False,job_only=False):
    if target not in r.SERVICES:
        raise r.Refused("Unsupported development rollout target")
    if metadata_only and job_only:raise r.Refused("Choose metadata-only or a finite-job configuration window")
    runtime=[]
    jobs=[]
    for change in plan.get("resource_changes",[]):
        actions=change.get("change",{}).get("actions",[])
        if actions in ([],["no-op"],["read"]):
            continue
        if "delete" in actions:
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
    args=parser.parse_args()
    try:
        result=verify(json.loads(Path(args.plan).read_text(encoding="utf-8-sig")),args.target,metadata_only=args.metadata_only,job_only=args.job_only)
    except r.Refused as error:
        raise SystemExit(str(error)) from None
    print("Reviewed runtime plan: "+str(len(result))+" service mutation(s); no deletion or billing changes")
