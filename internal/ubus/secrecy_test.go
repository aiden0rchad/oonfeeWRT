package ubus

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
)

func clientForServer(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	c := New(Options{Host: strings.TrimPrefix(server.URL, "http://")})
	t.Cleanup(c.Close)
	return c
}

func assertSecretsAbsent(t *testing.T, surfaced string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if secret != "" && strings.Contains(surfaced, secret) {
			t.Fatalf("credential/session marker leaked through error text: %q", surfaced)
		}
	}
}

func TestSafeTransportErrorClassifiesOnlyWrappedRouteFailures(t *testing.T) {
	const secretURL = "http://credential-marker.invalid/ubus"
	for _, cause := range []error{syscall.EHOSTUNREACH, syscall.ENETUNREACH} {
		err := safeTransportError(context.Background(), &url.Error{
			Op: "Post", URL: secretURL,
			Err: &net.OpError{Op: "dial", Net: "tcp", Err: cause},
		})
		if got, want := err.Error(), "ubus transport: controller host has no usable route to the device"; got != want {
			t.Fatalf("route error = %q, want %q", got, want)
		}
		assertSecretsAbsent(t, err.Error(), secretURL)
	}

	// A peer can put the same words in a parser error. Strings are never
	// classified; only an error chain carrying the OS errno is trusted.
	remote := safeTransportError(context.Background(), &url.Error{
		Op: "Post", URL: secretURL,
		Err: fmt.Errorf("peer reflected no route to host and credential-marker"),
	})
	if got := remote.Error(); got != "ubus transport: request failed" {
		t.Fatalf("remote text crossed the transport boundary: %q", got)
	}
	assertSecretsAbsent(t, remote.Error(), secretURL, "credential-marker")
}

func TestLoginHTTPErrorNeverReflectsRequestBytes(t *testing.T) {
	const (
		user      = "not-a-real-operator-marker"
		password  = "not-a-real-password-marker-9f3a"
		fakeToken = "not-a-real-session-marker-84cb"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// Both header and body are attacker-controlled reflection channels.
		w.Header().Set("Content-Type", "application/"+password)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write(append(body, []byte(fakeToken)...))
	}))
	defer server.Close()

	err := clientForServer(t, server).Login(context.Background(), user, password)
	if err == nil {
		t.Fatal("reflected HTTP failure reported login success")
	}
	// This is the same wrapping path adoption/API errors and structured log
	// attributes use: once Error() is safe, wrapping must not reintroduce bytes.
	surfaced := fmt.Errorf("could not sign in to device: %w", err).Error()
	assertSecretsAbsent(t, surfaced, user, password, fakeToken, nullSession)
	for _, want := range []string{"HTTP 401", "other content type", "body"} {
		if !strings.Contains(surfaced, want) {
			t.Errorf("safe diagnostic %q missing from %q", want, surfaced)
		}
	}
}

func TestLoginJSONRPCErrorNeverReflectsRequestBytes(t *testing.T) {
	const (
		user     = "jsonrpc-user-marker"
		password = "not-a-real-jsonrpc-marker-53a1"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1,
			"error": map[string]any{"code": rpcErrInternal, "message": string(body)},
		})
	}))
	defer server.Close()

	err := clientForServer(t, server).Login(context.Background(), user, password)
	if err == nil {
		t.Fatal("reflected JSON-RPC failure reported login success")
	}
	assertSecretsAbsent(t, err.Error(), user, password, nullSession)
	if !strings.Contains(err.Error(), "session.login: JSON-RPC error response") {
		t.Fatalf("fixed JSON-RPC diagnostic missing: %v", err)
	}
}

