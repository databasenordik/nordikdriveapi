variable "enable_notification_source" {
  type        = bool
  default     = false
  description = "Prepare the private producer source before enabling event-driven email delivery."
}

variable "enable_event_driven_notifications" {
  type        = bool
  default     = false
  description = "Publish committed email IDs to the existing delivery queues instead of waiting for a scan."
}

variable "enable_low_frequency_reconciliation" {
  type        = bool
  default     = false
  description = "After first-processing acceptance, normalize every six hours and reconcile/clean up every twelve hours."
}

locals {
  notification_source_runtime    = local.notifications_runtime && var.enable_notification_source
  notification_events_runtime    = local.notifications_runtime && var.enable_event_driven_notifications
  notification_outbox_source_url = var.project_number != null ? "https://${var.name_prefix}-legacy-${var.project_number}.${var.region}.run.app" : ""
  notification_source_env = var.enable_notification_source ? {
    NOTIFICATION_OUTBOX_SOURCE_URL     = local.notification_outbox_source_url
    NOTIFICATION_OUTBOX_READER_ACCOUNT = google_service_account.runtime["notifications"].email
  } : {}
  notification_event_env = var.enable_event_driven_notifications ? {
    NOTIFICATION_EVENT_DISPATCH = "true"
    NOTIFICATIONS_OTP_QUEUE     = "projects/${var.project_id}/locations/${var.region}/queues/${var.name_prefix}-otp"
    NOTIFICATIONS_NORMAL_QUEUE  = "projects/${var.project_id}/locations/${var.region}/queues/${var.name_prefix}-normal"
    NOTIFICATIONS_TASK_ACCOUNT  = google_service_account.runtime["tasks"].email
  } : {}
}

resource "terraform_data" "notification_event_safeguards" {
  lifecycle {
    precondition {
      condition     = !var.enable_notification_source || var.enable_notifications
      error_message = "Prepare Notifications and its verified project number before its private producer source."
    }
    precondition {
      condition     = !var.enable_event_driven_notifications || (var.enable_notification_source && var.notification_delivery_enabled)
      error_message = "Event delivery requires the accepted producer source and transactional outbox mode."
    }
    precondition {
      condition     = !var.enable_low_frequency_reconciliation || (var.enable_event_driven_notifications && var.enable_file_version_events)
      error_message = "Accept immediate email and file-version processing before slowing their fallback scans."
    }
  }
}

resource "google_cloud_run_v2_service_iam_member" "notification_source_reader" {
  count    = local.notification_source_runtime ? 1 : 0
  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_service.backend["legacy"].name
  role     = "roles/run.invoker"
  member   = "serviceAccount:${google_service_account.runtime["notifications"].email}"
}

resource "google_cloud_tasks_queue_iam_member" "notification_event_producer" {
  for_each = local.notification_events_runtime ? {
    for pair in setproduct(concat(["legacy", "worker"], var.enable_identity ? ["identity"] : []), ["otp", "normal"]) : "${pair[0]}:${pair[1]}" => pair
  } : {}
  project  = var.project_id
  location = var.region
  name     = google_cloud_tasks_queue.notifications[each.value[1]].name
  role     = "roles/cloudtasks.enqueuer"
  member   = "serviceAccount:${google_service_account.runtime[each.value[0]].email}"
}

resource "google_service_account_iam_member" "notification_event_task_identity" {
  for_each           = local.notification_events_runtime ? toset(concat(["legacy", "worker"], var.enable_identity ? ["identity"] : [])) : toset([])
  service_account_id = google_service_account.runtime["tasks"].name
  role               = "roles/iam.serviceAccountUser"
  member             = "serviceAccount:${google_service_account.runtime[each.key].email}"
}
