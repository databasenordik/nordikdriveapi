import json
import unittest

from milestone2_inventory import public_url, safe_service_snapshot


class InventoryPrivacyTests(unittest.TestCase):
    def test_literal_credentials_dsn_and_annotations_are_not_exported(self):
        service = {
            "metadata": {"name": "example", "annotations": {"last-applied": "ANNOTATION_SECRET"}},
            "spec": {"template": {"spec": {"containers": [{"image": "fixture@sha256:123", "env": [
                {"name": "JWT_SECRET", "value": "JWT_PAYLOAD"},
                {"name": "GMAIL_APP_PASSWORD", "value": "SMTP_PAYLOAD"},
                {"name": "ASSISTANT_DATABASE_URL", "value": "postgresql://alice:DB_PAYLOAD@127.0.0.1/postgres?password=QUERY_PAYLOAD"},
                {"name": "UNKNOWN_KEY", "value": "UNRECOGNIZED_PAYLOAD"},
            ]}]}}},
            "status": {"url": "https://example.run.app"},
        }
        safe = safe_service_snapshot(service)
        serialized = json.dumps(safe)
        for secret in ("ANNOTATION_SECRET", "JWT_PAYLOAD", "SMTP_PAYLOAD", "DB_PAYLOAD", "QUERY_PAYLOAD", "UNRECOGNIZED_PAYLOAD"):
            self.assertNotIn(secret, serialized)
        self.assertEqual(safe["database_targets"], [{"setting": "ASSISTANT_DATABASE_URL", "host": "127.0.0.1", "database": "postgres"}])
        self.assertIn("JWT_SECRET", safe["plaintext_credential_setting_names"])
        self.assertFalse(safe["public_import_config_has_no_literal_credentials"])

    def test_secret_references_and_safe_configuration_are_retained(self):
        service = {"spec": {"template": {"spec": {"containers": [{"env": [
            {"name": "JWT_SECRET", "valueFrom": {"secretKeyRef": {"name": "fixture-jwt", "key": "2"}}},
            {"name": "DB_HOST", "value": "127.0.0.1"},
            {"name": "DB_NAME", "value": "postgres"},
            {"name": "NIA_TOKEN_AUDIENCE", "value": "https://nia.run.app"},
        ]}]}}}}
        safe = safe_service_snapshot(service)
        self.assertEqual(safe["secret_references"], [{"setting": "JWT_SECRET", "secret_id": "fixture-jwt", "version": "2"}])
        self.assertEqual(safe["environment"]["NIA_TOKEN_AUDIENCE"], "https://nia.run.app")
        self.assertTrue(safe["public_import_config_has_no_literal_credentials"])

    def test_even_allowlisted_settings_cannot_export_uri_credentials(self):
        service = {"spec": {"template": {"spec": {"containers": [{"env": [
            {"name": "DB_HOST", "value": "postgres://user:HOST_PAYLOAD@localhost/db"},
        ]}]}}}}
        safe = safe_service_snapshot(service)
        self.assertNotIn("HOST_PAYLOAD", json.dumps(safe))
        self.assertEqual(safe["environment"]["DB_HOST"], "<credential-bearing value omitted>")

    def test_endpoint_sanitization_removes_userinfo_queries_and_fragments(self):
        self.assertEqual(public_url("https://user:URL_PAYLOAD@example.run.app/job?secret=QUERY_PAYLOAD#FRAGMENT_PAYLOAD"), "https://example.run.app/job")

    def test_legacy_serialized_configuration_blocks_public_import(self):
        service = {"metadata": {"annotations": {"kubectl.kubernetes.io/last-applied-configuration": "LEGACY_CONFIG_PAYLOAD"}}}
        safe = safe_service_snapshot(service)
        self.assertNotIn("LEGACY_CONFIG_PAYLOAD", json.dumps(safe))
        self.assertFalse(safe["public_import_config_has_no_literal_credentials"])
        self.assertEqual(safe["serialized_configuration_annotation_names"], ["service:kubectl.kubernetes.io/last-applied-configuration"])


if __name__ == "__main__":
    unittest.main()
