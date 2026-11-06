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
  project_id                              = "test-project"
  sql_instance_name                       = "fixture-instance"
  sql_connection_name                     = "test-project:us-west1:fixture-instance"
  database_name                           = "fixture"
  server_image                            = "us-west1-docker.pkg.dev/test-project/runtime/server@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  adapter_image                           = "us-west1-docker.pkg.dev/test-project/runtime/adapter@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
  jwt_secret                              = { secret_id = "fixture-jwt", version = "1" }
  trusted_proxy_hops                      = 1
  database_connection_capacity            = 25
  database_superuser_reserved_connections = 3
  db_pool_limits                          = { gateway = 1, legacy = 3, worker = 2, cleanup = 1 }
  max_instances                           = { gateway = 1, legacy = 1, worker = 1 }
  rollout_extra_instances                 = { gateway = 1, legacy = 1, worker = 1 }
  worker_request_concurrency              = 1
  other_reserved_connections              = 7
  connection_budget_reviewed              = true
  iam_database_authentication_verified    = true
  runtime_prerequisites_verified          = true
}

run "bootstrap_needs_no_images_db_users_or_runtime_budget" {
  command = plan
  variables {
    deployment_stage                     = "bootstrap"
    server_image                         = null
    adapter_image                        = null
    trusted_proxy_hops                   = null
    other_reserved_connections           = 160
    connection_budget_reviewed           = false
    iam_database_authentication_verified = false
    runtime_prerequisites_verified       = false
  }
  assert {
    condition = (
      length(google_service_account.runtime) == 5 &&
      length(google_secret_manager_secret.runtime) == 1 &&
      length(google_sql_user.iam) == 0 &&
      length(google_cloud_run_v2_service.backend) == 0 &&
      length(google_cloud_run_v2_service.gateway) == 0 &&
      length(google_cloud_run_v2_job.cleanup) == 0 &&
      length(google_cloud_scheduler_job.business) == 0 &&
      length(google_cloud_scheduler_job.cleanup) == 0
    )
    error_message = "Bootstrap must create metadata and IAM only, without requiring placeholder images, SQL IAM users or a fabricated runtime budget."
  }
}

run "database_stage_registers_users_without_starting_runtime" {
  command = plan
  variables {
    deployment_stage               = "database"
    server_image                   = null
    adapter_image                  = null
    trusted_proxy_hops             = null
    connection_budget_reviewed     = false
    runtime_prerequisites_verified = false
  }
  assert {
    condition = (
      length(google_sql_user.iam) == 4 &&
      length(google_cloud_run_v2_service.backend) == 0 &&
      length(google_cloud_run_v2_service.gateway) == 0 &&
      length(google_cloud_scheduler_job.business) == 0
    )
    error_message = "The database stage must expose IAM usernames for grants while keeping all application revisions absent."
  }
}

run "database_stage_requires_verified_iam_authentication" {
  command = plan
  variables {
    deployment_stage                     = "database"
    iam_database_authentication_verified = false
  }
  expect_failures = [terraform_data.safeguards]
}

run "preview_requires_migrations_and_secret_versions" {
  command = plan
  variables {
    deployment_stage               = "preview"
    runtime_prerequisites_verified = false
  }
  expect_failures = [terraform_data.safeguards]
}

run "schedules_require_old_scheduling_retirement" {
  command = plan
  variables {
    deployment_stage                   = "preview"
    enable_schedules                   = true
    legacy_scheduling_retired_verified = false
  }
  expect_failures = [terraform_data.safeguards]
}

run "preview_cannot_receive_public_invoker_grant" {
  command = plan
  variables {
    deployment_stage = "preview"
    public_gateway   = true
  }
  expect_failures = [terraform_data.safeguards]
}

run "public_cutover_keeps_preview_and_pins_old_traffic" {
  command = plan
  variables {
    deployment_stage             = "cutover"
    database_connection_capacity = 100
    public_gateway_service_name  = "existing-api"
    public_serving_revision      = "existing-api-00001-old"
    public_gateway               = true
    preview_invoker_members      = ["user:tester@example.com"]
  }
  assert {
    condition = (
      length(google_cloud_run_v2_service.gateway) == 2 &&
      google_cloud_run_v2_service.gateway["preview"].name == "nordikdriveapi-gateway" &&
      google_cloud_run_v2_service.gateway["public"].name == "existing-api" &&
      google_cloud_run_v2_service_iam_member.public[0].name == "existing-api" &&
      google_cloud_run_v2_service_iam_member.preview_testers["user:tester@example.com"].name == "nordikdriveapi-gateway"
    )
    error_message = "Preview and imported public services must retain independent state addresses and invocation policies."
  }
  assert {
    condition = (
      one([for target in google_cloud_run_v2_service.gateway["public"].traffic : target.percent if target.type == "TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION"]) == 100 &&
      one([for target in google_cloud_run_v2_service.gateway["public"].traffic : target.revision if target.type == "TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION"]) == "existing-api-00001-old" &&
      one([for target in google_cloud_run_v2_service.gateway["public"].traffic : target.percent if target.type == "TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST"]) == 0 &&
      one([for target in google_cloud_run_v2_service.gateway["public"].traffic : target.tag if target.type == "TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST"]) == "milestone2"
    )
    error_message = "Staging a public gateway must preserve the compatible serving revision at 100% and expose the candidate only under its 0% tag."
  }
  assert {
    condition     = output.connection_budget.gateway_deployments == 2 && output.connection_budget.total_allocated == 22
    error_message = "The budget must count both the retained preview and public gateway plus their replacement allocations."
  }
}

