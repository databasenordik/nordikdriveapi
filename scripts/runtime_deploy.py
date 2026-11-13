"""Stage, test and promote one development revision under the persistent marker.

Failed/cancelled windows remain held. Image publication never acquires this lock.
"""
from __future__ import annotations
import argparse
import copy
import datetime as dt
import json
import hashlib
import os
from pathlib import Path
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

import runtime_rollout as r

TAG = "m4-candidate"
RETIRED_ACCEPTANCE_TAGS = {"m3-completion"}
TEMPLATE_FIELDS={"revision","labels","annotations","scaling","vpcAccess","timeout","serviceAccount","containers","volumes","executionEnvironment","encryptionKey","maxInstanceRequestConcurrency","serviceMesh","encryptionKeyRevocationAction","encryptionKeyShutdownDuration","sessionAffinity","healthCheckDisabled","nodeSelector","client","clientVersion","gpuZonalRedundancyDisabled"}

def template_shape(template):
    if set(template)-TEMPLATE_FIELDS:raise r.Refused("Unrecognized template fields require explicit rollback review")
    shape={"fields":sorted(template),"labels":sorted(template.get("labels",{})),"annotations":sorted(template.get("annotations",{}))}
    options={key:template[key] for key in ("healthCheckDisabled",) if key in template}
    return shape,options

def accepted_template(revision,shape,options):
    result={}
    if set(shape.get("fields",[]))-TEMPLATE_FIELDS:raise r.Refused("Unsupported accepted template shape")
    for key in shape.get("fields",[]):
        if key=="revision":continue
        if key in {"labels","annotations"}:
            values=revision.get(key,{})
            if any(item not in values for item in shape[key]):raise r.Refused("Accepted revision metadata is incomplete")
            result[key]={item:values[item] for item in shape[key]}
        elif key in revision:result[key]=copy.deepcopy(revision[key])
        elif key in options:result[key]=options[key]
        elif key in {"sessionAffinity","gpuZonalRedundancyDisabled"}:result[key]=False
        else:raise r.Refused("Accepted revision is missing template field "+key)
    if not result.get("containers"):raise r.Refused("Accepted revision has no runtime containers")
    return result

def rollback_ownership(target,revision,phase):
    values={item["name"]:item.get("value","") for item in image_container(revision,target).get("env",[])}
    if phase=="m3":return
    if target in {"legacy","worker","audit"} and not values.get("IDENTITY_URL"):
        raise r.Refused("Accepted rollback revision lacks the Identity owner API")
    if target=="worker" and not values.get("IDENTITY_NOTIFICATION_DISPATCH_URL"):
        raise r.Refused("Accepted rollback revision lacks private Identity dispatch")
    if target=="notifications" and not values.get("NOTIFICATION_OUTBOX_IDENTITY_URL"):
        raise r.Refused("Accepted rollback revision lacks the Identity notification source")
    if target=="gateway":
        try:backends=json.loads(values.get("SERVICE_BACKENDS_JSON","{}"))
        except (ValueError,TypeError):raise r.Refused("Accepted gateway routing is invalid") from None
        if "identity" not in backends:raise r.Refused("Rollback would remove Identity routing")
        if phase in {"audit_active","audit_relocated","grants_removed"} and "audit" not in backends:
            raise r.Refused("Rollback would remove Audit routing")
    if phase in {"audit_active","audit_relocated","grants_removed"} and target in {"legacy","identity"} and values.get("AUDIT_EVENTS_ENABLED")!="true":
        raise r.Refused("Accepted rollback revision lacks transactional Audit production")
    if phase=="grants_removed" and target=="legacy" and (values.get("LEGACY_IDENTITY_PUBLIC_DISABLED")!="true" or values.get("LEGACY_AUDIT_PUBLIC_DISABLED")!="true"):
        raise r.Refused("Accepted rollback revision still exposes retired owner routes")


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args):
        return None


def request_json(url, token, *, raw=False):
    if not token:
        raise r.Refused("Required deployment identity token is missing")
    req = urllib.request.Request(url, headers={"Authorization":"Bearer "+token,
        "X-Serverless-Authorization":"Bearer "+token})
    try:
        with urllib.request.build_opener(NoRedirect).open(req, timeout=30) as response:
            if response.status != 200:
                raise r.Refused("Runtime inspection returned HTTP "+str(response.status))
            return response.read(128*1024) if raw else json.load(response)
    except urllib.error.HTTPError as error:
        raise r.Refused("Runtime inspection returned HTTP "+str(error.code)) from None
    except (OSError,ValueError):
        raise r.Refused("Runtime inspection is unavailable") from None


