"""Verify an existing development rollout window using GitHub's WIF identity.

This cannot stage images, change runtime configuration, or acquire a new window.
It tests the recorded candidate or releases a promoted window after real drain.
"""
import argparse
import json
import os
from pathlib import Path
import time

import runtime_deploy as d
import runtime_rollout as r


def prepare(runtime,target,token,operation):
    _,window=runtime.coordinator.read_owned(token)
    if target not in r.SERVICES or target!=window.get("target"):
        raise r.Refused("Verification target differs from the held window")
    phases={"test":{"staged","recovering"},"drain":{"promoted"}}
    if operation not in phases or window.get("state") not in phases[operation]:
        raise r.Refused("Verification operation does not match the recorded phase")
    runtime.verify_other_runtimes(window)
    return runtime.api.get(d.name(target))["uri"],window


def inspection(runtime,target,window):
    address="https://nordikdriveapi-worker-724838782318.us-west1.run.app"
    if target=="worker":
        service=runtime.api.get(d.name(target))
        if window.get("rollback") and d.serving_revision(service)==window["revision"]:
            address=service["uri"]
        else:
            candidates=[x for x in service.get("trafficStatuses",[]) if x.get("tag")==d.TAG and x.get("revision","").rsplit("/",1)[-1]==window["revision"] and x.get("percent",0)==0]
            if len(candidates)!=1 or not candidates[0].get("uri"):
                raise r.Refused("Exact Worker inspection candidate is unavailable")
            address=candidates[0]["uri"]
    value=runtime.fetch(address+"/internal/v1/rollout/connections",os.environ.get("WORKER_INSPECTION_ID_TOKEN",""))
    d.validate_inspection(value)


def verify(runtime,target,token,operation,*,timeout=1200):
    _,window=prepare(runtime,target,token,operation)
    if operation=="test":
        if target!="worker":inspection(runtime,target,window)
        runtime.test(token)
    else:
        deadline=time.monotonic()+timeout
        while True:
            try:
                runtime.coordinator.release(token,runtime.observe_drain(token))
                break
            except r.Refused:
                if time.monotonic()>=deadline:raise r.Refused("Explicit drain not established; persistent window remains blocked") from None
                print("Waiting for explicit SQL and active/idle instance drain evidence",flush=True)
                time.sleep(30)
    return {"target":target,"operation":operation,"revision":window["revision"],"verified":True}


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--target",required=True,choices=list(r.SERVICES))
    parser.add_argument("--token",required=True)
    parser.add_argument("--operation",required=True,choices=["test","drain"])
    parser.add_argument("--output")
    args=parser.parse_args()
    result=verify(d.Runtime(),args.target,args.token,args.operation)
    if args.output:Path(args.output).write_text(json.dumps(result,indent=2)+"\n",encoding="utf-8")
    print(json.dumps(result),flush=True)


if __name__=="__main__":
    try:main()
    except r.Refused as error:raise SystemExit(str(error)) from None
