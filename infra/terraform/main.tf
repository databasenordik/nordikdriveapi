locals {

  database_enabled = var.deployment_stage != "bootstrap"
  runtime_enabled  = contains(["preview", "cutover"], var.deployment_stage)
  gateway_deployments = local.runtime_enabled ? merge(
    var.enable_preview_gateway ? { preview = coalesce(var.gateway_service_name, "${var.name_prefix}-gateway") } : {},
    var.deployment_stage == "cutover" && var.public_gateway_service_name != null ? { public = var.public_gateway_service_name } : {},
  ) : {}
  gateway_deployment_count = length(local.gateway_deployments)
  roles                    = setunion(toset(["gateway", "legacy", "worker", "scheduler", "migrations"]), var.enable_notifications ? toset(["notifications", "tasks"]) : toset([]), var.enable_file_version_events ? toset(["work-tasks"]) : toset([]))
  sql_owners               = setunion(toset(["gateway", "legacy", "worker", "migrations"]), var.enable_notifications ? toset(["notifications"]) : toset([]))
  runtime_owners           = setunion(toset(["gateway", "legacy", "worker"]), var.enable_notifications ? toset(["notifications"]) : toset([]))
  pools = {
    gateway = var.db_pool_limits.gateway
    legacy  = var.db_pool_limits.legacy
    worker  = var.db_pool_limits.worker
  }
  runtime_connections   = sum([for owner, pool in local.pools : pool * var.max_instances[owner] * (owner == "gateway" ? local.gateway_deployment_count : 1)]) + local.notification_connections
  rollout_connections   = sum([for owner, pool in local.pools : pool * var.rollout_extra_instances[owner] * (owner == "gateway" ? local.gateway_deployment_count : 1)]) + local.notification_connections
  allocated_connections = local.runtime_connections + local.rollout_connections + var.db_pool_limits.cleanup + local.signup_cleanup_connections + var.other_reserved_connections
  connection_budget_limit = min(
    floor(var.database_connection_capacity * 0.8),
    var.database_connection_capacity - var.database_superuser_reserved_connections,
  )
  secrets = merge({

    gateway = merge(lookup(var.runtime_secrets, "gateway", {}), {
      JWT_SECRET = var.jwt_secret
    })
    legacy = merge(lookup(var.runtime_secrets, "legacy", {}), {
      JWT_SECRET = var.jwt_secret
    })
    worker = lookup(var.runtime_secrets, "worker", {})
  }, var.enable_notifications ? { notifications = lookup(var.runtime_secrets, "notifications", {}) } : {})
  secret_pairs = {
    for item in flatten([for owner, secrets in local.secrets : [for env, secret in secrets : merge(secret, {
      owner = owner, env = env
    })]]) : "${item.owner}:${item.env}" => item
  }
  secret_ids = toset([for item in values(local.secret_pairs) : item.secret_id])
  schedules = {

    file-review-email   = var.enable_low_frequency_reconciliation ? "5 */12 * * *" : "*/5 * * * *"
    form-review-email   = var.enable_low_frequency_reconciliation ? "10 */12 * * *" : "*/5 * * * *"
    file-normalization  = var.enable_low_frequency_reconciliation ? "15 */6 * * *" : "*/10 * * * *"
    file-reconciliation = var.enable_low_frequency_reconciliation ? "20 */12 * * *" : "* * * * *"
    daily-honour        = "0 0 * * *"
    support-maintenance = "0 0 * * *"

  }
  common_env = {
    DB_AUTH_MODE = "iam", CLOUD_SQL_INSTANCE = var.sql_connection_name, CLOUD_SQL_IP_TYPE = var.sql_ip_type, DB_NAME = var.database_name, GOOGLE_CLOUD_PROJECT = var.project_id, APP_TIMEZONE = "America/Toronto", OTEL_EXPORTER = "gcp"
  }

}

