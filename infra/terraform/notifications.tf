variable "enable_notifications" {

  type        = bool
  default     = false
  description = "Provision the development Notifications extraction. Existing deployments remain unchanged until explicitly enabled."

}

variable "project_number" {

  type        = string
  default     = null
  description = "Verified numeric project number, used for the deterministic Notifications URL and explicit audience."

}

variable "notifications_image" {

  type    = string
  default = null
  validation {

    condition     = var.notifications_image == null || can(regex("/nordik_notifications_go@sha256:[a-f0-9]{64}$", var.notifications_image))
    error_message = "Notifications requires its own immutable image digest."

  }


}

variable "notification_delivery_enabled" {
  type    = bool
  default = false
}

variable "notification_queues_enabled" {
  type    = bool
  default = false
}

variable "signup_require_verified_email" {
  type    = bool
  default = false
}

variable "notification_payload_key_version" {
  type    = string
  default = "1"
}

variable "signup_verification_key_version" {
  type    = string
  default = "1"
}


locals {

  notifications_runtime    = local.runtime_enabled && var.enable_notifications
  notification_connections = local.notifications_runtime ? 1 : 0
  notifications_url        = var.enable_notifications && var.project_number != null ? "https://${var.name_prefix}-notifications-${var.project_number}.${var.region}.run.app" : ""
  notification_producer_env = var.enable_notifications ? {

    NOTIFICATIONS_URL                = local.notifications_url
    NOTIFICATION_DELIVERY_MODE       = var.notification_delivery_enabled ? "outbox" : "direct"
    NOTIFICATION_PAYLOAD_KEY_VERSION = var.notification_payload_key_version
    SIGNUP_VERIFICATION_KEY_VERSION  = var.signup_verification_key_version

    } : {

  }


}

resource "terraform_data" "notification_safeguards" {

  lifecycle {

    precondition {

      condition     = !var.enable_notifications || can(regex("^[0-9]+$", var.project_number))
      error_message = "Verify project_number before provisioning Notifications."

    }

    precondition {

      condition     = !local.notifications_runtime || var.notifications_image != null
      error_message = "A reviewed Notifications image is required before runtime provisioning."

    }

    precondition {

      condition     = !var.notification_delivery_enabled || var.enable_notifications
      error_message = "Producer outbox delivery requires Notifications resources."

    }

    precondition {

      condition = !local.notifications_runtime || alltrue([for name in ["GMAIL_APP_PASSWORD", "NOTIFICATION_PAYLOAD_KEYS"] : contains(keys(lookup(var.runtime_secrets, "notifications", {

        }
      )), name)])
      error_message = "Pin Notifications SMTP and payload-key secret versions before runtime provisioning."

    }

    precondition {

      condition = !var.notification_delivery_enabled || alltrue([for owner in ["legacy", "worker"] : contains(keys(lookup(var.runtime_secrets, owner, {

        }
      )), "NOTIFICATION_PAYLOAD_KEYS")])
      error_message = "Legacy and worker need pinned payload-key versions before producing outboxes."

    }

    precondition {

      condition = !var.signup_require_verified_email || (var.notification_delivery_enabled && contains(keys(lookup(var.runtime_secrets, var.identity_public_enabled ? "identity" : "legacy", {

        }
      )), "SIGNUP_VERIFICATION_KEYS"))
      error_message = "Mandatory signup verification requires outbox delivery, its auth key and the compatible frontend rollout."

    }

    precondition {

      condition     = alltrue(flatten([for settings in values(var.runtime_settings) : [for name in keys(settings) : !contains(["NOTIFICATION_PAYLOAD_KEYS", "SIGNUP_VERIFICATION_KEYS", "DB_SCHEMA", "NOTIFICATION_DELIVERY_MODE", "SIGNUP_REQUIRE_VERIFIED_EMAIL", "NOTIFICATIONS_URL"], name)]]))
      error_message = "Use protected secret references and the dedicated notification ownership/rollout inputs."

    }


  }


}

