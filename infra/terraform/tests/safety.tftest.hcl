mock_provider "google" {
  mock_resource "google_service_account" {
    override_during = plan
    defaults = {
      email = "fixture@test-project.iam.gserviceaccount.com"
      name  = "projects/test-project/serviceAccounts/fixture@test-project.iam.gserviceaccount.com"
    }
  }
  mock_resource "google_cloud_run_v2_service" {
    override_during = plan
    defaults        = { uri = "https://fixture.run.app" }
  }
}

variables {
  deployment_stage                     = "preview"
  iam_database_authentication_verified = true
  runtime_prerequisites_verified       = true
  project_id                           = "test-project"
  sql_instance_name                    = "fixture-instance"
  sql_connection_name                  = "test-project:us-west1:fixture-instance"
  database_name                        = "fixture"
  server_image                         = "us-west1-docker.pkg.dev/test-project/runtime/server@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  adapter_image                        = "us-west1-docker.pkg.dev/test-project/runtime/adapter@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
  jwt_secret                           = { secret_id = "fixture-jwt", version = "1" }
  trusted_proxy_hops                   = 1
  database_connection_capacity         = 100
  other_reserved_connections           = 0
  # Preserve the original fixture's no-overlap assumption. The new budget
  # suite separately exercises replacement capacity and unverified inventories.
  rollout_extra_instances    = { gateway = 0, legacy = 0, worker = 0 }
  connection_budget_reviewed = true
}

run "safe_defaults" {
  command = plan
  assert {
    condition     = google_cloud_run_v2_service.gateway["preview"].template[0].scaling[0].min_instance_count == 0 && alltrue([for service in google_cloud_run_v2_service.backend : service.template[0].scaling[0].min_instance_count == 0])
    error_message = "All services must scale to zero."
  }
  assert {
    condition     = alltrue([for schedule in google_cloud_scheduler_job.business : schedule.paused]) && google_cloud_scheduler_job.cleanup[0].paused
    error_message = "Schedules must remain paused until the manual cutover."
  }
  assert {
    condition     = length(google_cloud_run_v2_service_iam_member.public) == 0
    error_message = "The initial preview must not be publicly exposed."
  }
  assert {
    condition     = google_cloud_run_v2_service_iam_member.gateway_invoker[0].member == "serviceAccount:fixture@test-project.iam.gserviceaccount.com" && google_cloud_run_v2_service_iam_member.scheduler_invoker[0].role == "roles/run.invoker"
    error_message = "Backends require scoped workload invocation."
  }
  assert {
    condition     = !contains([for env in google_cloud_run_v2_service.backend["worker"].template[0].containers[0].env : env.name], "JWT_SECRET")
    error_message = "The worker must not receive the application signing secret."
  }
}

run "reject_database_overcommit" {
  command = plan
  variables { database_connection_capacity = 20 }
  expect_failures = [terraform_data.safeguards]
}

run "reject_unpinned_secret" {
  command = plan
  variables { jwt_secret = { secret_id = "fixture-jwt", version = "latest" } }
  expect_failures = [var.jwt_secret]
}

run "reject_plaintext_database_password" {
  command = plan
  variables { runtime_settings = { legacy = { DB_PASSWORD = "synthetic-rejected-value" } } }
  expect_failures = [terraform_data.safeguards]
}

run "reject_private_sql_without_network" {
  command = plan
  variables { sql_ip_type = "private" }
  expect_failures = [terraform_data.safeguards]
}

run "service_can_route_independently" {
  command = plan
  variables {
    service_backends = { records = { url = "https://records-fixture.run.app", audience = "https://records-fixture.run.app" } }
  }
  assert {
    condition     = strcontains(one([for env in google_cloud_run_v2_service.gateway["preview"].template[0].containers[1].env : env.value if env.name == "SERVICE_BACKENDS_JSON"]), "records-fixture.run.app")
    error_message = "A service backend override must not require an endpoint catalog."
  }
}

run "worker_revision_is_independent" {
  command = plan
  variables {
    worker_image = "us-west1-docker.pkg.dev/test-project/runtime/server@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
  }
  assert {
    condition     = google_cloud_run_v2_service.backend["worker"].template[0].containers[0].image == var.worker_image && google_cloud_run_v2_service.backend["legacy"].template[0].containers[0].image == var.server_image
    error_message = "Selecting a worker revision must not change the monolith revision."
  }
}

run "deployer_cannot_assume_migration_identity" {
  command = plan
  variables { deployer_service_account = "deployer@test-project.iam.gserviceaccount.com" }
  assert {
    condition     = length(google_service_account_iam_member.deployer) == 3 && !contains(keys(google_service_account_iam_member.deployer), "migrations") && !contains(keys(google_service_account_iam_member.deployer), "scheduler")
    error_message = "Application deployment must not gain migration or Scheduler identity privileges."
  }
}