def name(target):
    if target not in r.SERVICES:
        raise r.Refused("Only reviewed development services are supported")
    return f"projects/{r.PROJECT}/locations/{r.REGION}/services/{r.SERVICES[target]}"


def validate_inspection(value):
    if value.get("version")!=2 or not isinstance(value.get("sessions"),list):
        raise r.Refused("Versioned aggregate inspection is required before promotion")
    try:observed=dt.datetime.fromisoformat(value["observed_at"].replace("Z","+00:00"))
    except (ValueError,KeyError,TypeError,AttributeError):raise r.Refused("Timestamped inspection is required") from None
    if observed.tzinfo is None or not 0<=(dt.datetime.now(dt.timezone.utc)-observed).total_seconds()<=30:
        raise r.Refused("Inspection evidence is stale")
    for item in value["sessions"]:
        if not isinstance(item,dict) or set(item)!={"application","state","owner","count"} or item.get("owner") not in {*r.SERVICES,"external"} or type(item.get("count")) is not int or item["count"]<0:
            raise r.Refused("Inspection must contain aggregate counts only")


def serving_revision(service):
    traffic=[item for item in service.get("trafficStatuses",[]) if item.get("percent",0)>0]
    if len(traffic)!=1 or traffic[0].get("percent")!=100 or not traffic[0].get("revision"):
        raise r.Refused("One explicit serving revision at 100 percent is required")
    return traffic[0]["revision"].rsplit("/",1)[-1]


def image_container(template,target):
    containers=template.get("containers",[])
    selected=[item for item in containers if item.get("name")=="authorization"] if target=="gateway" else containers
    if len(selected)!=1:
        raise r.Refused("The expected runtime container is not unique")
    return selected[0]


def instance_points(payload,service,revision):
    """Require explicit active AND idle series. Absent samples never mean zero."""
    groups={"active":[],"idle":[]}
    if payload.get("nextPageToken"):
        raise r.Refused("Incomplete instance telemetry cannot prove draining")
    for series in payload.get("timeSeries",[]):
        labels=series.get("resource",{}).get("labels",{})
        if series.get("resource",{}).get("type")!="cloud_run_revision" or labels.get("service_name")!=service or labels.get("revision_name")!=revision:
            raise r.Refused("Instance telemetry belongs to another runtime")
        state=series.get("metric",{}).get("labels",{}).get("state")
        if state not in groups:
            raise r.Refused("Unexpected instance state in telemetry")
        points=series.get("points",[])
        if not points:
            raise r.Refused("Instance state has no observed samples")
        latest=max(points,key=lambda point:point["interval"]["endTime"])
        groups[state].append({"count":int(latest["value"]["int64Value"]),"observed_at":latest["interval"]["endTime"]})
    result={}
    for state,points in groups.items():
        if not points:
            raise r.Refused("Missing explicit "+state+" instance telemetry")
        result[state]={"count":sum(item["count"] for item in points),"observed_at":min(item["observed_at"] for item in points)}
    return result


