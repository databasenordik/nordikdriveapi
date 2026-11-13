import unittest
from changed_services import affected_services, json_stream, SHARED_BUILD_FILES

class ChangeSelectionTests(unittest.TestCase):
    def setUp(self):
        self.services = {name: {"entrypoint": f"./cmd/{name}", "dockerfile": f"cmd/{name}/Dockerfile"} for name in ("legacy", "worker", "gateway", "notifications", "identity", "audit")}
        self.graphs = {name: {"directories": {f"cmd/{name}", f"internal/{name}", "internal/workload"}, "embedded": set()} for name in self.services}
        self.graphs["legacy"]["directories"].add("internal/auth")
        self.graphs["identity"]["directories"].add("internal/auth")
        self.graphs["legacy"]["directories"].add("internal/notificationcontract")
        self.graphs["notifications"]["directories"].add("internal/notificationcontract")
    def selected(self, *files):
        return [row["id"] for row in affected_services(set(files), self.services, self.graphs)]
    def test_notifications_is_independent(self):
        self.assertEqual(self.selected("internal/notifications/service.go"), ["notifications"])
    def test_shared_dependency_only_rebuilds_consumers(self):
        self.assertEqual(self.selected("internal/notificationcontract/command.go"), ["legacy", "notifications"])
    def test_auth_does_not_build_unrelated_services(self):
        self.assertEqual(self.selected("internal/auth/signup_otp.go"), ["legacy", "identity"])
    def test_new_owners_build_independently(self):
        self.assertEqual(self.selected("internal/identity/service.go"), ["identity"])
        self.assertEqual(self.selected("internal/audit/service.go"), ["audit"])
    def test_docs_and_tests_do_not_publish_images(self):
        self.assertEqual(self.selected("docs/milestone-3-plan.md", "internal/notifications/README.md", "internal/notifications/service_test.go", "scripts/test_changed_services.py"), [])
    def test_each_service_dockerfile_is_independent(self):
        for name in self.services:
            with self.subTest(name=name):
                self.assertEqual(self.selected(f"cmd/{name}/Dockerfile"), [name])
    def test_changed_deleted_and_moved_go_files_use_both_paths(self):
        self.assertEqual(self.selected("internal/legacy/old.go", "internal/notifications/new.go"), ["legacy", "notifications"])
    def test_embedded_documents_are_runtime_inputs(self):
        self.graphs["notifications"]["embedded"].add("internal/notifications/template.md")
        self.assertEqual(self.selected("internal/notifications/template.md"), ["notifications"])
    def test_shared_build_inputs_release_every_runtime(self):
        for path in SHARED_BUILD_FILES:
            with self.subTest(path=path):
                self.assertEqual(self.selected(path), list(self.services))
    def test_unknown_files_do_not_build(self):
        self.assertEqual(self.selected("infra/terraform/main.tf", "Dockerfile", "internal/future/service.go"), [])
    def test_json_stream_is_not_line_based(self):
        self.assertEqual(list(json_stream('{"Dir":"first"}\n{\n"Dir":"second"\n}\n')), [{"Dir":"first"}, {"Dir":"second"}])

if __name__ == "__main__":
    unittest.main()
