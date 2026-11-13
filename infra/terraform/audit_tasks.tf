resource "google_cloud_tasks_queue" "audit" {
  count           = local.audit_runtime ? 1 : 0
  project         = var.project_id
  name            = "${var.name_prefix}-audit"
  location        = var.region
  desired_state   = var.audit_queues_enabled ? "RUNNING" : "PAUSED"
  deletion_policy = "PREVENT"
  rate_limits {
    max_concurrent_dispatches = 1
    max_dispatches_per_second = 1
  }
  retry_config {
    max_attempts       = 20
    max_retry_duration = "86400s"
    min_backoff        = "5s"
    max_backoff        = "300s"
    max_doublings      = 5
  }
  http_target {
    oidc_token {
      service_account_email = google_service_account.runtime["audit-tasks"].email
      audience              = local.audit_url
    }
    uri_override {
      scheme                    = "HTTPS"
      host                      = trimprefix(local.audit_url, "https://")
      uri_override_enforce_mode = "ALWAYS"
      path_override { path = "/internal/v1/audit/ingest" }
      query_override { query_params = "" }
    }
  }
  depends_on = [google_project_service.apis, terraform_data.identity_audit_safeguards]
}
resource "google_cloud_tasks_queue_iam_member" "audit_producer" {
  for_each = local.audit_runtime ? toset(["legacy", "identity"]) : toset([])
  project  = var.project_id
  location = var.region
  name     = google_cloud_tasks_queue.audit[0].name
  role     = "roles/cloudtasks.enqueuer"
  member   = "serviceAccount:${google_service_account.runtime[each.key].email}"
}
resource "google_service_account_iam_member" "audit_task_identity" {
  for_each           = var.enable_audit ? toset(["legacy", "identity"]) : toset([])
  service_account_id = google_service_account.runtime["audit-tasks"].name
  role               = "roles/iam.serviceAccountUser"
  member             = "serviceAccount:${google_service_account.runtime[each.key].email}"
}
resource "google_cloud_scheduler_job" "audit_dispatch" {
  count            = local.audit_runtime ? 1 : 0
  project          = var.project_id
  name             = "audit-outbox-dispatch"
  region           = var.region
  schedule         = "35 */12 * * *"
  time_zone        = "Etc/UTC"
  paused           = !var.enable_audit_events || !var.enable_schedules
  attempt_deadline = "60s"
  retry_config {
    retry_count          = 2
    min_backoff_duration = "10s"
    max_backoff_duration = "30s"
    max_retry_duration   = "120s"
  }
  http_target {
    http_method = "POST"
    uri         = "${local.worker_url}/internal/jobs/audit-outbox-dispatch/run"
    oidc_token {
      service_account_email = google_service_account.runtime["scheduler"].email
      audience              = local.worker_url
    }
  }
  depends_on = [google_cloud_run_v2_service_iam_member.scheduler_invoker]
}