resource "google_cloud_run_v2_service" "notifications" {

  count               = local.notifications_runtime ? 1 : 0
  project             = var.project_id
  name                = "${var.name_prefix}-notifications"
  location            = var.region
  deletion_protection = true
  ingress             = "INGRESS_TRAFFIC_ALL"
  custom_audiences    = [local.notifications_url]
  scaling {
    max_instance_count = 1
  }

  dynamic "traffic" {
    for_each = contains(keys(var.runtime_serving_revisions), "notifications") ? [1] : []
    content {
      type     = "TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION"
      revision = var.runtime_serving_revisions["notifications"]
      percent  = 100
    }
  }
  dynamic "traffic" {
    for_each = contains(keys(var.runtime_candidate_revisions), "notifications") ? [1] : []
    content {
      type     = "TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION"
      revision = var.runtime_candidate_revisions["notifications"]
      percent  = 0
      tag      = "m4-candidate"
    }
  }
  template {
    revision = lookup(var.runtime_candidate_revisions, "notifications", lookup(var.runtime_template_revisions, "notifications", null))

    service_account                  = google_service_account.runtime["notifications"].email
    timeout                          = "40s"
    max_instance_request_concurrency = 1
    scaling {
      min_instance_count = 0
      max_instance_count = 1
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

      image = var.notifications_image
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

        for_each = merge(lookup(var.runtime_settings, "notifications", {

          }
          ), local.common_env, local.notifications_identity_env, var.enable_notification_source ? { NOTIFICATION_OUTBOX_SOURCE_URL = local.notification_outbox_source_url } : {}, {

          DB_SCHEMA                        = "notifications"
          DB_USER                          = google_sql_user.iam["notifications"].name
          DB_MAX_OPEN                      = "1"
          NOTIFICATION_PAYLOAD_KEY_VERSION = var.notification_payload_key_version
          NOTIFICATIONS_URL                = local.notifications_url
          NOTIFICATIONS_LEGACY_ACCOUNT     = google_service_account.runtime["legacy"].email
          NOTIFICATIONS_WORKER_ACCOUNT     = google_service_account.runtime["worker"].email
          NOTIFICATIONS_TASK_ACCOUNT       = google_service_account.runtime["tasks"].email
          NOTIFICATIONS_OPERATOR_ACCOUNTS  = join(",", var.operator_accounts)
          NOTIFICATIONS_OTP_QUEUE          = google_cloud_tasks_queue.notifications["otp"].id
          NOTIFICATIONS_NORMAL_QUEUE       = google_cloud_tasks_queue.notifications["normal"].id

          }
        )
        content {
          name  = env.key
          value = env.value
        }


      }

      dynamic "env" {

        for_each = lookup(local.secrets, "notifications", {})
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

  depends_on = [terraform_data.runtime_staging_safeguards, terraform_data.serialized_rollout_safeguards, terraform_data.safeguards, terraform_data.notification_safeguards, google_project_service.apis, google_project_iam_member.sql, google_secret_manager_secret_iam_member.runtime]
  lifecycle {
    prevent_destroy = true
  }


}

resource "google_cloud_tasks_queue" "notifications" {

  for_each        = local.notifications_runtime ? toset(["otp", "normal"]) : toset([])
  project         = var.project_id
  name            = "${var.name_prefix}-${each.key}"
  location        = var.region
  desired_state   = var.notification_queues_enabled ? "RUNNING" : "PAUSED"
  deletion_policy = "PREVENT"
  rate_limits {
    max_concurrent_dispatches = 1
    max_dispatches_per_second = each.key == "otp" ? 1 : 0.1
  }

  retry_config {
    max_attempts       = 5
    max_retry_duration = each.key == "otp" ? "600s" : "86400s"
    min_backoff        = "10s"
    max_backoff        = "60s"
    max_doublings      = 3
  }

  http_target {

    oidc_token {
      service_account_email = google_service_account.runtime["tasks"].email
      audience              = local.notifications_url
    }

    uri_override {

      scheme                    = "HTTPS"
      host                      = trimprefix(local.notifications_url, "https://")
      uri_override_enforce_mode = "ALWAYS"
      path_override {
        path = "/internal/v1/deliver"
      }

      query_override {
        query_params = ""
      }


    }


  }

  depends_on = [google_project_service.apis, terraform_data.notification_safeguards]

}

resource "google_cloud_tasks_queue_iam_member" "notification_enqueue" {

  for_each = google_cloud_tasks_queue.notifications
  project  = var.project_id
  location = var.region
  name     = each.value.name
  role     = "roles/cloudtasks.enqueuer"
  member   = "serviceAccount:${google_service_account.runtime["notifications"].email}"

}

resource "google_service_account_iam_member" "notification_task_user" {

  count              = var.enable_notifications ? 1 : 0
  service_account_id = google_service_account.runtime["tasks"].name
  role               = "roles/iam.serviceAccountUser"
  member             = "serviceAccount:${google_service_account.runtime["notifications"].email}"

}

resource "google_cloud_run_v2_service_iam_member" "notification_callers" {

  for_each = local.notifications_runtime ? setunion(toset(["legacy", "worker", "tasks"]), var.enable_identity ? toset(["identity"]) : toset([])) : toset([])
  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_service.notifications[0].name
  role     = "roles/run.invoker"
  member   = "serviceAccount:${google_service_account.runtime[each.key].email}"

}

resource "google_storage_bucket_iam_member" "notification_attachments" {

  count  = var.enable_notifications ? 1 : 0
  bucket = "nordik-drive-photos"
  role   = "roles/storage.objectViewer"
  member = "serviceAccount:${google_service_account.runtime["notifications"].email}"
  condition {

    title      = "support_screenshot_generations_only"
    expression = "resource.name.startsWith('projects/_/buckets/nordik-drive-photos/objects/support_requests/')"

  }


}

resource "google_cloud_scheduler_job" "notification_dispatch" {

  count            = local.notifications_runtime ? 1 : 0
  project          = var.project_id
  name             = "notification-outbox-dispatch"
  region           = var.region
  schedule         = var.enable_low_frequency_reconciliation ? "0 */12 * * *" : "* * * * *"
  time_zone        = "Etc/UTC"
  paused           = !var.notification_delivery_enabled || !var.enable_schedules
  attempt_deadline = "60s"
  retry_config {
    retry_count          = 2
    min_backoff_duration = "10s"
    max_backoff_duration = "30s"
    max_retry_duration   = "120s"
  }

  http_target {

    http_method = "POST"
    uri         = "${google_cloud_run_v2_service.backend["worker"].uri}/internal/jobs/notification-outbox-dispatch/run"
    oidc_token {
      service_account_email = google_service_account.runtime["scheduler"].email
      audience              = var.enable_file_version_events ? local.file_version_worker_url : google_cloud_run_v2_service.backend["worker"].uri
    }


  }

  depends_on = [google_cloud_run_v2_service_iam_member.scheduler_invoker]

}

output "notifications" {

  value = local.notifications_runtime ? {

    service       = google_cloud_run_v2_service.notifications[0].name
    url           = local.notifications_url
    canonical_url = google_cloud_run_v2_service.notifications[0].uri
    queues = {
      for key, queue in google_cloud_tasks_queue.notifications : key => queue.id
    }

    iam_database_user            = google_sql_user.iam["notifications"].name
    connections_with_replacement = 2

  } : null

}
