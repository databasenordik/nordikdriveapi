//go:build integration && envoyintegration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"google.golang.org/grpc"

	"nordik-drive-api/internal/gateway"
)

const envoyFixtureImage = "envoyproxy/envoy@sha256:93a82e7584cb8181ad408e4e49477a3ae1d775f698214841eda4e9309fc06a06"

func TestEnvoyServiceRoutingAndStreaming(t *testing.T) {
	db := platformFixture(t)
	pool, _ := db.DB()
	firstUpload := make(chan struct{}, 1)
	backends := map[string]gateway.Backend{}
	fixtureHost := os.Getenv("ENVOY_FIXTURE_HOST")
	if fixtureHost == "" {
		fixtureHost = "host.docker.internal"
	}
	for _, name := range gateway.ServiceNames {
		name := name
		listener, err := net.Listen("tcp", "0.0.0.0:0")
		if err != nil {
			t.Fatal(err)
		}
		server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/missing") {
				http.Error(w, name+" owns this 404", 404)
				return
			}
			if r.Method == "OPTIONS" {
				w.Header().Set("Access-Control-Allow-Origin", "http://localhost:3000")
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.WriteHeader(204)
				return
			}
			if strings.HasSuffix(r.URL.Path, "/download") {
				w.Header().Set("Content-Disposition", `attachment; filename="fixture.txt"`)
				w.Header().Set("Content-Type", "application/octet-stream")
				w.(http.Flusher).Flush()
				_, _ = w.Write([]byte("fixture-download"))
				return
			}
			var body bytes.Buffer
			buffer := make([]byte, 1024)
			n, err := r.Body.Read(buffer)
			if n > 0 {
				body.Write(buffer[:n])
				select {
				case firstUpload <- struct{}{}:
				default:
				}
			}
			if err == nil {
				_, _ = io.Copy(&body, r.Body)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"service": name, "path": r.URL.RequestURI(), "method": r.Method, "body": body.String(), "cookie": r.Header.Get("Cookie"), "authorization": r.Header.Get("Authorization"), "forged_identity": r.Header.Get("X-User-ID"), "forged_workload": r.Header.Get("X-Serverless-Authorization"), "retry_override": r.Header.Get("X-Envoy-Retry-On")})
		})}
		go func() { _ = server.Serve(listener) }()
		t.Cleanup(func() { _ = server.Close() })
		_, port, _ := net.SplitHostPort(listener.Addr().String())
		backends[name] = gateway.Backend{URL: "http://" + fixtureHost + ":" + port}
	}
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := gateway.Config{Backends: backends, JWTSecret: "synthetic-fixture-key", Local: true, Mode: "observe", ClientIPMode: "peer", AuthLimit: 20, PublicLimit: 120, UserLimit: 120, ChatLimit: 10}
	server := grpc.NewServer()
	authv3.RegisterAuthorizationServer(server, gateway.NewAuthorizer(cfg, gateway.PostgresLimiter{Pool: pool}, nil))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	data, err := gateway.Bootstrap(backends)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	// The production sidecar is localhost. This Docker fixture reaches the
	// host-side Go authorizer through Docker Desktop/the Linux host gateway.
	for _, value := range config["static_resources"].(map[string]any)["clusters"].([]any) {
		cluster := value.(map[string]any)
		if cluster["name"] == "authorization" {
			cluster["type"] = "LOGICAL_DNS"
			endpoint := cluster["load_assignment"].(map[string]any)["endpoints"].([]any)[0].(map[string]any)["lb_endpoints"].([]any)[0].(map[string]any)["endpoint"].(map[string]any)["address"].(map[string]any)["socket_address"].(map[string]any)
			endpoint["address"] = fixtureHost
			endpoint["port_value"] = listener.Addr().(*net.TCPAddr).Port
		}
	}
	data, err = json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	validationArgs := []string{"run", "--rm", envoyFixtureImage, "--mode", "validate", "--config-yaml", string(data)}
	if output, err := exec.Command("docker", validationArgs...).CombinedOutput(); err != nil {
		t.Fatalf("Envoy configuration validation: %v: %s", err, output)
	}
	name := fmt.Sprintf("nordik-envoy-fixture-%d", time.Now().UnixNano())
	args := []string{"run", "--detach", "--name", name}
	sharedNetwork := os.Getenv("ENVOY_FIXTURE_NETWORK")
	if sharedNetwork != "" {
		args = append(args, "--network", sharedNetwork)
	} else {
		args = append(args, "-p", "127.0.0.1::8080")
		if runtime.GOOS == "linux" {
			args = append(args, "--add-host=host.docker.internal:host-gateway")
		}
	}
	args = append(args, envoyFixtureImage, "--config-yaml", string(data), "--log-level", "warning")
	if output, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("start Envoy: %v: %s", err, output)
	}
	t.Cleanup(func() {
		if t.Failed() {
			output, _ := exec.Command("docker", "logs", name).CombinedOutput()
			t.Logf("Envoy fixture: %s", output)
		}
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})
	base := "http://127.0.0.1:8080"
	if sharedNetwork == "" {
		output, err := exec.Command("docker", "port", name, "8080/tcp").Output()
		if err != nil {
			t.Fatal(err)
		}
		base = "http://" + strings.TrimSpace(string(output))
	}
	client := &http.Client{Timeout: 10 * time.Second}
	deadline := time.Now().Add(20 * time.Second)
	for {
		response, err := client.Get(base + "/health")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("Envoy fixture never became ready")
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, tc := range []struct{ path, service string }{{"/api/user/new-endpoint", "identity"}, {"/api/file/new-endpoint?version=2", "records"}, {"/api/support-schedule/new-endpoint", "support"}, {"/api/chat/new-endpoint", "chat"}, {"/api/logs/new-endpoint", "audit"}} {
		request, _ := http.NewRequest("PATCH", base+tc.path, strings.NewReader("request-body"))
		request.AddCookie(fixtureCookie(t, 7))
		request.Header.Set("Authorization", "Bearer application-token")
		request.Header.Set("X-User-ID", "forged")
		request.Header.Set("X-Envoy-Retry-On", "5xx")
		request.Header.Set("X-Serverless-Authorization", "Bearer forged")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]string
		err = json.NewDecoder(response.Body).Decode(&result)
		response.Body.Close()
		if err != nil {
			t.Fatalf("decode %s status %d: %v", tc.path, response.StatusCode, err)
		}
		if result["service"] != tc.service || result["path"] != tc.path || result["method"] != "PATCH" || result["body"] != "request-body" || result["authorization"] != "Bearer application-token" || result["cookie"] == "" || result["forged_identity"] != "" || result["forged_workload"] != "" || result["retry_override"] != "" {
			t.Fatalf("proxy contract changed: %+v", result)
		}
	}
	for _, path := range []string{"/internal/log-level", "/api/internal/jobs/honour/run"} {
		response, err := client.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 404 {
			t.Fatal("internal endpoint exposed")
		}
	}
	request, _ := http.NewRequest("GET", base+"/api/file/missing", nil)
	request.AddCookie(fixtureCookie(t, 7))
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 404 || !strings.Contains(string(body), "records owns this 404") {
		t.Fatal("gateway took over service endpoint handling")
	}
	request, _ = http.NewRequest("OPTIONS", base+"/api/file/new-endpoint", nil)
	request.Header.Set("Origin", "http://localhost:3000")
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 204 || response.Header.Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatal("CORS preflight changed")
	}
	request, _ = http.NewRequest("GET", base+"/api/file/download", nil)
	request.AddCookie(fixtureCookie(t, 7))
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "fixture-download" || response.Header.Get("Content-Disposition") != `attachment; filename="fixture.txt"` {
		t.Fatal("download body/filename changed")
	}
	// A service receives bytes before the sender closes its body: ext_authz
	// must not force whole-upload buffering.
	for len(firstUpload) > 0 {
		<-firstUpload
	}
	reader, writer := io.Pipe()
	request, _ = http.NewRequest("POST", base+"/api/file/upload", reader)
	request.AddCookie(fixtureCookie(t, 7))
	done := make(chan error, 1)
	go func() {
		response, err := client.Do(request)
		if err == nil {
			_, err = io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
		done <- err
	}()
	if _, err := writer.Write(bytes.Repeat([]byte("x"), 32*1024)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstUpload:
	case <-time.After(3 * time.Second):
		_ = writer.Close()
		t.Fatal("gateway buffered the full upload")
	}
	_ = writer.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Stop the auth sidecar: the gateway must fail closed, not bypass it.
	server.Stop()
	request, _ = http.NewRequest("GET", base+"/api/lookup/province", nil)
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 503 {
		t.Fatalf("authorization outage returned %d, want 503", response.StatusCode)
	}
}