resource "terraform_data" "safeguards" {

  lifecycle {

    precondition {
      condition     = !local.runtime_enabled || length(local.gateway_deployments) > 0
      error_message = "Runtime stages need a gateway: enable the optional preview or use cutover with the existing public service."
    }

    precondition {

      condition     = !local.runtime_enabled || local.allocated_connections <= local.connection_budget_limit
      error_message = "Runtime pools, rollout overlap, cleanup and other consumers exceed the smaller of the 80% budget and non-superuser connection capacity."

    }
    precondition {

      condition     = !local.runtime_enabled || var.connection_budget_reviewed
      error_message = "Review the connection allocation, including controlled runtime limits, old revisions and the stated allowance for unchanged clients. Observed activity is not an enforced limit."

    }
    precondition {

      condition     = !local.database_enabled || var.iam_database_authentication_verified
      error_message = "Enable and verify Cloud SQL IAM database authentication before the database, preview or cutover stage."

    }
    precondition {

      condition     = !local.runtime_enabled || var.runtime_prerequisites_verified
      error_message = "Apply the platform migrations/grants and verify numeric secret versions and runtime settings before creating application revisions."

    }
    precondition {

      condition     = !local.runtime_enabled || (var.server_image != null && var.adapter_image != null && var.trusted_proxy_hops != null)
      error_message = "Runtime stages require immutable server/adapter images and a verified trusted proxy hop count."

    }
    precondition {

      condition     = !var.enable_schedules || (local.runtime_enabled && var.legacy_scheduling_retired_verified)
      error_message = "External schedules require runtime services plus verified retirement of old cron revisions and the honour-daily trigger."

    }
    precondition {

      condition     = !var.public_gateway || var.deployment_stage == "cutover"
      error_message = "Only the cutover-stage public gateway may receive an allUsers invoker grant; the preview remains private."

    }
    precondition {

      condition     = var.deployment_stage != "cutover" || (var.public_gateway_service_name != null && try(startswith(var.public_serving_revision, "${var.public_gateway_service_name}-"), false))
      error_message = "Cutover requires the existing public service name and an explicit currently serving revision belonging to that service. Import the public resource before applying."

    }
    precondition {

      condition     = var.public_gateway_service_name == null || coalesce(var.gateway_service_name, "${var.name_prefix}-gateway") != var.public_gateway_service_name
      error_message = "Preview and public gateway names must differ; two Terraform addresses must never manage the same Cloud Run service."

    }
    precondition {

      condition     = var.sql_ip_type != "private" || (var.vpc_network != null && var.vpc_subnetwork != null)
      error_message = "Private SQL requires a verified Direct VPC network/subnetwork."

    }
    precondition {

      condition     = alltrue(flatten([for owner, env in var.runtime_settings : [for key in keys(env) : !contains(["APP_ROLE", "DB_AUTH_MODE", "DB_USER", "DB_PASSWORD", "DB_MAX_OPEN", "DB_NAME", "CLOUD_SQL_INSTANCE", "CLOUD_SQL_IP_TYPE", "SCHEDULER_MODE", "WORKER_SCHEDULER_PREFIX", "JWT_SECRET", "GMAIL_APP_PASSWORD", "ZOOM_CLIENT_SECRET"], key)]]))
      error_message = "runtime_settings cannot override ownership/auth settings or carry credentials. Use runtime_secrets for protected values."

    }

  }

}

resource "google_project_service" "apis" {

  for_each           = setunion(toset(["run.googleapis.com", "sqladmin.googleapis.com", "secretmanager.googleapis.com", "cloudscheduler.googleapis.com", "iamcredentials.googleapis.com", "cloudtrace.googleapis.com", "monitoring.googleapis.com"]), var.enable_notifications || var.enable_file_version_events ? toset(["cloudtasks.googleapis.com"]) : toset([]))
  service            = each.key
  disable_on_destroy = false

}
resource "google_service_account" "runtime" {

  for_each     = local.roles
  account_id   = "${var.name_prefix}-${each.key}"
  display_name = "Nordik ${each.key}"

}
resource "google_project_iam_member" "sql" {

  for_each = {
    for item in setproduct(local.sql_owners, ["roles/cloudsql.client", "roles/cloudsql.instanceUser"]) : "${item[0]}:${item[1]}" => item
  }
  project = var.project_id
  role    = each.value[1]
  member  = "serviceAccount:${google_service_account.runtime[each.value[0]].email}"

}
resource "google_sql_user" "iam" {

  for_each   = local.database_enabled ? local.sql_owners : toset([])
  name       = trimsuffix(google_service_account.runtime[each.key].email, ".gserviceaccount.com")
  instance   = var.sql_instance_name
  type       = "CLOUD_IAM_SERVICE_ACCOUNT"
  depends_on = [terraform_data.safeguards, google_project_service.apis, google_project_iam_member.sql]
  lifecycle { prevent_destroy = true }

}
resource "google_project_iam_member" "telemetry" {

  for_each = {
    for item in setproduct(local.runtime_owners, ["roles/cloudtrace.agent", "roles/monitoring.metricWriter"]) : "${item[0]}:${item[1]}" => item
  }
  project = var.project_id
  role    = each.value[1]
  member  = "serviceAccount:${google_service_account.runtime[each.value[0]].email}"

}
resource "google_project_iam_member" "vertex" {

  for_each = toset(["legacy", "worker"])
  project  = var.project_id
  role     = "roles/aiplatform.user"
  member   = "serviceAccount:${google_service_account.runtime[each.key].email}"

}
resource "google_storage_bucket_iam_member" "objects" {

  for_each = {
    for item in flatten([for owner, buckets in var.buckets_by_owner : [for bucket in buckets : {
      owner = owner, bucket = bucket
    }]]) : "${item.owner}:${item.bucket}" => item
  }
  bucket = each.value.bucket
  role   = "roles/storage.objectAdmin"
  member = "serviceAccount:${google_service_account.runtime[each.value.owner].email}"

}
resource "google_secret_manager_secret" "runtime" {

  for_each  = local.secret_ids
  secret_id = each.key
  replication {
    auto {}
  }
  depends_on = [google_project_service.apis]
  lifecycle { prevent_destroy = true }

}
resource "google_secret_manager_secret_iam_member" "runtime" {

  for_each  = local.secret_pairs
  secret_id = google_secret_manager_secret.runtime[each.value.secret_id].id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.runtime[each.value.owner].email}"

}

