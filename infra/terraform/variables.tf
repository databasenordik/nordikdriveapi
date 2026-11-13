variable "project_id" { type = string }
variable "region" {
  type    = string
  default = "us-west1"
}
variable "name_prefix" {
  type    = string
  default = "nordikdriveapi"
  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{2,17}$", var.name_prefix))
    error_message = "Use a 3-18 character lowercase resource prefix."
  }
}
variable "sql_instance_name" { type = string }
variable "sql_connection_name" { type = string }
variable "database_name" { type = string }
variable "sql_ip_type" {
  type    = string
  default = "public"
  validation {
    condition     = contains(["public", "private"], var.sql_ip_type)
    error_message = "sql_ip_type must be public or private."
  }
}
variable "vpc_network" {
  type    = string
  default = null
}
variable "vpc_subnetwork" {
  type    = string
  default = null
}
variable "server_image" {
  type    = string
  default = null
  validation {
    condition     = var.server_image == null || can(regex("@sha256:[a-f0-9]{64}$", var.server_image))
    error_message = "Use an immutable server image digest."
  }
}
variable "adapter_image" {
  type    = string
  default = null
  validation {
    condition     = var.adapter_image == null || can(regex("@sha256:[a-f0-9]{64}$", var.adapter_image))
    error_message = "Use an immutable adapter image digest."
  }
}
variable "worker_image" {
  type        = string
  default     = null
  description = "Optional independently selected worker digest; defaults to the server artifact."
  validation {
    condition     = var.worker_image == null || can(regex("@sha256:[a-f0-9]{64}$", var.worker_image))
    error_message = "Use an immutable worker image digest."
  }
}
variable "envoy_image" {
  type    = string
  default = "envoyproxy/envoy@sha256:93a82e7584cb8181ad408e4e49477a3ae1d775f698214841eda4e9309fc06a06"
}
variable "gateway_service_name" {
  type        = string
  default     = null
  description = "Optional private preview service name. Public cutover uses public_gateway_service_name and a separate resource address."
}
variable "public_gateway" {
  type    = bool
  default = false
}
variable "jwt_secret" {
  type = object({ secret_id = string, version = string })
  validation {
    condition     = can(regex("^[1-9][0-9]*$", var.jwt_secret.version))
    error_message = "Pin a numeric JWT secret version; latest is not allowed."
  }
}
variable "runtime_secrets" {
  type    = map(map(object({ secret_id = string, version = string })))
  default = { gateway = {}, legacy = {}, worker = {} }
  validation {
    condition     = alltrue([for owner, secrets in var.runtime_secrets : contains(["gateway", "legacy", "worker", "notifications", "identity", "audit"], owner)]) && alltrue(flatten([for secrets in values(var.runtime_secrets) : [for secret in values(secrets) : can(regex("^[1-9][0-9]*$", secret.version))]]))
    error_message = "Use gateway/legacy/worker owners and numeric secret versions."
  }
}
variable "runtime_settings" {
  type    = map(map(string))
  default = { legacy = {}, worker = {} }
  validation {
    condition     = alltrue([for owner in keys(var.runtime_settings) : contains(["legacy", "worker", "notifications", "identity", "audit"], owner)])
    error_message = "runtime_settings supports legacy, worker and notifications only."
  }
}
variable "rate_limit_mode" {
  type    = string
  default = "observe"
  validation {
    condition     = contains(["observe", "enforce"], var.rate_limit_mode)
    error_message = "Choose observe or enforce."
  }
}
variable "trusted_proxy_hops" {
  type    = number
  default = null
  validation {
    condition     = var.trusted_proxy_hops == null || try(var.trusted_proxy_hops >= 1 && var.trusted_proxy_hops <= 10 && floor(var.trusted_proxy_hops) == var.trusted_proxy_hops, false)
    error_message = "Provide the verified 1-10 proxy hop count."
  }
}
variable "service_backends" {
  type        = map(object({ url = string, audience = string }))
  default     = {}
  description = "Optional per-service routing overrides after extraction. Unspecified groups use the legacy service. Grant its invoker IAM separately for every override."
  validation {
    condition     = alltrue([for name in keys(var.service_backends) : contains(["identity", "records", "support", "chat", "audit"], name)])
    error_message = "Supported backends: identity, records, support, chat, audit."
  }
}
variable "max_instances" {
  type    = map(number)
  default = { gateway = 2, legacy = 2, worker = 1 }
  validation {
    condition     = alltrue([for name in ["gateway", "legacy", "worker"] : try(var.max_instances[name] >= 1 && floor(var.max_instances[name]) == var.max_instances[name], false)])
    error_message = "Provide positive integer maximums for gateway, legacy and worker."
  }
}
variable "database_connection_capacity" {
  type        = number
  description = "Verified Cloud SQL connection capacity. Combined pools plus cleanup reserve must leave 20% spare."
  validation {
    condition     = var.database_connection_capacity >= 1 && floor(var.database_connection_capacity) == var.database_connection_capacity
    error_message = "Connection capacity must be a verified positive integer."
  }
}
variable "other_reserved_connections" {
  type        = number
  description = "Verified maximum connections reserved for existing services, old revisions and other clients outside this module."
  validation {
    condition     = var.other_reserved_connections >= 0 && floor(var.other_reserved_connections) == var.other_reserved_connections
    error_message = "Provide a non-negative integer reservation for all other database consumers."
  }
}
variable "gateway_settings" {
  type    = map(string)
  default = {}
  validation {
    condition     = alltrue([for key in keys(var.gateway_settings) : contains(["CORS_ORIGINS", "RATE_AUTH_PER_MINUTE", "RATE_PUBLIC_PER_MINUTE", "RATE_USER_PER_MINUTE", "RATE_CHAT_PER_MINUTE", "RATE_DB_ACQUIRE_MS", "RATE_DB_QUERY_MS"], key)])
    error_message = "gateway_settings supports CORS, quota thresholds and limiter timing budgets only."
  }
}
variable "enable_schedules" {
  type        = bool
  default     = false
  description = "Leave paused until migrations, SMTP/Zoom, IAM and scheduler cutover checks pass."
}
variable "buckets_by_owner" {
  type    = map(set(string))
  default = { legacy = [], worker = [] }
  validation {
    condition     = alltrue([for name in keys(var.buckets_by_owner) : contains(["legacy", "worker"], name)])
    error_message = "Only legacy/worker currently require object access."
  }
}
variable "operator_accounts" {
  type    = set(string)
  default = []
}
variable "deployer_service_account" {
  type    = string
  default = null
}
variable "nia_service_name" {
  type    = string
  default = null
}
variable "nia_region" {
  type    = string
  default = "us-west1"
}
