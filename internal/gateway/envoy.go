package gateway

import (
	"encoding/json"
	"net/url"
	"strconv"
)

// Bootstrap contains service boundaries only. No endpoint catalogue or tokens
// are embedded; the selected backend's token is supplied by ext_authz.
func Bootstrap(backends map[string]Backend) ([]byte, error) {
	clusters := []any{map[string]any{"name": "authorization", "type": "STATIC", "connect_timeout": "5s", "typed_extension_protocol_options": map[string]any{"envoy.extensions.upstreams.http.v3.HttpProtocolOptions": map[string]any{"@type": "type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions", "explicit_http_config": map[string]any{"http2_protocol_options": map[string]any{}}}}, "load_assignment": assignment("authorization", "127.0.0.1", 9001)}}
	routes := []any{map[string]any{"match": map[string]any{"path": "/healthz"}, "direct_response": map[string]any{"status": 200, "body": map[string]any{"inline_string": "ok"}}, "typed_per_filter_config": disabledAuth()}}
	for _, name := range ServiceNames {
		u, err := url.Parse(backends[name].URL)
		if err != nil {
			return nil, err
		}
		port := 443
		if u.Scheme == "http" {
			port = 80
		}
		if u.Port() != "" {
			port, err = strconv.Atoi(u.Port())
			if err != nil {
				return nil, err
			}
		}
		cluster := map[string]any{"name": name, "type": "LOGICAL_DNS", "connect_timeout": "10s", "load_assignment": assignment(name, u.Hostname(), port)}
		if u.Scheme == "https" {
			cluster["transport_socket"] = map[string]any{"name": "envoy.transport_sockets.tls", "typed_config": map[string]any{"@type": "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext", "sni": u.Hostname(), "common_tls_context": map[string]any{"validation_context": map[string]any{"trusted_ca": map[string]any{"filename": "/etc/ssl/certs/ca-certificates.crt"}, "match_typed_subject_alt_names": []any{map[string]any{"san_type": "DNS", "matcher": map[string]any{"exact": u.Hostname()}}}}}}}
		}
		clusters = append(clusters, cluster)
	}
	for _, boundary := range Boundaries {
		u, _ := url.Parse(backends[boundary.Service].URL)
		routes = append(routes, map[string]any{"match": map[string]any{"path_separated_prefix": boundary.Prefix}, "route": map[string]any{"cluster": boundary.Service, "host_rewrite_literal": u.Host, "timeout": "320s"}})
	}
	routes = append(routes, map[string]any{"match": map[string]any{"prefix": "/"}, "direct_response": map[string]any{"status": 404}, "typed_per_filter_config": disabledAuth()})
	manager := map[string]any{
		"@type": "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager", "stat_prefix": "gateway", "normalize_path": true, "merge_slashes": false, "generate_request_id": true, "preserve_external_request_id": false, "use_remote_address": true, "skip_xff_append": true, "stream_idle_timeout": "330s", "request_headers_timeout": "15s",
		"route_config": map[string]any{"name": "services", "virtual_hosts": []any{map[string]any{"name": "application", "domains": []string{"*"}, "routes": routes, "response_headers_to_add": []any{map[string]any{"header": map[string]any{"key": "x-request-id", "value": "%REQ(X-REQUEST-ID)%"}}}}}},
		"http_filters": []any{map[string]any{"name": "envoy.filters.http.ext_authz", "typed_config": map[string]any{"@type": "type.googleapis.com/envoy.extensions.filters.http.ext_authz.v3.ExtAuthz", "transport_api_version": "V3", "grpc_service": map[string]any{"envoy_grpc": map[string]any{"cluster_name": "authorization"}, "timeout": "5s"}, "failure_mode_allow": false, "status_on_error": map[string]any{"code": 503}, "validate_mutations": true}}, map[string]any{"name": "envoy.filters.http.router", "typed_config": map[string]any{"@type": "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router"}}},
	}
	config := map[string]any{"static_resources": map[string]any{"listeners": []any{map[string]any{"name": "public", "address": socket("0.0.0.0", 8080), "filter_chains": []any{map[string]any{"filters": []any{map[string]any{"name": "envoy.filters.network.http_connection_manager", "typed_config": manager}}}}}}, "clusters": clusters}, "admin": map[string]any{"address": socket("127.0.0.1", 9901)}}
	return json.MarshalIndent(config, "", "  ")
}
func socket(host string, port int) map[string]any {
	return map[string]any{"socket_address": map[string]any{"address": host, "port_value": port}}
}
func assignment(name, host string, port int) map[string]any {
	return map[string]any{"cluster_name": name, "endpoints": []any{map[string]any{"lb_endpoints": []any{map[string]any{"endpoint": map[string]any{"address": socket(host, port)}}}}}}
}
func disabledAuth() map[string]any {
	return map[string]any{"envoy.filters.http.ext_authz": map[string]any{"@type": "type.googleapis.com/envoy.extensions.filters.http.ext_authz.v3.ExtAuthzPerRoute", "disabled": true}}
}
