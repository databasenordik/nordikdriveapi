variable "deployment_stage" {
  type        = string
  default     = "bootstrap"
  description = "Monotonic rollout: bootstrap (metadata/IAM), database (IAM DB users), preview (private runtime/paused jobs), cutover (also imported public gateway). Reverting stages would destroy resources and is blocked by prevent_destroy."
  validation {
    condition     = contains(["bootstrap", "database", "preview", "cutover"], var.deployment_stage)
    error_message = "Choose bootstrap, database, preview or cutover."
  }
}

variable "enable_preview_gateway" {
  type        = bool
  default     = true
  nullable    = false
  description = "Optional separate private preview. The development profile disables this and stages the gateway on the existing public service at 0% traffic."
}

variable "iam_database_authentication_verified" {
  type        = bool
  default     = false
  description = "True only after checking cloudsql.iam_authentication is enabled on the referenced existing instance."
}

variable "runtime_prerequisites_verified" {
  type        = bool
  default     = false
  description = "True after platform migrations/grants, numeric secret versions and enabled provider configuration are verified."
}

variable "legacy_scheduling_retired_verified" {
  type        = bool
  default     = false
  description = "True only after old unguarded cron revisions are drained and the old honour Scheduler trigger is retired."
}

variable "public_gateway_service_name" {
  type        = string
  default     = null
  description = "Existing public service to import into google_cloud_run_v2_service.gateway[\"public\"] at cutover. Preview keeps its own address."
  validation {
    condition     = var.public_gateway_service_name == null || can(regex("^[a-z][a-z0-9-]{0,47}[a-z0-9]$", var.public_gateway_service_name))
    error_message = "Public service name must be a valid Cloud Run service name."
  }
}

variable "public_serving_revision" {
  type        = string
  default     = null
  description = "Explicit revision retaining 100% of public service traffic. At staging use the current compatible API revision; promote by updating this to the verified gateway revision."
}

variable "stage_public_candidate" {
  type        = bool
  default     = true
  description = "Add a milestone2 tag for the latest public revision at 0% traffic. After promotion set false and retain an explicit serving revision."
}

resource "google_cloud_run_v2_service_iam_member" "preview_testers" {
  for_each = local.runtime_enabled && var.enable_preview_gateway ? var.preview_invoker_members : toset([])
  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_service.gateway["preview"].name
  role     = "roles/run.invoker"
  member   = each.value
}

variable "preview_invoker_members" {
  type        = set(string)
  default     = []
  description = "Explicit users/service accounts allowed to invoke the private preview gateway."
  validation {
    condition     = alltrue([for member in var.preview_invoker_members : can(regex("^(user|serviceAccount):[^ ]+@[^ ]+$", member))])
    error_message = "Preview invokers must be explicit user:email or serviceAccount:email members."
  }
}
