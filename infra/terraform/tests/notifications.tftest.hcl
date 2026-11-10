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
  deployment_stage                        = "preview"
  iam_database_authentication_verified    = true
  runtime_prerequisites_verified          = true
  connection_budget_reviewed              = true
  project_id                              = "test-project"
  project_number                          = "123456789012"
  sql_instance_name                       = "fixture-instance"
  sql_connection_name                     = "test-project:us-west1:fixture-instance"
  database_name                           = "fixture"
  server_image                            = "us-west1-docker.pkg.dev/test-project/runtime/server@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  adapter_image                           = "us-west1-docker.pkg.dev/test-project/runtime/adapter@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
  notifications_image                     = "us-west1-docker.pkg.dev/test-project/runtime/nordik_notifications_go@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
  jwt_secret                              = { secret_id = "fixture-jwt", version = "1" }
  trusted_proxy_hops                      = 1
  db_pool_limits                          = { gateway = 1, legacy = 2, worker = 2, cleanup = 1 }
  max_instances                           = { gateway = 1, legacy = 1, worker = 1 }
  rollout_extra_instances                 = { gateway = 1, legacy = 1, worker = 1 }
  worker_request_concurrency              = 1
  database_connection_capacity            = 25
  database_superuser_reserved_connections = 3
  other_reserved_connections              = 6
  enable_notifications                    = true
  runtime_secrets = {
    notifications = {
      GMAIL_USER                = { secret_id = "fixture-smtp-user", version = "1" }
      GMAIL_APP_PASSWORD        = { secret_id = "fixture-smtp-password", version = "1" }
      NOTIFICATION_PAYLOAD_KEYS = { secret_id = "fixture-payload-keys", version = "1" }
    }
    legacy = {
      NOTIFICATION_PAYLOAD_KEYS = { secret_id = "fixture-payload-keys", version = "1" }
      SIGNUP_VERIFICATION_KEYS  = { secret_id = "fixture-auth-keys", version = "1" }
    }
    worker = { NOTIFICATION_PAYLOAD_KEYS = { secret_id = "fixture-payload-keys", version = "1" } }
  }
}
run "private_notification_runtime_fits_micro_database" {
  command = plan
  assert {
    condition     = output.connection_budget.total_allocated == 19 && output.connection_budget.remaining_allocation == 1
    error_message = "Notifications must include its serving and replacement connection in the unchanged 20-connection safeguard."
  }
  assert {
    condition = (
      google_cloud_run_v2_service.notifications[0].scaling[0].max_instance_count == 1 &&
      google_cloud_run_v2_service.notifications[0].template[0].scaling[0].min_instance_count == 0 &&
      google_cloud_run_v2_service.notifications[0].template[0].scaling[0].max_instance_count == 1 &&
      google_cloud_run_v2_service.notifications[0].template[0].max_instance_request_concurrency == 1 &&
      one([for env in google_cloud_run_v2_service.notifications[0].template[0].containers[0].env : env.value if env.name == "DB_MAX_OPEN"]) == "1" &&
      one([for env in google_cloud_run_v2_service.notifications[0].template[0].containers[0].env : env.value if env.name == "DB_SCHEMA"]) == "notifications"
    )
    error_message = "Notifications must scale from zero with a one-connection pool and its own schema."
  }
  assert {
    condition = (
      length(google_cloud_run_v2_service_iam_member.public) == 0 &&
      alltrue([for queue in google_cloud_tasks_queue.notifications : queue.desired_state == "PAUSED" && queue.rate_limits[0].max_concurrent_dispatches == 1]) &&
      google_cloud_scheduler_job.notification_dispatch[0].paused &&
      !contains(keys(google_sql_user.iam), "tasks") &&
      one([for env in google_cloud_run_v2_service.backend["legacy"].template[0].containers[0].env : env.value if env.name == "NOTIFICATION_DELIVERY_MODE"]) == "direct"
    )
    error_message = "Initial staging must keep queues/dispatch paused, producers unchanged, and the task identity without a database user."
  }
}
run "unsafe_capacity_is_rejected" {
  command = plan
  variables { other_reserved_connections = 8 }
  expect_failures = [terraform_data.safeguards]
}
run "producer_keys_are_required_for_cutover" {
  command = plan
  variables {
    notification_delivery_enabled = true
    runtime_secrets = {
      notifications = {
        GMAIL_USER                = { secret_id = "fixture-smtp-user", version = "1" }
        GMAIL_APP_PASSWORD        = { secret_id = "fixture-smtp-password", version = "1" }
        NOTIFICATION_PAYLOAD_KEYS = { secret_id = "fixture-payload-keys", version = "1" }
      }
    }
  }
  expect_failures = [terraform_data.notification_safeguards]
}
run "mandatory_signup_requires_outboxes" {
  command = plan
  variables { signup_require_verified_email = true }
  expect_failures = [terraform_data.notification_safeguards]
}
run "credential_values_cannot_be_plain_runtime_settings" {
  command = plan
  variables { runtime_settings = { notifications = { NOTIFICATION_PAYLOAD_KEYS = "not-a-secret-reference" } } }
  expect_failures = [terraform_data.notification_safeguards]
}
