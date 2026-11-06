import contextlib
import io
import json
import subprocess
import unittest
from unittest.mock import patch

import milestone2_migrate as migrate


class MigrationPreparationTests(unittest.TestCase):
    def users(self):
        return {owner: f"nordikdriveapi-{owner}@planar-ray-472112-e8.iam" for owner in ("gateway", "legacy", "worker")}

    def test_exact_development_users_are_accepted(self):
        migrate.validate_users(self.users())

    def test_wrong_project_or_injected_username_is_rejected(self):
        for value in ("someone@another-project.iam", 'user"; DROP SCHEMA public; --'):
            users = self.users()
            users["gateway"] = value
            with self.assertRaises(RuntimeError):
                migrate.validate_users(users)

    def test_default_mode_reads_only_local_state(self):
        result = subprocess.CompletedProcess([], 0, json.dumps(self.users()).encode(), b"")
        with patch.object(migrate.subprocess, "run", return_value=result) as run, \
                patch.object(migrate, "cloud") as cloud, patch("sys.argv", ["migrate"]), \
                contextlib.redirect_stdout(io.StringIO()):
            migrate.main()
        self.assertEqual(run.call_count, 1)
        self.assertIn("output", run.call_args.args[0])
        cloud.assert_not_called()

    def test_password_stays_out_of_docker_arguments_and_output(self):
        private = "synthetic-private-password"
        entries = [{"name": name, "value": value} for name, value in {
            "DB_HOST": "34.11.220.37", "DB_NAME": "postgres", "DB_USER": "postgres", "DB_PASSWORD": private,
        }.items()]
        service = {"spec": {"template": {"spec": {"containers": [{"env": entries}]}}}}
        calls = []

        def run(command, **kwargs):
            calls.append((command, kwargs))
            if "output" in command:
                return subprocess.CompletedProcess(command, 0, json.dumps(self.users()).encode(), b"")
            return subprocess.CompletedProcess(command, 0, b"", b"")

        output = io.StringIO()
        with patch.object(migrate.subprocess, "run", side_effect=run), \
                patch.object(migrate, "cloud", return_value=json.dumps(service).encode()), \
                patch("sys.argv", ["migrate", "--apply"]), contextlib.redirect_stdout(output):
            migrate.main()
        docker_calls = [(command, kwargs) for command, kwargs in calls if command[0] == "docker"]
        self.assertEqual(len(docker_calls), 3)
        for command, kwargs in docker_calls:
            self.assertNotIn(private, " ".join(command))
            self.assertEqual(kwargs["env"]["PGPASSWORD"], private)
        self.assertNotIn(private, output.getvalue())


if __name__ == "__main__":
    unittest.main()