class Runtime:
    def __init__(self,api=None,coordinator=None,fetch=request_json):
        self.api=api or r.API()
        self.coordinator=coordinator or r.Coordinator(self.api)
        self.fetch=fetch

    def snapshots(self):
        result={}
        for target in r.SERVICES:
            resource=name(target)
            try:value=self.api.get(resource)
            except r.Missing:result[resource]=None;continue
            projection={field:value.get(field) for field in ("uid","template","traffic","scaling","latestCreatedRevision")}
            result[resource]=hashlib.sha256(json.dumps(projection,sort_keys=True,separators=(",",":")).encode()).hexdigest()
        return result

    def verify_other_runtimes(self,window,*,include_target=False):
        current=self.snapshots()
        for resource,fingerprint in window.get("runtime_snapshots",{}).items():
            if not include_target and resource==name(window["target"]):continue
            if current.get(resource)!=fingerprint:
                raise r.Refused("Another runtime changed inside this window; explicit recovery required")

    def prepare(self,target,image,owner,*,new_service=False):
        if new_service:
            if target not in {"identity","audit"}:raise r.Refused("Only new private owner services may use provisioning windows")
            try:self.api.get(name(target))
            except r.Missing:current=None
            else:raise r.Refused("Provisioning target already exists")
            previous=None
        else:
            current=self.api.get(name(target))
            previous=serving_revision(current)
            latest=current.get("latestCreatedRevision","").rsplit("/",1)[-1]
            restored=None
            if latest!=previous:
                idle=r.marker(self.api.get(r.GATEWAY)) or {}
                parked=idle.get("parked_revisions",{}).get(target)
                if not parked or parked.get("latest_created_revision")!=latest or parked.get("serving_revision")!=previous or parked.get("uid")!=current.get("uid"):
                    raise r.Refused("An outstanding revision needs explicit inspection/recovery")
                restored=accepted_template(self.api.get(name(target)+"/revisions/"+previous),parked["template_shape"],parked["template_options"])
            if current.get("scaling",{}).get("maxInstanceCount")!=1 or current.get("template",{}).get("scaling",{}).get("maxInstanceCount")!=1:
                raise r.Refused("The reviewed single-instance runtime limits are required")
            if any(item.get("tag")==TAG for item in current.get("traffic",[])):
                raise r.Refused("An old candidate tag needs explicit recovery")
        snapshots=self.snapshots()
        token=self.coordinator.acquire(target,image,owner)
        revision=r.SERVICES[target][:43]+"-m4-"+uuid.uuid4().hex[:12]
        gateway,value=self.coordinator.read_owned(token)
        detail={"revision":revision,"previous_revision":previous,"runtime_snapshots":snapshots,"new_service":new_service}
        if current is not None:
            before_template=restored or current["template"]
            shape,options=template_shape(before_template)
            container=image_container(before_template,target)
            detail.update(service_uid=current["uid"],previous_template_shape=shape,previous_template_options=options,restore_accepted_template=restored is not None,previous_image=container["image"],previous_traffic=current["traffic"],previous_labelled=any(env.get("name")=="DB_APPLICATION_LABEL_VERSION" and env.get("value")=="1" for env in container.get("env",[])))
        self.coordinator._write(gateway,{**value,**detail})
        return {"token":token,"target":target,"revision":revision,"image":image}

    def record_stage(self,token):
        _,window=self.coordinator.read_owned(token)
        if window["state"]!="held":raise r.Refused("A held prepared window is required")
        self.verify_other_runtimes(window)
        updated=self.api.get(name(window["target"]))

        if updated.get("scaling",{}).get("maxInstanceCount")!=1 or updated.get("template",{}).get("scaling",{}).get("maxInstanceCount")!=1:
            raise r.Refused("Staged runtime scaling exceeds the reviewed single-instance allocation")
        revision=window["revision"]
        if updated.get("latestReadyRevision","").rsplit("/",1)[-1]!=revision:
            raise r.Refused("Candidate is not the exact ready revision; window remains held")
        actual=self.api.get(name(window["target"])+"/revisions/"+revision)
        if image_container(actual,window["target"]).get("image")!=window["image"]:
            raise r.Refused("Ready revision image differs from the reviewed digest")
        if window.get("new_service"):
            if serving_revision(updated)!=revision or updated.get("invokerIamDisabled"):
                raise r.Refused("New private service readiness or IAM protection differs")
        elif serving_revision(updated)!=window["previous_revision"]:
            raise r.Refused("Configuration staging moved serving traffic")
        self.coordinator.advance(token,"staged",revision=revision,previous_revision=window["previous_revision"])

    def close_no_replacement(self,token,reason):
        current,window=self.coordinator.read_owned(token)
        if window["state"]!="held" or not reason:
            raise r.Refused("Explicit held-window recovery reason required")
        self.verify_other_runtimes(window,include_target=True)
        try:self.api.get(name(window["target"])+"/revisions/"+window["revision"])
        except r.Missing:pass
        else:raise r.Refused("Planned revision exists and must pass test/drain recovery")
        return self.coordinator._write(current,{"version":1,"state":"idle","verified":True,"completed_at":self.coordinator.now().isoformat(),"no_runtime_replacement":True,"recovery_reason":reason,"parked_revisions":window.get("parked_revisions",{})})

    def finish_provisioning(self,token):
        current,window=self.coordinator.read_owned(token)
        if window["state"]!="tested" or not window.get("new_service") or window.get("previous_revision") is not None or not window.get("checks_passed"):
            raise r.Refused("Only a tested first private revision with verified prior absence may finish provisioning")
        if window["runtime_snapshots"].get(name(window["target"])) is not None:
            raise r.Refused("Provisioning window did not prove prior absence")
        self.verify_other_runtimes(window)
        actual=self.api.get(name(window["target"]))
        if serving_revision(actual)!=window["revision"]:
            raise r.Refused("Provisioned revision changed after its test")
        return self.coordinator._write(current,{"version":1,"state":"idle","verified":True,"completed_at":self.coordinator.now().isoformat(),"last_revision":window["revision"],"first_private_revision":True,"previous_service_absent":True,"parked_revisions":window.get("parked_revisions",{})})

    def stage(self,target,image,owner):
        current=self.api.get(name(target))
        prepared=self.prepare(target,image,owner)
        token=prepared["token"]
        previous=serving_revision(current)
        # Record the intended name before a mutation: an interrupted API response
        # can then be recovered by exact revision, rather than by latest.
        revision=prepared["revision"]
        _,window=self.coordinator.read_owned(token)
        template=accepted_template(self.api.get(name(target)+"/revisions/"+previous),window["previous_template_shape"],window["previous_template_options"]) if window.get("restore_accepted_template") else copy.deepcopy(current["template"])
        template["revision"]=revision
        image_container(template,target)["image"]=image
        traffic=copy.deepcopy(current["traffic"])
        for item in traffic:
            if item.get("type")=="TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST":
                item["type"]="TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION"
                item["revision"]=previous
        traffic.append({"type":"TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION","revision":revision,"percent":0,"tag":TAG})
        # The marker changes the gateway etag, so refresh that resource when it
        # is also the target. Its runtime configuration must still match.
        refreshed=self.api.get(name(target))
        for field in ("uid","template","traffic"):
            if refreshed.get(field)!=current.get(field):
                raise r.Refused("Runtime configuration changed during acquisition")
        updated=self.api.patch(refreshed,{"template":template,"traffic":traffic},"template,traffic")
        self.record_stage(token)
        return prepared

    def test(self,token):
        _,window=self.coordinator.read_owned(token)
        if window["state"] not in {"staged","recovering"}:
            raise r.Refused("Only the recorded staged/recovering revision can be tested")
        self.verify_other_runtimes(window)
        current=self.api.get(name(window["target"]))
        if window.get("rollback") and serving_revision(current)==window["revision"]:
            address=current["uri"]
        elif window.get("new_service"):
            if serving_revision(current)!=window["revision"] or current.get("invokerIamDisabled"):
                raise r.Refused("First private revision changed or its IAM check is disabled")
            address=current["uri"]
        else:
            candidates=[item for item in current.get("trafficStatuses",[]) if item.get("tag")==TAG and item.get("revision","").rsplit("/",1)[-1]==window["revision"]]
            if len(candidates)!=1 or candidates[0].get("percent",0)!=0 or not candidates[0].get("uri"):
                raise r.Refused("Exact zero-traffic candidate URL is unavailable")
            address=candidates[0]["uri"]
        if window["target"]=="worker":
            # The signed fixed database query proves readiness more strongly
            # than a health response, including on the first foundation image.
            validate_inspection(self.fetch(address+"/internal/v1/rollout/connections",os.environ.get("WORKER_INSPECTION_ID_TOKEN","")))
        else:
            self.fetch(address+"/health",os.environ.get("RUNTIME_ID_TOKEN",""),raw=True)
        actual=self.api.get(name(window["target"])+"/revisions/"+window["revision"])
        if image_container(actual,window["target"]).get("image")!=window["image"]:
            raise r.Refused("Tested revision digest does not match its release")
        self.coordinator.advance(token,"tested",revision=window["revision"],checks_passed=True)

    def promote(self,token):
        _,window=self.coordinator.read_owned(token)
        if window["state"]!="tested" or not window.get("checks_passed"):
            raise r.Refused("An exact tested revision is required before promotion")
        self.verify_other_runtimes(window)
        current=self.api.get(name(window["target"]))
        if serving_revision(current)!=window.get("expected_serving_revision",window["previous_revision"]):
            raise r.Refused("Serving traffic changed outside this window")
        traffic=[]
        for item in current.get("traffic",[]):
            if item.get("tag")==TAG:
                continue
            item=copy.deepcopy(item)
            if item.get("percent",0)>0:
                if item.get("tag") and item["tag"] not in RETIRED_ACCEPTANCE_TAGS:
                    raise r.Refused("A tagged serving route needs explicit compatibility review")
                continue
            traffic.append(item)
        traffic.append({"type":"TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION","revision":window["revision"],"percent":100})
        updated=self.api.patch(current,{"traffic":traffic},"traffic")
        if serving_revision(updated)!=window["revision"]:
            raise r.Refused("Promotion has not converged; window remains held")
        self.coordinator.advance(token,"promoted",revision=window["revision"],checks_passed=True)

    def rollback(self,token,evidence,reason):
        gateway,window=self.coordinator.read_owned(token)
        if not reason or window.get("new_service") or not window.get("previous_revision"):
            raise r.Refused("Rollback needs an existing accepted revision and explicit recovery reason")
        accepted=window.get("rollback_accepted_revision",window["previous_revision"])
        retired=window.get("rollback_retired_revision",window["revision"])
        old=self.api.get(name(window["target"])+"/revisions/"+accepted)
        old_image=image_container(old,window["target"])["image"]
        required=("public_contract_verified","cookies_verified","rollback_checks_passed")
        if evidence.get("target")!=window["target"] or evidence.get("revision")!=accepted or evidence.get("image")!=old_image or any(evidence.get(key) is not True for key in required):
            raise r.Refused("Exact compatible rollback evidence is required")
        phase=evidence.get("schema_phase")
        if phase not in {"m3","identity_active","identity_relocated","audit_active","audit_relocated","grants_removed"}:raise r.Refused("Record the verified database ownership phase")
        if phase!="m3" and evidence.get("owner_api_verified") is not True:raise r.Refused("Rollback must preserve extracted owner APIs")
        if phase in {"audit_active","audit_relocated","grants_removed"} and evidence.get("audit_outbox_verified") is not True:raise r.Refused("Rollback must preserve producer-owned Audit outboxes")
        rollback_ownership(window["target"],old,phase)
        if window.get("rollback"):
            shape,options=window["rollback_template_shape"],window["rollback_template_options"]
        else:shape,options=window["previous_template_shape"],window["previous_template_options"]
        # Confirm the accepted configuration can be reconstructed without storing
        # environment/secret values in the marker or creating a third revision.
        accepted_template(old,shape,options)
        self.verify_other_runtimes(window)
        current=self.api.get(name(window["target"]))
        serving=serving_revision(current)
        if serving not in {accepted,retired}:raise r.Refused("Traffic changed outside the recovery window")
        retired_resource=self.api.get(name(window["target"])+"/revisions/"+retired)
        retired_labelled=any(env.get("name")=="DB_APPLICATION_LABEL_VERSION" and env.get("value")=="1" for env in image_container(retired_resource,window["target"]).get("env",[]))
        if not retired_labelled:raise r.Refused("Verify revision labelling for the candidate before compatible rollback")
        self.coordinator._write(gateway,{**window,"state":"recovering","rollback":True,"rollback_accepted_revision":accepted,"rollback_retired_revision":retired,"rollback_template_shape":shape,"rollback_template_options":options,"previous_labelled":True,"revision":accepted,"image":old_image,"previous_revision":retired,"expected_serving_revision":serving,"checks_passed":False,"recovery_reason":reason,"compatibility_phase":phase})
        traffic=[]
        for item in current.get("traffic",[]):
            if item.get("tag")==TAG:continue
            item=copy.deepcopy(item)
            if item.get("type")=="TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST":item.update(type="TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION",revision=serving)
            traffic.append(item)
        if serving!=accepted:traffic.append({"type":"TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION","revision":accepted,"percent":0,"tag":TAG})
        refreshed=self.api.get(name(window["target"]))
        if any(refreshed.get(field)!=current.get(field) for field in ("uid","template","traffic")):
            raise r.Refused("Runtime changed while preparing compatible rollback")
        self.api.patch(refreshed,{"traffic":traffic},"traffic")

    def observe_drain(self,token):
        _,window=self.coordinator.read_owned(token)
        if window["state"]!="promoted":
            raise r.Refused("Drain observation requires completed promotion")
        self.verify_other_runtimes(window)
        if serving_revision(self.api.get(name(window["target"])))!=window["revision"]:
            raise r.Refused("Serving traffic changed before drain acceptance")
        retired=window["previous_revision"]
        label=r.application_name(retired)
        inspection=self.fetch(f"https://nordikdriveapi-worker-724838782318.{r.REGION}.run.app/internal/v1/rollout/connections",os.environ.get("WORKER_INSPECTION_ID_TOKEN",""))
        validate_inspection(inspection)
        sessions=sum(int(item["count"]) for item in inspection["sessions"] if item.get("application")==label)
        unlabelled=sum(int(item["count"]) for item in inspection["sessions"] if item.get("owner")==window["target"] and not item.get("application","").startswith("run:"))
        now=dt.datetime.now(dt.timezone.utc)
        evidence={"revision":retired,"application":label,"sql_observed":True,"sql_sessions":sessions,"sql_observed_at":inspection["observed_at"],"unlabelled_observed":True,"unlabelled_sessions":unlabelled}
        revision_name=name(window["target"])+"/revisions/"+retired
        try:retirement=r.retired_revision(self.api.get(revision_name),revision_name,now)
        except r.Refused:pass
        else:return {**evidence,"retirement":retirement}
        filters=f'metric.type="run.googleapis.com/container/instance_count" AND resource.type="cloud_run_revision" AND resource.labels.service_name="{r.SERVICES[window["target"]]}" AND resource.labels.revision_name="{retired}"'
        query=urllib.parse.urlencode({"filter":filters,"interval.startTime":(now-dt.timedelta(minutes=10)).isoformat(),"interval.endTime":now.isoformat(),"view":"FULL","pageSize":"100"})
        payload=self.fetch(f"https://monitoring.googleapis.com/v3/projects/{r.PROJECT}/timeSeries?"+query,os.environ.get("GOOGLE_OAUTH_ACCESS_TOKEN",""))
        return {**evidence,**instance_points(payload,r.SERVICES[window["target"]],retired)}


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command",choices=["describe","bootstrap","prepare","prepare-new","record-stage","finish-provisioning","close-no-replacement","stage","test","promote","drain","deploy","recover","rollback"])
    parser.add_argument("--target",choices=list(r.SERVICES))
    parser.add_argument("--image")
    parser.add_argument("--owner",default=os.environ.get("GITHUB_RUN_ID",""))
    parser.add_argument("--token")
    parser.add_argument("--reason")
    parser.add_argument("--evidence-file")
    parser.add_argument("--output")
    parser.add_argument("--drain-timeout",type=int,default=1200)
    args=parser.parse_args()
    runtime=Runtime()
    if args.command=="describe":
        value={"rollout":r.marker(runtime.api.get(r.GATEWAY))}
        if args.target:
            current=runtime.api.get(name(args.target));value.update(audience=current["uri"],service=name(args.target))
    elif args.command=="bootstrap":
        current=runtime.coordinator.bootstrap(resume_token=args.token,recovery_reason=args.reason);value={"state":r.marker(current)["state"]}
    elif args.command in {"prepare","prepare-new"}:
        value=runtime.prepare(args.target,args.image,args.owner,new_service=args.command=="prepare-new")
    elif args.command=="record-stage":
        runtime.record_stage(args.token);value={"staged":True}
    elif args.command=="finish-provisioning":
        runtime.finish_provisioning(args.token);value={"provisioned":True}
    elif args.command=="close-no-replacement":
        runtime.close_no_replacement(args.token,args.reason);value={"no_runtime_replacement":True}
    elif args.command in {"stage","deploy"}:
        value=runtime.stage(args.target,args.image,args.owner)
        if args.output:
            Path(args.output).write_text(json.dumps(value,indent=2)+"\n",encoding="utf-8")
        if args.command=="deploy":
            runtime.test(value["token"]);runtime.promote(value["token"])
    elif args.command=="test":
        runtime.test(args.token);value={"tested":True}
    elif args.command=="promote":
        runtime.promote(args.token);value={"promoted":True}
    elif args.command=="recover":
        runtime.coordinator.advance(args.token,"recovering",recovery_reason=args.reason);value={"state":"recovering"}
    elif args.command=="rollback":
        if not args.evidence_file:raise r.Refused("A reviewed compatible rollback evidence file is required")
        runtime.rollback(args.token,json.loads(Path(args.evidence_file).read_text(encoding="utf-8-sig")),args.reason);value={"rollback_prepared":True}
    else:
        value={"token":args.token}
    if args.command in {"deploy","drain"}:
        deadline=time.monotonic()+args.drain_timeout
        while True:
            try:
                evidence=runtime.observe_drain(value["token"])
                runtime.coordinator.release(value["token"],evidence)
                value["drained"]=True;break
            except r.Refused as error:
                if time.monotonic()>=deadline:
                    raise r.Refused(str(error)+"; persistent window remains blocked") from None
                print("Waiting for explicit SQL and active/idle instance drain evidence",flush=True)
                time.sleep(30)
    if args.output and args.command!="deploy":
        Path(args.output).write_text(json.dumps(value,indent=2)+"\n",encoding="utf-8")
    print(json.dumps(value),flush=True)


if __name__=="__main__":
    try:main()
    except r.Refused as error:raise SystemExit(str(error)) from None
