variable "runtime_serving_revisions" {
  type        = map(string)
  default     = {}
  description = "Verified current serving revisions for private runtimes. Pin before staging a configuration change. Gateway retains public_serving_revision."
  validation {
    condition     = alltrue([for owner, revision in var.runtime_serving_revisions : contains(["legacy", "worker", "notifications", "identity", "audit"], owner) && can(regex("^[a-z][a-z0-9-]{0,62}$", revision))])
    error_message = "Use an explicit verified development revision belonging to a supported private runtime."
  }
}
variable "runtime_candidate_revisions" {
  type        = map(string)
  default     = {}
  description = "At most one exact revision recorded in the held persistent window; stage it at zero traffic."
  validation {
    condition     = length(var.runtime_candidate_revisions) <= 1 && alltrue([for owner, revision in var.runtime_candidate_revisions : contains(["gateway", "legacy", "worker", "notifications", "identity", "audit"], owner) && can(regex("^[a-z][a-z0-9-]{0,62}$", revision))])
    error_message = "Stage at most one explicit runtime revision in each persistent window."
  }
}
variable "runtime_template_revisions" {
  type        = map(string)
  default     = {}
  description = "Retain the exact last-created template revision after promotion, so clearing the zero-traffic candidate does not create another revision."
  validation {
    condition     = alltrue([for owner, revision in var.runtime_template_revisions : contains(["gateway", "legacy", "worker", "notifications", "identity", "audit"], owner) && can(regex("^[a-z][a-z0-9-]{0,62}$", revision))])
    error_message = "Retain explicit development template revision names."
  }
}
resource "terraform_data" "runtime_staging_safeguards" {
  lifecycle {
    precondition {
      condition     = alltrue([for owner, revision in var.runtime_template_revisions : startswith(revision, "${owner == "gateway" ? var.public_gateway_service_name : "${var.name_prefix}-${owner}"}-")])
      error_message = "Retained template revisions must belong to their selected service."
    }
    precondition {
      condition     = alltrue([for owner, revision in var.runtime_candidate_revisions : try(startswith(revision, "${owner == "gateway" ? var.public_gateway_service_name : "${var.name_prefix}-${owner}"}-"), false) && (owner == "gateway" ? var.public_serving_revision != null : contains(keys(var.runtime_serving_revisions), owner))])
      error_message = "A staged candidate must belong to its service and retain the verified serving revision."
    }
    precondition {
      condition     = length(var.runtime_candidate_revisions) == 0 || try(local.current_rollout_marker.token == var.rollout_window_token && local.current_rollout_marker.verified && local.current_rollout_marker.state == "held" && lookup(var.runtime_candidate_revisions, local.current_rollout_marker.target, null) == local.current_rollout_marker.revision, false)
      error_message = "Terraform staging must match the exact revision/target recorded before the mutation."
    }
  }
}
resource "google_project_iam_custom_role" "rollout_metrics" {
  count       = var.rollout_inspection_enabled ? 1 : 0
  project     = var.project_id
  role_id     = "nordikRuntimeRolloutMetrics"
  title       = "Nordik runtime rollout metrics"
  permissions = ["monitoring.timeSeries.list"]
}
resource "google_project_iam_member" "rollout_metrics" {
  count   = var.rollout_inspection_enabled ? 1 : 0
  project = var.project_id
  role    = google_project_iam_custom_role.rollout_metrics[0].name
  member  = "serviceAccount:${var.rollout_deployer_account}"
}
resource "google_cloud_run_v2_service_iam_member" "owner_release_testers" {
  for_each = var.rollout_inspection_enabled ? local.extracted_services : {}
  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_service.owner[each.key].name
  role     = "roles/run.invoker"
  member   = "serviceAccount:${var.rollout_deployer_account}"
}