resource "google_cloud_run_v2_service" "backend" {

  for_each            = local.runtime_enabled ? toset(["legacy", "worker"]) : toset([])
  name                = "${var.name_prefix}-${each.key}"
  location            = var.region
  deletion_protection = true
  ingress             = "INGRESS_TRAFFIC_ALL"
  custom_audiences = each.key == "legacy" && var.enable_notification_source ? [local.notification_outbox_source_url] : (
    each.key == "worker" && var.enable_file_version_events ? [local.file_version_worker_url] : []
  )
  scaling {
    max_instance_count = var.max_instances[each.key]
  }
  template {

    service_account                  = google_service_account.runtime[each.key].email
    timeout                          = each.key == "worker" ? "930s" : "330s"
    max_instance_request_concurrency = each.key == "worker" ? var.worker_request_concurrency : 40
    scaling {

      min_instance_count = 0
      max_instance_count = var.max_instances[each.key]

    }
    dynamic "vpc_access" {

      for_each = var.sql_ip_type == "private" ? [1] : []
      content {

        egress = "PRIVATE_RANGES_ONLY"
        network_interfaces {

          network    = var.vpc_network
          subnetwork = var.vpc_subnetwork

        }

      }

    }
    containers {

      image = each.key == "worker" ? coalesce(var.worker_image, var.server_image) : var.server_image
      ports {
        container_port = 8080
      }
      resources {

        limits = {
          cpu = "1", memory = "512Mi"
        }
        cpu_idle = true

      }
      dynamic "env" {

        for_each = merge(lookup(var.runtime_settings, each.key, {}), local.common_env, local.notification_producer_env, local.notification_source_env, local.notification_event_env, local.file_version_event_env, var.enable_notifications ? { SIGNUP_REQUIRE_VERIFIED_EMAIL = tostring(each.key == "legacy" && var.signup_require_verified_email) } : {}, {

          APP_ROLE                  = each.key == "worker" ? "worker" : "monolith"
          SCHEDULER_MODE            = "external"
          WORKER_SCHEDULER_PREFIX   = "projects/${var.project_id}/locations/${var.region}/jobs"
          DB_USER                   = google_sql_user.iam[each.key].name
          DB_MAX_OPEN               = tostring(local.pools[each.key])
          OPERATOR_TOKEN_AUDIENCE   = "https://${var.name_prefix}-${each.key}.internal"
          OPERATOR_SERVICE_ACCOUNTS = join(",", var.operator_accounts)

        })
        content {
          name  = env.key
          value = env.value

        }

      }
      dynamic "env" {

        for_each = local.secrets[each.key]
        content {

          name = env.key
          value_source {
            secret_key_ref {

              secret  = google_secret_manager_secret.runtime[env.value.secret_id].secret_id
              version = env.value.version

            }
          }

        }

      }
      startup_probe {

        http_get {
          path = "/healthz"
        }
        period_seconds    = 3
        failure_threshold = 30

      }

    }

  }
  depends_on = [terraform_data.safeguards, google_project_service.apis, google_project_iam_member.sql, google_secret_manager_secret_iam_member.runtime]
  lifecycle { prevent_destroy = true }

}

