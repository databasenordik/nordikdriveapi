# Run with -var-file=tests/fixtures/small-db.tfvars.example.
# Only the Google provider is mocked; synthetic budget assumptions are evaluated.
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
}

run "candidate_requires_verified_consumers" {
  command         = plan
  expect_failures = [terraform_data.safeguards]
}

run "verified_candidate_fits_and_configures_every_pool" {
  command = plan
  variables { connection_budget_reviewed = true }
  assert {
    condition = (
      output.connection_budget.total_capacity == 25 &&
      output.connection_budget.superuser_reserved == 3 &&
      output.connection_budget.ordinary_connection_limit == 22 &&
      output.connection_budget.allocation_limit == 20 &&
      output.connection_budget.steady_runtime == 6 &&
      output.connection_budget.rollout_overlap == 6 &&
      output.connection_budget.cleanup == 1 &&
      output.connection_budget.existing_and_other_clients == 7 &&
      output.connection_budget.total_allocated == 20 &&
      output.connection_budget.remaining_allocation == 0
    )
    error_message = "The candidate must account for steady-state pools, one replacement per service, cleanup and the provisional seven-slot external allowance."
  }
  assert {
    condition = (
      one([for env in google_cloud_run_v2_service.gateway["preview"].template[0].containers[1].env : env.value if env.name == "DB_MAX_OPEN"]) == "1" &&
      one([for env in google_cloud_run_v2_service.backend["legacy"].template[0].containers[0].env : env.value if env.name == "DB_MAX_OPEN"]) == "3" &&
      one([for env in google_cloud_run_v2_service.backend["worker"].template[0].containers[0].env : env.value if env.name == "DB_MAX_OPEN"]) == "2" &&
      one([for env in google_cloud_run_v2_job.cleanup[0].template[0].template[0].containers[0].env : env.value if env.name == "DB_MAX_OPEN"]) == "1" &&
      google_cloud_run_v2_service.backend["worker"].template[0].max_instance_request_concurrency == 1
    )
    error_message = "All four process pools must receive the budgeted limits, with a single concurrent worker request."
  }
  assert {
    condition = (
      alltrue([for service in google_cloud_run_v2_service.backend : service.template[0].scaling[0].max_instance_count == 1 && service.scaling[0].max_instance_count == 1]) &&
      google_cloud_run_v2_service.gateway["preview"].scaling[0].max_instance_count == 1 &&
      google_cloud_run_v2_service.gateway["preview"].template[0].scaling[0].max_instance_count == 1 &&
      alltrue([for job in google_cloud_scheduler_job.business : job.paused]) &&
      google_cloud_scheduler_job.cleanup[0].paused &&
      length(google_cloud_run_v2_service_iam_member.public) == 0
    )
    error_message = "The candidate must remain private, scale to at most one configured instance per revision, and leave schedules paused."
  }
}

run "old_api_overlap_exceeds_capacity" {
  command = plan
  variables {
    connection_budget_reviewed = true
    other_reserved_connections = 25
  }
  expect_failures = [terraform_data.safeguards]
}

run "original_pools_do_not_fit_even_without_overlap" {
  command = plan
  variables {
    connection_budget_reviewed = true
    db_pool_limits             = { gateway = 4, legacy = 25, worker = 10, cleanup = 4 }
    rollout_extra_instances    = { gateway = 0, legacy = 0, worker = 0 }
    other_reserved_connections = 0
  }
  expect_failures = [terraform_data.safeguards]
}

run "additional_external_client_exceeds_capacity" {
  command = plan
  variables {
    connection_budget_reviewed = true
    other_reserved_connections = 8
  }
  expect_failures = [terraform_data.safeguards]
}

run "additional_steady_instance_exceeds_capacity" {
  command = plan
  variables {
    connection_budget_reviewed = true
    max_instances              = { gateway = 2, legacy = 1, worker = 1 }
  }
  expect_failures = [terraform_data.safeguards]
}

run "additional_rollout_instance_exceeds_capacity" {
  command = plan
  variables {
    connection_budget_reviewed = true
    rollout_extra_instances    = { gateway = 2, legacy = 1, worker = 1 }
  }
  expect_failures = [terraform_data.safeguards]
}

run "cleanup_pool_is_counted" {
  command = plan
  variables {
    connection_budget_reviewed = true
    db_pool_limits             = { gateway = 1, legacy = 3, worker = 2, cleanup = 2 }
  }
  expect_failures = [terraform_data.safeguards]
}

run "superuser_reservation_cannot_be_allocated" {
  command = plan
  variables {
    connection_budget_reviewed              = true
    database_superuser_reserved_connections = 7
  }
  expect_failures = [terraform_data.safeguards]
}

run "zero_pool_is_rejected" {
  command = plan
  variables {
    connection_budget_reviewed = true
    db_pool_limits             = { gateway = 0, legacy = 3, worker = 2, cleanup = 1 }
  }
  expect_failures = [var.db_pool_limits]
}

run "fractional_pool_is_rejected" {
  command = plan
  variables {
    connection_budget_reviewed = true
    db_pool_limits             = { gateway = 1, legacy = 2.5, worker = 2, cleanup = 1 }
  }
  expect_failures = [var.db_pool_limits]
}

run "negative_rollout_count_is_rejected" {
  command = plan
  variables {
    connection_budget_reviewed = true
    rollout_extra_instances    = { gateway = -1, legacy = 1, worker = 1 }
  }
  expect_failures = [var.rollout_extra_instances]
}

run "fractional_capacity_is_rejected" {
  command = plan
  variables {
    connection_budget_reviewed   = true
    database_connection_capacity = 25.5
  }
  expect_failures = [var.database_connection_capacity]
}

run "worker_concurrency_cannot_exhaust_lock_and_query_slots" {
  command = plan
  variables {
    connection_budget_reviewed = true
    worker_request_concurrency = 2
  }
  expect_failures = [var.worker_request_concurrency]
}
