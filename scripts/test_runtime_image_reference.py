import copy
import hashlib
import json
import unittest

import runtime_image_reference as r

IMAGE="us-west1-docker.pkg.dev/planar-ray-472112-e8/nordikdriveapi/nordik_worker_go"
CHILD="sha256:"+"a"*64


class RuntimeImageTests(unittest.TestCase):
    def setUp(self):
        self.child={"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":CHILD,"platform":{"os":"linux","architecture":"amd64"}}
        self.document={"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[self.child,{"platform":{"os":"unknown","architecture":"unknown"},"digest":"sha256:"+"b"*64}]}
    def select(self,document):
        raw=json.dumps(document,separators=(",",":")).encode();digest="sha256:"+hashlib.sha256(raw).hexdigest()
        return r.reference(IMAGE,digest,raw)
    def test_selects_runtime_without_discarding_attestation_index(self):
        self.assertEqual(self.select(self.document),IMAGE+"@"+CHILD)
        self.assertEqual(len(self.document["manifests"]),2)
    def test_supports_docker_manifest_lists_and_single_platform_images(self):
        self.document["mediaType"]="application/vnd.docker.distribution.manifest.list.v2+json"
        self.document["manifests"][0]["mediaType"]="application/vnd.docker.distribution.manifest.v2+json"
        self.assertEqual(self.select(self.document),IMAGE+"@"+CHILD)
        raw=b'{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}'
        digest="sha256:"+hashlib.sha256(raw).hexdigest()
        self.assertEqual(r.reference(IMAGE,digest,raw),IMAGE+"@"+digest)
        self.assertEqual(r.reference(IMAGE,digest,raw+b"\n"),IMAGE+"@"+digest)
    def test_unknown_ambiguous_or_missing_platforms_fail(self):
        for edit in (lambda d:d["manifests"].append(copy.deepcopy(self.child)),lambda d:d["manifests"].clear(),lambda d:d["manifests"][0]["platform"].update(architecture="arm64"),lambda d:d["manifests"][0].update(digest="tag:mutable"),lambda d:d.update(mediaType="unknown"),lambda d:d.update(schemaVersion=1),lambda d:d.update(manifests=None)):
            document=copy.deepcopy(self.document);edit(document)
            with self.assertRaises(ValueError):self.select(document)
    def test_untrusted_registry_digest_or_content_fail(self):
        raw=json.dumps(self.document).encode();digest="sha256:"+hashlib.sha256(raw).hexdigest()
        for image,value,data in (("other.example/image",digest,raw),(IMAGE,"latest",raw),(IMAGE,digest,raw+b" "),(IMAGE,digest,b"x"*(1024*1024+1))):
            with self.assertRaises(ValueError):r.reference(image,value,data)
        for raw in (b"invalid",b"[]"):
            with self.assertRaises(ValueError):r.reference(IMAGE,"sha256:"+hashlib.sha256(raw).hexdigest(),raw)


if __name__=="__main__":unittest.main()