resource "google_cloud_run_v2_service" "gateway" {

  for_each            = local.gateway_deployments
  name                = each.value
  location            = var.region
  deletion_protection = true
  scaling {
    max_instance_count = var.max_instances.gateway
  }
  dynamic "traffic" {
    for_each = each.key == "public" ? [1] : []
    content {
      type     = "TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION"
      revision = var.public_serving_revision
      percent  = 100
    }
  }
  dynamic "traffic" {
    for_each = each.key == "public" && var.stage_public_candidate ? [1] : []
    content {
      type    = "TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST"
      percent = 0
      tag     = "milestone2"
    }
  }
  template {

    service_account                  = google_service_account.runtime["gateway"].email
    timeout                          = "330s"
    max_instance_request_concurrency = 40
    scaling {

      min_instance_count = 0
      max_instance_count = var.max_instances["gateway"]

    }
    dynamic "vpc_access" {

      for_each = var.sql_ip_type == "private" ? [1] : []
      content {

        egress = "PRIVATE_RANGES_ONLY"
        network_interfaces {

          network    = var.vpc_network
          subnetwork = var.vpc_subnetwork

        }

      }

    }
    volumes {

      name = "bootstrap"
      empty_dir {
        medium     = "MEMORY"
        size_limit = "8Mi"

      }

    }
    containers {

      name       = "envoy"
      image      = var.envoy_image
      args       = ["-c", "/shared/envoy.json", "--log-format", "%v"]
      depends_on = ["authorization"]
      ports {
        container_port = 8080
      }
      resources {
        limits = {
          cpu = "1", memory = "256Mi"
        }
        cpu_idle = true

      }
      volume_mounts {
        name       = "bootstrap"
        mount_path = "/shared"

      }
      startup_probe {
        http_get {
          path = "/healthz"
        }
        period_seconds    = 3
        failure_threshold = 30

      }

    }
    containers {

      name  = "authorization"
      image = var.adapter_image
      resources {
        limits = {
          cpu = "1", memory = "256Mi"
        }
        cpu_idle = true

      }
      volume_mounts {
        name       = "bootstrap"
        mount_path = "/shared"

      }
      dynamic "env" {

        for_each = merge(local.common_env, var.gateway_settings, {

          DB_USER                 = google_sql_user.iam["gateway"].name
          DB_MAX_OPEN             = tostring(var.db_pool_limits.gateway)
          ENVOY_CONFIG_PATH       = "/shared/envoy.json"
          LEGACY_BACKEND_URL      = google_cloud_run_v2_service.backend["legacy"].uri
          LEGACY_BACKEND_AUDIENCE = google_cloud_run_v2_service.backend["legacy"].uri
          SERVICE_BACKENDS_JSON   = jsonencode(var.service_backends)
          RATE_LIMIT_MODE         = var.rate_limit_mode
          CLIENT_IP_MODE          = "forwarded"
          TRUSTED_PROXY_HOPS      = tostring(var.trusted_proxy_hops)

        })
        content {
          name  = env.key
          value = env.value

        }

      }
      dynamic "env" {

        for_each = local.secrets.gateway
        content {
          name = env.key
          value_source {
            secret_key_ref {

              secret  = google_secret_manager_secret.runtime[env.value.secret_id].secret_id
              version = env.value.version

            }
          }

        }

      }
      startup_probe {

        http_get {
          path = "/healthz"
          port = 9002

        }
        period_seconds    = 3
        failure_threshold = 30

      }

    }

  }
  depends_on = [terraform_data.safeguards, google_project_iam_member.sql, google_secret_manager_secret_iam_member.runtime]
  lifecycle { prevent_destroy = true }

}

