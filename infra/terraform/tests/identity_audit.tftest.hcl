mock_provider "google" {
  mock_data "google_cloud_run_v2_service" {
    override_during = plan
    defaults        = { annotations = { "nordikdrive.io/runtime-rollout" = "{\"version\":1,\"verified\":true,\"state\":\"held\",\"token\":\"fixture-window\"}" } }
  }
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
  deployment_stage                        = "cutover"
  enable_preview_gateway                  = false
  public_gateway_service_name             = "nordikdriveapi"
  public_serving_revision                 = "nordikdriveapi-existing"
  enable_signup_cleanup                   = true
  serialized_runtime_rollouts             = true
  rollout_inspection_enabled              = true
  rollout_deployer_account                = "deployer@test-project.iam.gserviceaccount.com"
  rollout_window_token                    = "fixture-window"
  identity_image                          = "us-west1-docker.pkg.dev/test-project/runtime/nordik_identity_go@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
  audit_image                             = "us-west1-docker.pkg.dev/test-project/runtime/nordik_audit_go@sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
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
  notification_delivery_enabled           = true
  enable_notification_source              = true
  enable_event_driven_notifications       = true
  signup_require_verified_email           = true
  runtime_secrets = {
    identity = {
      NOTIFICATION_PAYLOAD_KEYS = { secret_id = "fixture-payload-keys", version = "1" }
      SIGNUP_VERIFICATION_KEYS  = { secret_id = "fixture-auth-keys", version = "1" }
    }
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

run "serialized_m3_keeps_capacity_and_worker_pool" {
  command = plan
  assert {
    condition     = output.connection_budget.total_allocated == 16 && output.connection_budget.rollout_overlap == 2 && one([for env in google_cloud_run_v2_service.backend["worker"].template[0].containers[0].env : env.value if env.name == "DB_MAX_OPEN"]) == "2" && contains(google_cloud_run_v2_service.backend["worker"].custom_audiences, local.worker_url)
    error_message = "M3 must allocate 16 with one replacement allowance and preserve the two-connection Worker."
  }
}
run "identity_adds_one_private_connection" {
  command = plan
  variables { enable_identity = true }
  assert {
    condition     = output.connection_budget.total_allocated == 17 && google_cloud_run_v2_service.owner["identity"].scaling[0].max_instance_count == 1 && google_cloud_run_v2_service.owner["identity"].template[0].scaling[0].min_instance_count == 0 && google_cloud_run_v2_service.owner["identity"].template[0].containers[0].ports[0].name == "h2c" && one([for env in google_cloud_run_v2_service.owner["identity"].template[0].containers[0].env : env.value if env.name == "DB_MAX_OPEN"]) == "1"
    error_message = "Identity must use one private scale-to-zero runtime and one shared HTTP/2 listener."
  }
}
run "audit_adds_one_private_connection_and_separate_queue_identity" {
  command = plan
  variables {
    enable_identity           = true
    enable_identity_consumers = true
    enable_audit              = true
  }
  assert {
    condition     = output.connection_budget.total_allocated == 18 && google_cloud_tasks_queue.audit[0].desired_state == "PAUSED" && !contains(keys(google_sql_user.iam), "audit-tasks") && length(local.secrets.audit) == 1 && local.secrets.audit.JWT_SECRET == var.jwt_secret
    error_message = "Audit must fit 18, use a paused separate task queue and receive only the existing JWT secret."
  }
}
run "identity_reuses_single_finite_signup_cleanup_job" {
  command = plan
  variables {
    enable_identity           = true
    enable_identity_consumers = true
    identity_public_enabled   = true
    identity_signup_cleanup   = true
    identity_outbox_exclusive = true
    identity_tables_relocated = true
  }
  assert {
    condition     = length(google_cloud_run_v2_job.signup_cleanup) == 1 && google_cloud_run_v2_job.signup_cleanup[0].template[0].template[0].containers[0].image == var.identity_image && google_cloud_run_v2_job.signup_cleanup[0].template[0].template[0].containers[0].args == tolist(["signup-cleanup"]) && one([for env in google_cloud_run_v2_job.signup_cleanup[0].template[0].template[0].containers[0].env : env.value if env.name == "DB_SCHEMA"]) == "identity"
    error_message = "Transfer the one existing finite cleanup job to Identity; do not add a cleanup API/job."
  }
}
run "independent_overlap_is_rejected_before_identity" {
  command = plan
  variables {
    serialized_runtime_rollouts = false
    enable_identity             = true
  }
  expect_failures = [terraform_data.identity_audit_safeguards, terraform_data.safeguards]
}
run "audit_cannot_precede_identity_consumers" {
  command = plan
  variables {
    enable_identity = true
    enable_audit    = true
  }
  expect_failures = [terraform_data.identity_audit_safeguards]
}
run "identity_relocation_requires_cleanup_and_drain_cutover" {
  command = plan
  variables {
    enable_identity           = true
    identity_tables_relocated = true
  }
  expect_failures = [terraform_data.identity_audit_safeguards]
}
run "audit_cannot_receive_smtp_or_payload_keys" {
  command = plan
  variables {
    runtime_secrets = {
      audit         = { GMAIL_APP_PASSWORD = { secret_id = "wrong-audit-smtp", version = "1" } }
      notifications = { GMAIL_APP_PASSWORD = { secret_id = "smtp", version = "1" }, NOTIFICATION_PAYLOAD_KEYS = { secret_id = "payload", version = "1" } }
      legacy        = { NOTIFICATION_PAYLOAD_KEYS = { secret_id = "payload", version = "1" }, SIGNUP_VERIFICATION_KEYS = { secret_id = "auth", version = "1" } }
      worker        = { NOTIFICATION_PAYLOAD_KEYS = { secret_id = "payload", version = "1" } }
    }
  }
  expect_failures = [terraform_data.identity_audit_safeguards]
}
run "terraform_cannot_use_another_rollout_window" {
  command = plan
  variables { rollout_window_token = "wrong-window" }
  expect_failures = [terraform_data.serialized_rollout_safeguards]
}
run "serialized_rollout_requires_inspection" {
  command = plan
  variables { rollout_inspection_enabled = false }
  expect_failures = [terraform_data.serialized_rollout_safeguards]
}
run "audit_reconciliation_uses_twelve_hours" {
  command = plan
  variables {
    enable_identity           = true
    enable_identity_consumers = true
    identity_public_enabled   = true
    enable_audit              = true
    enable_audit_events       = true
    audit_queues_enabled      = true
  }
  assert {
    condition     = google_cloud_scheduler_job.audit_dispatch[0].schedule == "35 */12 * * *" && google_cloud_tasks_queue.audit[0].desired_state == "RUNNING" && google_cloud_tasks_queue.audit[0].http_target[0].uri_override[0].path_override[0].path == "/internal/v1/audit/ingest"
    error_message = "Audit uses immediate ID-only tasks with automatic twelve-hour fallback reconciliation and a fixed task target."
  }
}

run "identity_requires_accepted_m3_delivery" {
  command = plan
  variables {
    enable_identity                   = true
    enable_event_driven_notifications = false
  }
  expect_failures = [terraform_data.identity_audit_safeguards]
}

run "worker_configuration_is_staged_without_moving_traffic" {
  command = plan
  override_data {
    target = data.google_cloud_run_v2_service.rollout_marker[0]
    values = { annotations = { "nordikdrive.io/runtime-rollout" = "{\"version\":1,\"verified\":true,\"state\":\"held\",\"token\":\"fixture-window\",\"target\":\"worker\",\"revision\":\"nordikdriveapi-worker-m4-fixture\"}" } }
  }
  variables {
    runtime_serving_revisions   = { worker = "nordikdriveapi-worker-accepted" }
    runtime_candidate_revisions = { worker = "nordikdriveapi-worker-m4-fixture" }
  }
  assert {
    condition     = google_cloud_run_v2_service.backend["worker"].template[0].revision == "nordikdriveapi-worker-m4-fixture" && length([for route in google_cloud_run_v2_service.backend["worker"].traffic : route if route.revision == "nordikdriveapi-worker-accepted" && route.percent == 100]) == 1 && length([for route in google_cloud_run_v2_service.backend["worker"].traffic : route if route.revision == "nordikdriveapi-worker-m4-fixture" && route.percent == 0 && route.tag == "m4-candidate"]) == 1
    error_message = "Configuration changes must preserve serving traffic while staging the exact candidate."
  }
}

run "promoted_template_name_is_retained" {
  command = plan
  variables {
    runtime_serving_revisions  = { worker = "nordikdriveapi-worker-m4-fixture" }
    runtime_template_revisions = { worker = "nordikdriveapi-worker-m4-fixture" }
  }
  assert {
    condition     = google_cloud_run_v2_service.backend["worker"].template[0].revision == "nordikdriveapi-worker-m4-fixture" && length(google_cloud_run_v2_service.backend["worker"].traffic) == 1
    error_message = "Removing the candidate tag must retain the promoted template revision."
  }
}
