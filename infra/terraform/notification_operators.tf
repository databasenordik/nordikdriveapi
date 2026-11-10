resource "google_cloud_run_v2_service_iam_member" "notification_operators" {
  for_each = local.notifications_runtime ? toset(var.operator_accounts) : toset([])
  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_service.notifications[0].name
  role     = "roles/run.invoker"
  member   = "serviceAccount:${each.value}"
}