func TestLoginDecodeErrorsNeverQuoteResponseJSON(t *testing.T) {
	const password = "not-a-real-decode-marker-493f"
	for _, tc := range []struct {
		name string
		body string
	}{
		{"malformed envelope", `{"reflected":"` + password},
		{"wrong session type", `{"jsonrpc":"2.0","id":1,"result":[0,{"ubus_rpc_session":{"reflected":"` + password + `"}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()

			err := clientForServer(t, server).Login(context.Background(), "root", password)
			if err == nil {
				t.Fatal("malicious JSON reported login success")
			}
			assertSecretsAbsent(t, err.Error(), password)
			if !strings.Contains(err.Error(), "byte") {
				t.Fatalf("bounded JSON location diagnostic missing: %v", err)
			}
		})
	}
}

func TestLoginNeverFollowsPasswordBearingRedirect(t *testing.T) {
	const password = "not-a-real-redirect-marker-736e"
	var reached atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached.Add(1)
	}))
	defer target.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Location", target.URL+"/capture/"+url.PathEscape(password))
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	err := clientForServer(t, origin).Login(context.Background(), "root", password)
	if err == nil {
		t.Fatal("redirected login reported success")
	}
	if got := reached.Load(); got != 0 {
		t.Fatalf("password-bearing POST followed a redirect to %d target request(s)", got)
	}
	assertSecretsAbsent(t, err.Error(), password)
	if !strings.Contains(err.Error(), "HTTP 307") {
		t.Fatalf("redirect was not reported as fixed HTTP metadata: %v", err)
	}
}

func TestLoginTransportParserErrorNeverReflectsResponseBytes(t *testing.T) {
	const password = "not-a-real-status-line-marker-01de"
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			done <- err
			return
		}
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
		// net/http ordinarily quotes this attacker-controlled status token in
		// its parser error. The transport boundary must replace that error.
		_, err = fmt.Fprintf(conn, "HTTP/1.1 %s reflected\r\nContent-Length: 0\r\n\r\n", password)
		done <- err
	}()

	client := New(Options{Host: listener.Addr().String()})
	defer client.Close()
	err = client.Login(context.Background(), "root", password)
	if err == nil {
		t.Fatal("malformed reflected status line reported login success")
	}
	assertSecretsAbsent(t, err.Error(), password)
	if err.Error() != "ubus transport: request failed" {
		t.Fatalf("transport parser detail crossed the safe boundary: %v", err)
	}
	if serveErr := <-done; serveErr != nil {
		t.Fatalf("malicious fixture: %v", serveErr)
	}
}

func TestAuthenticatedCallErrorsNeverReflectSessionOrArguments(t *testing.T) {
	const (
		password = "not-a-real-stored-marker-24cd"
		token    = "not-a-real-session-marker-3b610afe"
		argument = "call-argument-marker-b770"
	)
	for _, tc := range []struct {
		name  string
		reply func(http.ResponseWriter, []byte)
	}{
		{
			name: "HTTP body",
			reply: func(w http.ResponseWriter, body []byte) {
				w.Header().Set("Content-Type", "application/"+token)
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write(body)
			},
		},
		{
			name: "JSON-RPC message",
			reply: func(w http.ResponseWriter, body []byte) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0", "id": 2,
					"error": map[string]any{"code": rpcErrInternal, "message": string(body)},
				})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if requests.Add(1) == 1 {
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w,
						`{"jsonrpc":"2.0","id":1,"result":[0,{"ubus_rpc_session":%q,"timeout":300}]}`,
						token)
					return
				}
				tc.reply(w, body)
			}))
			defer server.Close()

			client := clientForServer(t, server)
			if err := client.Login(context.Background(), "root", password); err != nil {
				t.Fatalf("fixture login: %v", err)
			}
			err := client.Call(context.Background(), "test", "reflect",
				map[string]string{"marker": argument}, nil)
			if err == nil {
				t.Fatal("malicious authenticated response reported success")
			}
			assertSecretsAbsent(t, err.Error(), password, token, argument)
		})
	}
}

func TestSuccessfulResponseBodyIsBoundedBeforeJSONDecode(t *testing.T) {
	const token = "not-a-real-session-marker-bounded"
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		if requests.Add(1) == 1 {
			_, _ = fmt.Fprintf(w,
				`{"jsonrpc":"2.0","id":1,"result":[0,{"ubus_rpc_session":%q,"timeout":300}]}`,
				token)
			return
		}
		_, _ = io.CopyN(w, strings.NewReader(strings.Repeat("x", maxSuccessResponseBytes+1)),
			maxSuccessResponseBytes+1)
	}))
	defer server.Close()

	client := clientForServer(t, server)
	if err := client.Login(context.Background(), "root", "placeholder"); err != nil {
		t.Fatal(err)
	}
	err := client.Call(context.Background(), "test", "oversized", struct{}{}, nil)
	if err == nil || !strings.Contains(err.Error(), "response body exceeds") {
		t.Fatalf("oversized successful body error=%v", err)
	}
	assertSecretsAbsent(t, err.Error(), token)
}

func TestBatchFailureDropsDeviceControlledPayload(t *testing.T) {
	const token = "not-a-real-batch-session-a018"
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		if requests.Add(1) == 1 {
			_, _ = fmt.Fprintf(w,
				`{"jsonrpc":"2.0","id":1,"result":[0,{"ubus_rpc_session":%q,"timeout":300}]}`,
				token)
			return
		}
		_, _ = fmt.Fprintf(w,
			`[{"jsonrpc":"2.0","id":2,"result":[6,{"reflected":%q}]}]`, token)
	}))
	defer server.Close()

	client := clientForServer(t, server)
	if err := client.Login(context.Background(), "root", "password"); err != nil {
		t.Fatalf("fixture login: %v", err)
	}
	results, err := client.Batch(context.Background(), []Invocation{{
		Object: "test", Method: "denied", Args: struct{}{},
	}})
	if err != nil {
		t.Fatalf("batch transport: %v", err)
	}
	if len(results) != 1 || results[0].Err == nil {
		t.Fatalf("denied batch result = %+v", results)
	}
	if len(results[0].Data) != 0 {
		t.Fatalf("error result retained %d device-controlled payload bytes", len(results[0].Data))
	}
	assertSecretsAbsent(t, results[0].Err.Error(), token)
}

func TestBatchDecodeErrorNeverQuotesResponseJSON(t *testing.T) {
	const token = "not-a-real-batch-decode-a019"
	result := Result{Data: json.RawMessage(`{"reflected":"` + token)}
	var out map[string]any
	err := result.Decode(&out)
	if err == nil {
		t.Fatal("malformed batch payload decoded successfully")
	}
	assertSecretsAbsent(t, err.Error(), token)
	if !strings.Contains(err.Error(), "byte") {
		t.Fatalf("bounded JSON location diagnostic missing: %v", err)
	}
}
