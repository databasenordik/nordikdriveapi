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
  mock_resource "google_cloud_run_v2_job" {
    override_during = plan
    defaults        = { id = "projects/test-project/locations/us-west1/jobs/fixture" }
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

run "signup_cleanup_is_a_secret_free_finite_job" {
  command = plan
  variables { enable_signup_cleanup = true }
  assert {
    condition     = output.connection_budget.total_allocated == 20 && output.connection_budget.signup_cleanup == 1
    error_message = "The auth cleanup connection must be included in the unchanged micro-database budget."
  }
  assert {
    condition = (
      google_cloud_run_v2_job.signup_cleanup[0].template[0].task_count == 1 &&
      google_cloud_run_v2_job.signup_cleanup[0].template[0].parallelism == 1 &&
      google_cloud_run_v2_job.signup_cleanup[0].template[0].template[0].service_account == google_service_account.runtime["legacy"].email &&
      google_cloud_run_v2_job.signup_cleanup[0].template[0].template[0].containers[0].image == var.server_image &&
      one(google_cloud_run_v2_job.signup_cleanup[0].template[0].template[0].containers[0].args) == "signup-cleanup" &&
      one([for env in google_cloud_run_v2_job.signup_cleanup[0].template[0].template[0].containers[0].env : env.value if env.name == "DB_MAX_OPEN"]) == "1" &&
      alltrue([for env in google_cloud_run_v2_job.signup_cleanup[0].template[0].template[0].containers[0].env : !contains(["JWT_SECRET", "GMAIL_APP_PASSWORD", "NOTIFICATION_PAYLOAD_KEYS", "SIGNUP_VERIFICATION_KEYS"], env.name)])
    )
    error_message = "Cleanup must use the existing server command/legacy identity with one connection and no application secrets."
  }
  assert {
    condition = (
      google_cloud_scheduler_job.signup_cleanup[0].schedule == "17 * * * *" &&
      google_cloud_scheduler_job.signup_cleanup[0].paused &&
      startswith(google_cloud_scheduler_job.signup_cleanup[0].http_target[0].uri, "https://run.googleapis.com/v2/") &&
      endswith(google_cloud_scheduler_job.signup_cleanup[0].http_target[0].uri, ":run") &&
      google_cloud_scheduler_job.signup_cleanup[0].http_target[0].oauth_token[0].service_account_email == google_service_account.runtime["scheduler"].email
    )
    error_message = "Scheduler must launch the finite job through Cloud Run, not an application cleanup API."
  }
}

run "signup_cleanup_cannot_overallocate_connections" {
  command = plan
  variables {
    enable_signup_cleanup      = true
    other_reserved_connections = 7
  }
  expect_failures = [terraform_data.safeguards]
}

run "signup_cleanup_requires_its_schema" {
  command = plan
  variables {
    enable_notifications  = false
    enable_signup_cleanup = true
  }
  expect_failures = [terraform_data.signup_cleanup_safeguards]
}

run "notification_source_can_precede_producer_cutover" {
  command = plan
  variables { enable_notification_source = true }
  assert {
    condition = (
      length(google_cloud_run_v2_service_iam_member.notification_source_reader) == 1 &&
      length(google_cloud_tasks_queue_iam_member.notification_event_producer) == 0 &&
      length(google_cloud_tasks_queue.file_events) == 0 &&
      !contains([for env in google_cloud_run_v2_service.backend["worker"].template[0].containers[0].env : env.name], "NOTIFICATION_OUTBOX_SOURCE_URL") &&
      one([for env in google_cloud_run_v2_service.notifications[0].template[0].containers[0].env : env.value if env.name == "NOTIFICATION_OUTBOX_SOURCE_URL"]) == local.notification_outbox_source_url
    )
    error_message = "Private source/receipts must be deployable before publishers without creating another email queue."
  }
}

run "event_delivery_requires_its_source" {
  command = plan
  variables { enable_event_driven_notifications = true }
  expect_failures = [terraform_data.notification_event_safeguards]
}

run "fallback_frequency_cannot_change_before_events" {
  command = plan
  variables { enable_low_frequency_reconciliation = true }
  expect_failures = [terraform_data.notification_event_safeguards]
}

run "events_do_not_add_database_pools_or_broaden_task_targets" {
  command = plan
  variables {
    enable_notification_source        = true
    enable_event_driven_notifications = true
    notification_delivery_enabled     = true
    enable_file_version_events        = true
  }
  assert {
    condition = (
      output.connection_budget.total_allocated == 19 &&
      length(google_cloud_tasks_queue.notifications) == 2 &&
      length(google_cloud_tasks_queue_iam_member.notification_event_producer) == 4 &&
      !contains(keys(google_sql_user.iam), "work-tasks") &&
      google_cloud_tasks_queue.file_events[0].http_target[0].uri_override[0].path_override[0].path == "/internal/v1/work/file-version" &&
      google_cloud_tasks_queue.file_events[0].http_target[0].uri_override[0].uri_override_enforce_mode == "ALWAYS" &&
      google_cloud_tasks_queue.file_events[0].rate_limits[0].max_concurrent_dispatches == 1 &&
      google_cloud_scheduler_job.business["file-reconciliation"].schedule == "* * * * *" &&
      google_cloud_scheduler_job.business["file-reconciliation"].http_target[0].oidc_token[0].audience == local.file_version_worker_url &&
      one([for env in google_cloud_run_v2_service.backend["worker"].template[0].containers[0].env : env.value if env.name == "WORKER_SCHEDULER_ACCOUNT"]) == google_service_account.runtime["scheduler"].email
    )
    error_message = "Event cutover must reuse pools, restrict task paths, identify Scheduler separately, and retain fallback scans until accepted."
  }
}

run "accepted_events_allow_six_and_twelve_hour_scans" {
  command = plan
  variables {
    enable_signup_cleanup               = true
    enable_notification_source          = true
    enable_event_driven_notifications   = true
    notification_delivery_enabled       = true
    enable_file_version_events          = true
    enable_low_frequency_reconciliation = true
  }
  assert {
    condition = (
      google_cloud_scheduler_job.business["file-normalization"].schedule == "15 */6 * * *" &&
      google_cloud_scheduler_job.business["file-reconciliation"].schedule == "20 */12 * * *" &&
      google_cloud_scheduler_job.business["file-review-email"].schedule == "5 */12 * * *" &&
      google_cloud_scheduler_job.business["form-review-email"].schedule == "10 */12 * * *" &&
      google_cloud_scheduler_job.notification_dispatch[0].schedule == "0 */12 * * *" &&
      google_cloud_scheduler_job.cleanup[0].schedule == "25 */12 * * *" &&
      google_cloud_scheduler_job.signup_cleanup[0].schedule == "30 */12 * * *" &&
      google_cloud_scheduler_job.business["daily-honour"].schedule == "0 0 * * *" &&
      google_cloud_scheduler_job.business["daily-honour"].time_zone == "America/Toronto" &&
      google_cloud_scheduler_job.business["support-maintenance"].schedule == "0 0 * * *" &&
      google_cloud_scheduler_job.business["file-normalization"].time_zone == "Etc/UTC" &&
      output.connection_budget.total_allocated == 20
    )
    error_message = "Only accepted event paths may precede the requested six-hour normalization and twelve-hour reconciliation/cleanup cadence."
  }
}