run "cutover_cannot_hide_the_retained_preview_pool" {
  command = plan
  variables {
    deployment_stage            = "cutover"
    public_gateway_service_name = "existing-api"
    public_serving_revision     = "existing-api-00001-old"
  }
  expect_failures = [terraform_data.safeguards]
}

run "single_public_gateway_needs_no_preview_service_or_tester_grants" {
  command = plan
  variables {
    deployment_stage            = "cutover"
    enable_preview_gateway      = false
    public_gateway_service_name = "existing-api"
    public_serving_revision     = "existing-api-00001-maintenance"
    db_pool_limits              = { gateway = 1, legacy = 2, worker = 2, cleanup = 1 }
    preview_invoker_members     = ["user:unused@example.com"]
    other_reserved_connections  = 6
  }
  assert {
    condition = (
      length(google_cloud_run_v2_service.gateway) == 1 &&
      contains(keys(google_cloud_run_v2_service.gateway), "public") &&
      length(google_cloud_run_v2_service_iam_member.preview_testers) == 0 &&
      output.connection_budget.gateway_deployments == 1 &&
      output.connection_budget.total_allocated == 17 &&
      one([for target in google_cloud_run_v2_service.gateway["public"].traffic : target.percent if target.type == "TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION"]) == 100 &&
      one([for target in google_cloud_run_v2_service.gateway["public"].traffic : target.percent if target.type == "TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST"]) == 0
    )
    error_message = "The minimal rollout must create only the imported gateway, retain maintenance traffic and omit private preview grants."
  }
}

run "preview_stage_without_a_gateway_is_rejected" {
  command = plan
  variables {
    deployment_stage       = "preview"
    enable_preview_gateway = false
  }
  expect_failures = [terraform_data.safeguards]
}

run "synthetic_cutover_with_seven_external_slots_fits_twenty" {
  command = plan
  variables {
    deployment_stage            = "cutover"
    public_gateway_service_name = "existing-api"
    public_serving_revision     = "existing-api-00001-old"
    db_pool_limits              = { gateway = 1, legacy = 2, worker = 2, cleanup = 1 }
  }
  assert {
    condition = (
      output.connection_budget.gateway_deployments == 2 &&
      output.connection_budget.steady_runtime == 6 &&
      output.connection_budget.rollout_overlap == 6 &&
      output.connection_budget.cleanup == 1 &&
      output.connection_budget.existing_and_other_clients == 7 &&
      output.connection_budget.total_allocated == 20
    )
    error_message = "The synthetic fixture must include both gateways, replacements, cleanup and seven assumed external slots within 20; it is not live capacity verification."
  }
}

run "cutover_counts_bounded_transition_while_legacy_updates_are_frozen" {
  command = plan
  variables {
    deployment_stage            = "cutover"
    public_gateway_service_name = "existing-api"
    public_serving_revision     = "existing-api-00001-bounded"
    db_pool_limits              = { gateway = 1, legacy = 2, worker = 2, cleanup = 1 }
    other_reserved_connections  = 9
    rollout_extra_instances     = { gateway = 1, legacy = 0, worker = 1 }
  }
  assert {
    condition     = output.connection_budget.total_allocated == 20
    error_message = "The old bounded two-connection API must remain counted while it serves traffic; freeze legacy replacements to make room."
  }
}

run "public_promotion_pins_the_verified_revision" {
  command = plan
  variables {
    deployment_stage             = "cutover"
    database_connection_capacity = 100
    public_gateway_service_name  = "existing-api"
    public_serving_revision      = "existing-api-00002-gateway"
    stage_public_candidate       = false
  }
  assert {
    condition = (
      length(google_cloud_run_v2_service.gateway["public"].traffic) == 1 &&
      one(google_cloud_run_v2_service.gateway["public"].traffic).percent == 100 &&
      one(google_cloud_run_v2_service.gateway["public"].traffic).revision == "existing-api-00002-gateway"
    )
    error_message = "Promotion must target the verified revision explicitly rather than an unrelated future latest revision."
  }
}

run "cutover_rejects_revision_from_another_service" {
  command = plan
  variables {
    deployment_stage             = "cutover"
    database_connection_capacity = 100
    public_gateway_service_name  = "existing-api"
    public_serving_revision      = "another-service-00001-old"
  }
  expect_failures = [terraform_data.safeguards]
}

run "invalid_rollout_stage_is_rejected" {
  command = plan
  variables { deployment_stage = "unexpected" }
  expect_failures = [var.deployment_stage]
}

run "preview_and_public_cannot_manage_the_same_service" {
  command = plan
  variables {
    deployment_stage             = "cutover"
    database_connection_capacity = 100
    public_gateway_service_name  = "nordikdriveapi-gateway"
    public_serving_revision      = "nordikdriveapi-gateway-00001-old"
  }
  expect_failures = [terraform_data.safeguards]
}
