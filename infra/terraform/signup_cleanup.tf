variable "enable_signup_cleanup" {
  type        = bool
  default     = false
  description = "Enable the finite auth-owned signup expiry job after migration 004 and the compatible server image are verified."
}

locals {
  signup_cleanup_runtime     = local.notifications_runtime && var.enable_signup_cleanup
  signup_cleanup_connections = local.signup_cleanup_runtime ? 1 : 0
}

resource "terraform_data" "signup_cleanup_safeguards" {
  lifecycle {
    precondition {
      condition     = !var.enable_signup_cleanup || var.enable_notifications
      error_message = "Signup cleanup requires the Notifications/signup schema and reviewed compatible server image."
    }
  }
}

resource "google_cloud_run_v2_job" "signup_cleanup" {
  count               = local.signup_cleanup_runtime ? 1 : 0
  project             = var.project_id
  name                = "${var.name_prefix}-signup-cleanup"
  location            = var.region
  deletion_protection = true
  template {
    task_count  = 1
    parallelism = 1
    template {
      service_account = google_service_account.runtime[var.identity_signup_cleanup ? "identity" : "legacy"].email
      timeout         = "60s"
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
        image = var.identity_signup_cleanup ? var.identity_image : var.server_image
        args  = ["signup-cleanup"]
        resources {
          limits = { cpu = "1", memory = "512Mi" }
        }
        dynamic "env" {
          for_each = merge(local.common_env, {
            DB_USER     = google_sql_user.iam[var.identity_signup_cleanup ? "identity" : "legacy"].name
            DB_SCHEMA   = var.identity_signup_cleanup ? (var.identity_tables_relocated ? "identity" : "identity,public") : "public"
            DB_MAX_OPEN = "1"
          })
          content {
            name  = env.key
            value = env.value
          }
        }
      }
    }
  }
  depends_on = [terraform_data.serialized_rollout_safeguards, terraform_data.safeguards, terraform_data.signup_cleanup_safeguards, google_project_iam_member.sql]
  lifecycle { prevent_destroy = true }
}

resource "google_cloud_run_v2_job_iam_member" "signup_cleanup_invoker" {
  count    = local.signup_cleanup_runtime ? 1 : 0
  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_job.signup_cleanup[0].name
  role     = "roles/run.invoker"
  member   = "serviceAccount:${google_service_account.runtime["scheduler"].email}"
}

resource "google_cloud_scheduler_job" "signup_cleanup" {
  count            = local.signup_cleanup_runtime ? 1 : 0
  project          = var.project_id
  name             = "${var.name_prefix}-signup-cleanup"
  region           = var.region
  schedule         = var.enable_low_frequency_reconciliation ? "30 */12 * * *" : "17 * * * *"
  time_zone        = "Etc/UTC"
  paused           = !var.enable_schedules
  attempt_deadline = "30s"
  retry_config { retry_count = 0 }
  http_target {
    http_method = "POST"
    uri         = "https://run.googleapis.com/v2/${google_cloud_run_v2_job.signup_cleanup[0].id}:run"
    body        = base64encode("{}")
    headers     = { "Content-Type" = "application/json" }
    oauth_token {
      service_account_email = google_service_account.runtime["scheduler"].email
    }
  }
  depends_on = [google_cloud_run_v2_job_iam_member.signup_cleanup_invoker]
}
