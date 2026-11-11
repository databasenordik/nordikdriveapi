variable "enable_file_version_events" {
  type        = bool
  default     = false
  description = "Trigger newly committed replace/revert jobs on demand before slowing the periodic repair scan."
}

locals {
  file_version_events_runtime = local.runtime_enabled && var.enable_file_version_events
  file_version_worker_url     = var.project_number != null ? "https://${var.name_prefix}-worker-${var.project_number}.${var.region}.run.app" : ""
  file_version_event_env = var.enable_file_version_events ? {
    FILE_VERSION_EVENT_QUEUE   = "projects/${var.project_id}/locations/${var.region}/queues/${var.name_prefix}-file-events"
    FILE_VERSION_WORKER_URL    = local.file_version_worker_url
    FILE_VERSION_EVENT_ACCOUNT = google_service_account.runtime["work-tasks"].email
    WORKER_SCHEDULER_ACCOUNT   = google_service_account.runtime["scheduler"].email
  } : {}
}

resource "terraform_data" "file_version_event_safeguards" {
  lifecycle {
    precondition {
      condition     = !var.enable_file_version_events || can(regex("^[0-9]+$", var.project_number))
      error_message = "Verify project_number before preparing the private Worker event audience."
    }
  }
}

resource "google_cloud_tasks_queue" "file_events" {
  count           = local.file_version_events_runtime ? 1 : 0
  project         = var.project_id
  location        = var.region
  name            = "${var.name_prefix}-file-events"
  deletion_policy = "PREVENT"
  desired_state   = "RUNNING"
  rate_limits {
    max_concurrent_dispatches = 1
    max_dispatches_per_second = 1
  }
  retry_config {
    max_attempts       = 20
    max_retry_duration = "86400s"
    min_backoff        = "60s"
    max_backoff        = "900s"
    max_doublings      = 4
  }
  http_target {
    oidc_token {
      service_account_email = google_service_account.runtime["work-tasks"].email
      audience              = local.file_version_worker_url
    }
    uri_override {
      scheme                    = "HTTPS"
      host                      = trimprefix(local.file_version_worker_url, "https://")
      uri_override_enforce_mode = "ALWAYS"
      path_override { path = "/internal/v1/work/file-version" }
      query_override { query_params = "" }
    }
  }
  depends_on = [google_project_service.apis, terraform_data.file_version_event_safeguards]
}

resource "google_cloud_run_v2_service_iam_member" "file_event_invoker" {
  count    = local.file_version_events_runtime ? 1 : 0
  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_service.backend["worker"].name
  role     = "roles/run.invoker"
  member   = "serviceAccount:${google_service_account.runtime["work-tasks"].email}"
}

resource "google_cloud_tasks_queue_iam_member" "file_event_producer" {
  count    = local.file_version_events_runtime ? 1 : 0
  project  = var.project_id
  location = var.region
  name     = google_cloud_tasks_queue.file_events[0].name
  role     = "roles/cloudtasks.enqueuer"
  member   = "serviceAccount:${google_service_account.runtime["legacy"].email}"
}

resource "google_service_account_iam_member" "file_event_task_identity" {
  count              = local.file_version_events_runtime ? 1 : 0
  service_account_id = google_service_account.runtime["work-tasks"].name
  role               = "roles/iam.serviceAccountUser"
  member             = "serviceAccount:${google_service_account.runtime["legacy"].email}"
}
