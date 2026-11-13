"""Select the immutable Linux amd64 image within a verified published manifest."""
import argparse
import hashlib
import json
from pathlib import Path
import re

INDEX_TYPES={"application/vnd.oci.image.index.v1+json","application/vnd.docker.distribution.manifest.list.v2+json"}
IMAGE_TYPES={"application/vnd.oci.image.manifest.v1+json","application/vnd.docker.distribution.manifest.v2+json"}
IMAGES={"nordik_drive_go","nordik_worker_go","nordik_gateway_go","nordik_notifications_go","nordik_identity_go","nordik_audit_go"}


def reference(image,digest,raw):
    prefix="us-west1-docker.pkg.dev/planar-ray-472112-e8/nordikdriveapi/"
    if not image.startswith(prefix) or image[len(prefix):] not in IMAGES or not re.fullmatch(r"sha256:[a-f0-9]{64}",digest):
        raise ValueError("Reviewed development image and immutable digest required")
    if len(raw)>1024*1024:raise ValueError("Manifest exceeds bounded input")
    # imagetools may terminate its raw output with one display newline.
    bodies=[raw]
    if raw.endswith(b"\n"):bodies.append(raw[:-1])
    body=next((item for item in bodies if "sha256:"+hashlib.sha256(item).hexdigest()==digest),None)
    if body is None:raise ValueError("Manifest content does not match its published digest")
    try:document=json.loads(body)
    except (ValueError,UnicodeError):raise ValueError("Invalid image manifest") from None
    if not isinstance(document,dict) or document.get("schemaVersion")!=2:raise ValueError("Unsupported image schema")
    media=document.get("mediaType")
    if media in IMAGE_TYPES and "manifests" not in document:return image+"@"+digest
    if media not in INDEX_TYPES or not isinstance(document.get("manifests"),list):raise ValueError("Unsupported published manifest type")
    selected=[item for item in document["manifests"] if isinstance(item,dict) and item.get("platform",{}).get("os")=="linux" and item.get("platform",{}).get("architecture")=="amd64"]
    if len(selected)!=1 or selected[0].get("mediaType") not in IMAGE_TYPES or not re.fullmatch(r"sha256:[a-f0-9]{64}",selected[0].get("digest","")):
        raise ValueError("Exactly one immutable Linux amd64 image is required")
    return image+"@"+selected[0]["digest"]


if __name__=="__main__":
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image",required=True);parser.add_argument("--digest",required=True);parser.add_argument("--manifest",required=True)
    args=parser.parse_args()
    try:print(reference(args.image,args.digest,Path(args.manifest).read_bytes()))
    except ValueError as error:raise SystemExit(str(error)) from None