resource "google_cloud_run_v2_service_iam_member" "gateway_invoker" {

  count    = local.runtime_enabled ? 1 : 0
  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_service.backend["legacy"].name
  role     = "roles/run.invoker"
  member   = "serviceAccount:${google_service_account.runtime["gateway"].email}"

}
resource "google_cloud_run_v2_service_iam_member" "scheduler_invoker" {

  count    = local.runtime_enabled ? 1 : 0
  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_service.backend["worker"].name
  role     = "roles/run.invoker"
  member   = "serviceAccount:${google_service_account.runtime["scheduler"].email}"

}
resource "google_cloud_run_v2_service_iam_member" "public" {

  count    = var.public_gateway && contains(keys(local.gateway_deployments), "public") ? 1 : 0
  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_service.gateway["public"].name
  role     = "roles/run.invoker"
  member   = "allUsers"

}
resource "google_cloud_run_v2_service_iam_member" "operators" {

  for_each = local.runtime_enabled ? {
    for item in setproduct(["legacy", "worker"], var.operator_accounts) : "${item[0]}:${item[1]}" => item
  } : {}
  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_service.backend[each.value[0]].name
  role     = "roles/run.invoker"
  member   = "serviceAccount:${each.value[1]}"

}
resource "google_cloud_run_v2_service_iam_member" "nia" {

  count    = var.nia_service_name == null ? 0 : 1
  project  = var.project_id
  location = var.nia_region
  name     = var.nia_service_name
  role     = "roles/run.invoker"
  member   = "serviceAccount:${google_service_account.runtime["legacy"].email}"

}
resource "google_service_account_iam_member" "deployer" {

  for_each           = var.deployer_service_account == null ? toset([]) : local.runtime_owners
  service_account_id = google_service_account.runtime[each.key].name
  role               = "roles/iam.serviceAccountUser"
  member             = "serviceAccount:${var.deployer_service_account}"

}

resource "google_cloud_scheduler_job" "business" {

  for_each         = local.runtime_enabled ? local.schedules : {}
  name             = each.key
  region           = var.region
  schedule         = each.value
  time_zone        = var.enable_low_frequency_reconciliation && !contains(["daily-honour", "support-maintenance"], each.key) ? "Etc/UTC" : "America/Toronto"
  paused           = !var.enable_schedules
  attempt_deadline = "930s"
  retry_config {

    retry_count          = 3
    min_backoff_duration = "10s"
    max_backoff_duration = "60s"
    max_retry_duration   = "3600s"

  }
  http_target {

    http_method = "POST"
    uri         = "${google_cloud_run_v2_service.backend["worker"].uri}/internal/jobs/${each.key}/run"
    oidc_token {

      service_account_email = google_service_account.runtime["scheduler"].email
      audience              = var.enable_file_version_events ? local.file_version_worker_url : google_cloud_run_v2_service.backend["worker"].uri

    }

  }
  depends_on = [google_cloud_run_v2_service_iam_member.scheduler_invoker]

}

resource "google_cloud_run_v2_job" "cleanup" {

  count               = local.runtime_enabled ? 1 : 0
  name                = "${var.name_prefix}-limit-cleanup"
  location            = var.region
  deletion_protection = true
  template {

    task_count  = 1
    parallelism = 1
    template {

      service_account = google_service_account.runtime["gateway"].email
      timeout         = "90s"
      max_retries     = 1
      dynamic "vpc_access" {

        for_each = var.sql_ip_type == "private" ? [1] : []
        content {
          egress = "PRIVATE_RANGES_ONLY"
          network_interfaces {
            network    = var.vpc_network
            subnetwork = var.vpc_subnetwork

          }

        }

      }
      containers {

        image = var.adapter_image
        args  = ["cleanup"]
        resources {
          limits = {
            cpu = "1", memory = "512Mi"
          }
        }
        dynamic "env" {

          for_each = merge(local.common_env, {
            DB_USER = google_sql_user.iam["gateway"].name, DB_MAX_OPEN = tostring(var.db_pool_limits.cleanup)
          })
          content {
            name  = env.key
            value = env.value

          }

        }

      }

    }

  }
  depends_on = [terraform_data.safeguards, google_project_iam_member.sql]
  lifecycle { prevent_destroy = true }

}
resource "google_cloud_run_v2_job_iam_member" "cleanup_invoker" {

  count    = local.runtime_enabled ? 1 : 0
  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_job.cleanup[0].name
  role     = "roles/run.invoker"
  member   = "serviceAccount:${google_service_account.runtime["scheduler"].email}"

}
resource "google_cloud_scheduler_job" "cleanup" {

  count     = local.runtime_enabled ? 1 : 0
  name      = "${var.name_prefix}-limit-cleanup"
  region    = var.region
  schedule  = var.enable_low_frequency_reconciliation ? "25 */12 * * *" : "*/10 * * * *"
  time_zone = "Etc/UTC"
  paused    = !var.enable_schedules
  http_target {

    http_method = "POST"
    uri         = "https://run.googleapis.com/v2/${google_cloud_run_v2_job.cleanup[0].id}:run"
    oauth_token {
      service_account_email = google_service_account.runtime["scheduler"].email
    }

  }
  depends_on = [google_cloud_run_v2_job_iam_member.cleanup_invoker]

}
