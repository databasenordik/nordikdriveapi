output "gateway_url" { value = try(google_cloud_run_v2_service.gateway["public"].uri, google_cloud_run_v2_service.gateway["preview"].uri, null) }
output "gateway_urls" { value = { for name, service in google_cloud_run_v2_service.gateway : name => service.uri } }
output "backend_urls" { value = { for name, service in google_cloud_run_v2_service.backend : name => service.uri } }
output "database_users" { value = { for name, user in google_sql_user.iam : name => user.name } }
output "service_accounts" { value = { for name, sa in google_service_account.runtime : name => sa.email } }
output "operator_audiences" { value = { for name in ["legacy", "worker"] : name => "https://${var.name_prefix}-${name}.internal" } }
