import contextlib
import io
import json
import unittest
from unittest.mock import patch

import milestone2_seed_secrets as seed


class SecretPreparationTests(unittest.TestCase):
    def test_literal_payload_is_preserved_byte_for_byte(self):
        self.assertEqual(seed.read_payload({"value": "keep-this-exact\r\n"}), b"keep-this-exact\r\n")

    def test_secret_reference_is_read_without_reencoding(self):
        value = b"\x00\xffexact\n"
        with patch.object(seed, "cloud", return_value=value) as cloud:
            self.assertEqual(seed.read_payload({"valueFrom": {"secretKeyRef": {
                "name": "existing-jwt", "key": "7"}}}), value)
        cloud.assert_called_once_with("secrets", "versions", "access", "7", "--secret=existing-jwt")

    def test_dry_run_reads_only_and_does_not_print_values(self):
        service = {"spec": {"template": {"spec": {
            "serviceAccountName": "runtime@example.com",
            "containers": [{"env": [{"name": "JWT_SECRET", "value": "must-stay-private"}]}],
        }}}}
        output = io.StringIO()
        with patch.object(seed, "cloud", return_value=json.dumps(service).encode()) as cloud, \
                patch("sys.argv", ["seed"]), contextlib.redirect_stdout(output):
            seed.main()
        self.assertEqual(cloud.call_count, 1)
        self.assertEqual(cloud.call_args.args[:3], ("run", "services", "describe"))
        self.assertNotIn("must-stay-private", output.getvalue())
        self.assertIn("Dry run", output.getvalue())

    def test_cloud_failure_does_not_relay_credentials(self):
        result = type("Result", (), {"returncode": 1, "stderr": b"private-password", "stdout": b""})()
        with patch.object(seed.shutil, "which", return_value="gcloud"), \
                patch.object(seed.subprocess, "run", return_value=result):
            with self.assertRaises(RuntimeError) as caught:
                seed.cloud("secrets", "versions", "access", "1", "--secret=fixture")
        self.assertNotIn("private-password", str(caught.exception))


if __name__ == "__main__":
    unittest.main()
