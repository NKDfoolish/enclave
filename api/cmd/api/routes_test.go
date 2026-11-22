package main

// Integration tests: the real HTTP handler from routes(), real services, and real Postgres,
// Redis and object storage. Each test creates its own users and workspaces with unique
// emails and its own bucket, so tests never share data and can run in any order.
// Run them with `make test`, which sets the TEST_* variables (see platform/testenv).

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	documentapp "enclave/api/internal/document/app"
	documentinfra "enclave/api/internal/document/infra"
	identityapp "enclave/api/internal/identity/app"
	identityinfra "enclave/api/internal/identity/infra"
	"enclave/api/internal/platform/config"
	"enclave/api/internal/platform/objectstore"
	"enclave/api/internal/platform/outbox"
	"enclave/api/internal/platform/testenv"
	workspaceapp "enclave/api/internal/workspace/app"
	workspaceinfra "enclave/api/internal/workspace/infra"
)

const testPassword = "correct horse battery staple"

// testMaxDocumentBytes keeps the "too large" test cheap.
const testMaxDocumentBytes = 1 << 20

type testEnv struct {
	server    *httptest.Server
	pool      *pgxpool.Pool
	store     *objectstore.Store
	documents *documentapp.Service
	worker    *outbox.Worker
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	pool := testenv.Postgres(t)
	redisClient := testenv.Redis(t)
	store := testenv.ObjectStore(t, "enclave-test-"+strings.ToLower(rand.Text()[:12]))

	_, signingKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		JWTSigningKey:   signingKey,
		AccessTokenTTL:  15 * time.Minute,
		RefreshTokenTTL: time.Hour,
		SecureCookies:   false,
	}
	workspaces := workspaceapp.NewService(workspaceinfra.NewRepository(pool))
	documents := documentapp.NewService(workspaces, documentinfra.NewRepository(pool), store, testMaxDocumentBytes)
	svc := services{
		identity: identityapp.NewService(
			identityinfra.NewRepository(pool),
			identityinfra.NewRevocationStore(redisClient),
			identityapp.TokenConfig{SigningKey: cfg.JWTSigningKey, AccessTTL: cfg.AccessTokenTTL, RefreshTTL: cfg.RefreshTokenTTL},
		),
		workspace: workspaces,
		document:  documents,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := httptest.NewServer(routes(cfg, logger, svc))
	t.Cleanup(server.Close)

	worker := outbox.NewWorker(pool, logger)
	worker.Handle(documentapp.JobValidateUpload, documents.ValidateUpload)
	worker.Handle(documentapp.JobDeleteObject, documents.DeleteObject)

	return &testEnv{server: server, pool: pool, store: store, documents: documents, worker: worker}
}

// runJobs runs queued background jobs until none are due, like the worker in production.
func (env *testEnv) runJobs(t *testing.T) {
	t.Helper()
	for {
		processed, err := env.worker.RunOnce(context.Background())
		if err != nil {
			t.Fatalf("run outbox job: %v", err)
		}
		if !processed {
			return
		}
	}
}

// testUser is one person using the API with their own cookie jar.
type testUser struct {
	t      *testing.T
	env    *testEnv
	client *http.Client
	ID     string
	Email  string
}

func (env *testEnv) anonymous(t *testing.T) *testUser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("create cookie jar: %v", err)
	}
	return &testUser{t: t, env: env, client: &http.Client{
		Jar: jar,
		// Tests look at redirects (downloads) instead of following them.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// newUser registers and logs in a user with a unique email.
func (env *testEnv) newUser(t *testing.T, name string) *testUser {
	t.Helper()
	u := env.anonymous(t)
	u.Email = strings.ToLower(name + "-" + rand.Text()[:10] + "@example.com")

	status, body := u.do(http.MethodPost, "/v1/auth/register", map[string]string{
		"email": u.Email, "display_name": name, "password": testPassword,
	})
	wantStatus(t, "register "+name, status, http.StatusCreated, body)
	u.ID = body["id"].(string)

	status, body = u.do(http.MethodPost, "/v1/auth/login", map[string]string{
		"email": u.Email, "password": testPassword,
	})
	wantStatus(t, "login "+name, status, http.StatusOK, body)
	return u
}

// do sends a JSON request and decodes the JSON response (nil for empty bodies).
func (u *testUser) do(method, path string, payload any) (int, map[string]any) {
	u.t.Helper()
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			u.t.Fatalf("encode request: %v", err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(u.t.Context(), method, u.env.server.URL+path, body)
	if err != nil {
		u.t.Fatalf("build request: %v", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := u.client.Do(req)
	if err != nil {
		u.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var decoded map[string]any
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 0 && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			u.t.Fatalf("%s %s: decode response %q: %v", method, path, raw, err)
		}
	}
	return resp.StatusCode, decoded
}

func wantStatus(t *testing.T, step string, got, want int, body map[string]any) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: status = %d, want %d (body: %v)", step, got, want, body)
	}
}

func errorCode(body map[string]any) string {
	detail, _ := body["error"].(map[string]any)
	code, _ := detail["code"].(string)
	return code
}

func (u *testUser) createWorkspace(name string) string {
	u.t.Helper()
	status, body := u.do(http.MethodPost, "/v1/workspaces", map[string]string{"name": name})
	wantStatus(u.t, "create workspace", status, http.StatusCreated, body)
	return body["id"].(string)
}

// invite returns the invitation token.
func (u *testUser) invite(workspaceID, email, role string) string {
	u.t.Helper()
	status, body := u.do(http.MethodPost, "/v1/workspaces/"+workspaceID+"/invitations",
		map[string]string{"email": email, "role": role})
	wantStatus(u.t, "invite "+email, status, http.StatusCreated, body)
	return body["token"].(string)
}

func (u *testUser) accept(token string) (int, map[string]any) {
	u.t.Helper()
	return u.do(http.MethodPost, "/v1/invitations/accept", map[string]string{"token": token})
}

// addMember invites member with role and has them accept.
func (u *testUser) addMember(workspaceID string, member *testUser, role string) {
	u.t.Helper()
	status, body := member.accept(u.invite(workspaceID, member.Email, role))
	wantStatus(u.t, "accept invitation as "+role, status, http.StatusOK, body)
}

func TestAuthentication(t *testing.T) {
	env := newTestEnv(t)
	alice := env.newUser(t, "Alice")
	anonymous := env.anonymous(t)

	t.Run("me returns the logged-in user", func(t *testing.T) {
		status, body := alice.do(http.MethodGet, "/v1/me", nil)
		wantStatus(t, "me", status, http.StatusOK, body)
		if body["email"] != alice.Email {
			t.Fatalf("email = %v, want %s", body["email"], alice.Email)
		}
	})

	t.Run("anonymous requests are rejected", func(t *testing.T) {
		for _, path := range []string{"/v1/me", "/v1/workspaces"} {
			status, body := anonymous.do(http.MethodGet, path, nil)
			wantStatus(t, path, status, http.StatusUnauthorized, body)
		}
	})

	t.Run("email is unique regardless of case", func(t *testing.T) {
		status, body := anonymous.do(http.MethodPost, "/v1/auth/register", map[string]string{
			"email": strings.ToUpper(alice.Email), "display_name": "Copy", "password": testPassword,
		})
		wantStatus(t, "register duplicate", status, http.StatusConflict, body)
	})

	t.Run("short password is rejected", func(t *testing.T) {
		status, body := anonymous.do(http.MethodPost, "/v1/auth/register", map[string]string{
			"email": "short-" + rand.Text()[:8] + "@example.com", "display_name": "Short", "password": "123",
		})
		wantStatus(t, "register", status, http.StatusBadRequest, body)
	})

	t.Run("wrong password and unknown email look the same", func(t *testing.T) {
		for _, email := range []string{alice.Email, "nobody-" + rand.Text()[:8] + "@example.com"} {
			status, body := anonymous.do(http.MethodPost, "/v1/auth/login", map[string]string{
				"email": email, "password": "wrong password!",
			})
			wantStatus(t, "login "+email, status, http.StatusUnauthorized, body)
			if errorCode(body) != "invalid_credentials" {
				t.Fatalf("code = %s, want invalid_credentials", errorCode(body))
			}
		}
	})

	t.Run("form posts are rejected (CSRF)", func(t *testing.T) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, env.server.URL+"/v1/auth/login",
			strings.NewReader("email="+alice.Email+"&password="+testPassword))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("refresh rotates the tokens", func(t *testing.T) {
		dave := env.newUser(t, "Dave")
		before := dave.cookies()

		status, body := dave.do(http.MethodPost, "/v1/auth/refresh", nil)
		wantStatus(t, "refresh", status, http.StatusNoContent, body)
		after := dave.cookies()
		if after["enclave_access"] == before["enclave_access"] || after["enclave_refresh"] == before["enclave_refresh"] {
			t.Fatal("refresh must replace both cookies")
		}

		status, body = dave.do(http.MethodGet, "/v1/me", nil)
		wantStatus(t, "me with new access token", status, http.StatusOK, body)

		// The old refresh token again, right away: a race between tabs, not theft.
		replay := env.withCookies(t, before)
		status, body = replay.do(http.MethodPost, "/v1/auth/refresh", nil)
		wantStatus(t, "reuse old refresh token", status, http.StatusConflict, body)
	})

	t.Run("logout revokes the tokens on the server", func(t *testing.T) {
		bob := env.newUser(t, "Bob")
		stolen := bob.cookies()

		status, _ := bob.do(http.MethodPost, "/v1/auth/logout", nil)
		wantStatus(t, "logout", status, http.StatusNoContent, nil)

		// Replay copies of the cookies: the tokens must be dead server-side, not just deleted
		// from the browser.
		replay := env.withCookies(t, stolen)
		status, body := replay.do(http.MethodGet, "/v1/me", nil)
		wantStatus(t, "me with old access token", status, http.StatusUnauthorized, body)
		status, body = replay.do(http.MethodPost, "/v1/auth/refresh", nil)
		wantStatus(t, "refresh with old refresh token", status, http.StatusUnauthorized, body)
	})

	t.Run("forged access token is rejected", func(t *testing.T) {
		forged := env.withCookies(t, map[string]string{"enclave_access": "eyJhbGciOiJub25lIn0.e30."})
		status, body := forged.do(http.MethodGet, "/v1/me", nil)
		wantStatus(t, "me with forged token", status, http.StatusUnauthorized, body)
	})
}

func TestRolePermissions(t *testing.T) {
	env := newTestEnv(t)
	admin := env.newUser(t, "Admin")
	staff := env.newUser(t, "Staff")
	client := env.newUser(t, "Client")

	workspaceID := admin.createWorkspace("Acme")
	admin.addMember(workspaceID, staff, "staff")
	admin.addMember(workspaceID, client, "client")
	base := "/v1/workspaces/" + workspaceID

	t.Run("each member sees their own role", func(t *testing.T) {
		for user, role := range map[*testUser]string{admin: "admin", staff: "staff", client: "client"} {
			status, body := user.do(http.MethodGet, base, nil)
			wantStatus(t, "get workspace", status, http.StatusOK, body)
			if body["role"] != role {
				t.Fatalf("role = %v, want %s", body["role"], role)
			}
		}
	})

	t.Run("admin and staff can list members, client cannot", func(t *testing.T) {
		status, body := admin.do(http.MethodGet, base+"/members", nil)
		wantStatus(t, "admin", status, http.StatusOK, body)
		if n := len(body["members"].([]any)); n != 3 {
			t.Fatalf("members = %d, want 3", n)
		}
		status, body = staff.do(http.MethodGet, base+"/members", nil)
		wantStatus(t, "staff", status, http.StatusOK, body)
		status, body = client.do(http.MethodGet, base+"/members", nil)
		wantStatus(t, "client", status, http.StatusForbidden, body)
	})

	t.Run("only admins manage members and invitations", func(t *testing.T) {
		for _, user := range []*testUser{staff, client} {
			requests := []struct {
				method, path string
				payload      any
			}{
				{http.MethodPost, base + "/invitations", map[string]string{"email": "x@example.com", "role": "client"}},
				{http.MethodGet, base + "/invitations", nil},
				{http.MethodPatch, base + "/members/" + client.ID, map[string]string{"role": "admin"}},
				{http.MethodDelete, base + "/members/" + admin.ID, nil},
			}
			for _, r := range requests {
				status, body := user.do(r.method, r.path, r.payload)
				wantStatus(t, r.method+" "+r.path, status, http.StatusForbidden, body)
			}
		}
	})

	t.Run("role change takes effect on the next request", func(t *testing.T) {
		status, body := admin.do(http.MethodPatch, base+"/members/"+client.ID, map[string]string{"role": "staff"})
		wantStatus(t, "promote client", status, http.StatusNoContent, body)

		status, body = client.do(http.MethodGet, base+"/members", nil)
		wantStatus(t, "list members after promotion", status, http.StatusOK, body)
	})

	t.Run("removed member loses access with the same session", func(t *testing.T) {
		status, body := admin.do(http.MethodDelete, base+"/members/"+staff.ID, nil)
		wantStatus(t, "remove staff", status, http.StatusNoContent, body)

		status, body = staff.do(http.MethodGet, base, nil)
		wantStatus(t, "get workspace after removal", status, http.StatusNotFound, body)
	})

	t.Run("workspace always keeps an admin", func(t *testing.T) {
		status, body := admin.do(http.MethodPatch, base+"/members/"+admin.ID, map[string]string{"role": "staff"})
		wantStatus(t, "demote last admin", status, http.StatusConflict, body)
		status, body = admin.do(http.MethodDelete, base+"/members/"+admin.ID, nil)
		wantStatus(t, "remove last admin", status, http.StatusConflict, body)

		status, body = admin.do(http.MethodPatch, base+"/members/"+client.ID, map[string]string{"role": "admin"})
		wantStatus(t, "promote second admin", status, http.StatusNoContent, body)
		status, body = admin.do(http.MethodPatch, base+"/members/"+admin.ID, map[string]string{"role": "staff"})
		wantStatus(t, "demote now that another admin exists", status, http.StatusNoContent, body)
	})

	t.Run("invalid role is rejected", func(t *testing.T) {
		// The subtests above run in order: client is now the only admin.
		status, body := client.do(http.MethodPatch, base+"/members/"+admin.ID, map[string]string{"role": "owner"})
		wantStatus(t, "set role owner", status, http.StatusBadRequest, body)
	})
}

func TestTenantIsolation(t *testing.T) {
	env := newTestEnv(t)
	alice := env.newUser(t, "Alice")
	bob := env.newUser(t, "Bob")

	aliceWorkspace := alice.createWorkspace("Alice Co")
	bobWorkspace := bob.createWorkspace("Bob Co")
	alice.invite(aliceWorkspace, "pending-"+rand.Text()[:8]+"@example.com", "client")
	_, invitations := alice.do(http.MethodGet, "/v1/workspaces/"+aliceWorkspace+"/invitations", nil)
	aliceInvitationID := invitations["invitations"].([]any)[0].(map[string]any)["id"].(string)

	t.Run("workspace list contains only own workspaces", func(t *testing.T) {
		_, body := alice.do(http.MethodGet, "/v1/workspaces", nil)
		for _, w := range body["workspaces"].([]any) {
			if w.(map[string]any)["id"] == bobWorkspace {
				t.Fatal("alice can see bob's workspace")
			}
		}
	})

	t.Run("another tenant's IDs look like they do not exist", func(t *testing.T) {
		alicePath := "/v1/workspaces/" + aliceWorkspace
		requests := []struct {
			name, method, path string
			payload            any
		}{
			{"get workspace", http.MethodGet, alicePath, nil},
			{"list members", http.MethodGet, alicePath + "/members", nil},
			{"list invitations", http.MethodGet, alicePath + "/invitations", nil},
			{"invite", http.MethodPost, alicePath + "/invitations", map[string]string{"email": bob.Email, "role": "admin"}},
			{"change role", http.MethodPatch, alicePath + "/members/" + alice.ID, map[string]string{"role": "client"}},
			{"remove member", http.MethodDelete, alicePath + "/members/" + alice.ID, nil},
			{"revoke invitation", http.MethodDelete, alicePath + "/invitations/" + aliceInvitationID, nil},
			// Bob's own workspace path with Alice's invitation ID: IDs never cross tenants.
			{"revoke foreign invitation", http.MethodDelete, "/v1/workspaces/" + bobWorkspace + "/invitations/" + aliceInvitationID, nil},
			{"change role of non-member", http.MethodPatch, "/v1/workspaces/" + bobWorkspace + "/members/" + alice.ID, map[string]string{"role": "client"}},
			{"random workspace ID", http.MethodGet, "/v1/workspaces/" + uuid.NewString(), nil},
			{"malformed workspace ID", http.MethodGet, "/v1/workspaces/123", nil},
		}
		for _, r := range requests {
			status, body := bob.do(r.method, r.path, r.payload)
			wantStatus(t, r.name, status, http.StatusNotFound, body)
		}
	})

	t.Run("alice's workspace is unchanged", func(t *testing.T) {
		status, body := alice.do(http.MethodGet, "/v1/workspaces/"+aliceWorkspace, nil)
		wantStatus(t, "get workspace", status, http.StatusOK, body)
		if body["role"] != "admin" {
			t.Fatalf("role = %v, want admin", body["role"])
		}
		_, body = alice.do(http.MethodGet, "/v1/workspaces/"+aliceWorkspace+"/invitations", nil)
		if n := len(body["invitations"].([]any)); n != 1 {
			t.Fatalf("invitations = %d, want 1", n)
		}
	})
}

func TestInvitations(t *testing.T) {
	env := newTestEnv(t)
	admin := env.newUser(t, "Admin")
	workspaceID := admin.createWorkspace("Acme")
	invitations := "/v1/workspaces/" + workspaceID + "/invitations"

	t.Run("invitation is bound to the invited email", func(t *testing.T) {
		carol := env.newUser(t, "Carol")
		mallory := env.newUser(t, "Mallory")
		token := admin.invite(workspaceID, carol.Email, "staff")

		status, body := mallory.accept(token)
		wantStatus(t, "accept as someone else", status, http.StatusForbidden, body)
		status, body = carol.accept(token)
		wantStatus(t, "accept as invitee", status, http.StatusOK, body)
	})

	t.Run("invitation works only once", func(t *testing.T) {
		dave := env.newUser(t, "Dave")
		token := admin.invite(workspaceID, dave.Email, "client")
		status, body := dave.accept(token)
		wantStatus(t, "first accept", status, http.StatusOK, body)
		status, body = dave.accept(token)
		wantStatus(t, "second accept", status, http.StatusNotFound, body)
	})

	t.Run("one pending invitation per email", func(t *testing.T) {
		email := "erin-" + rand.Text()[:8] + "@example.com"
		admin.invite(workspaceID, email, "client")
		status, body := admin.do(http.MethodPost, invitations, map[string]string{"email": email, "role": "staff"})
		wantStatus(t, "second invite", status, http.StatusConflict, body)
	})

	t.Run("existing members cannot be invited", func(t *testing.T) {
		status, body := admin.do(http.MethodPost, invitations, map[string]string{"email": admin.Email, "role": "staff"})
		wantStatus(t, "invite self", status, http.StatusConflict, body)
		if errorCode(body) != "already_member" {
			t.Fatalf("code = %s, want already_member", errorCode(body))
		}
	})

	t.Run("invalid email or role is rejected", func(t *testing.T) {
		for _, payload := range []map[string]string{
			{"email": "not-an-email", "role": "staff"},
			{"email": "ok-" + rand.Text()[:8] + "@example.com", "role": "owner"},
		} {
			status, body := admin.do(http.MethodPost, invitations, payload)
			wantStatus(t, "invite "+payload["email"], status, http.StatusBadRequest, body)
		}
	})

	t.Run("revoked invitation cannot be accepted", func(t *testing.T) {
		frank := env.newUser(t, "Frank")
		token := admin.invite(workspaceID, frank.Email, "client")
		_, list := admin.do(http.MethodGet, invitations, nil)
		var invitationID string
		for _, inv := range list["invitations"].([]any) {
			if inv.(map[string]any)["email"] == frank.Email {
				invitationID = inv.(map[string]any)["id"].(string)
			}
		}

		status, body := admin.do(http.MethodDelete, invitations+"/"+invitationID, nil)
		wantStatus(t, "revoke", status, http.StatusNoContent, body)
		status, body = frank.accept(token)
		wantStatus(t, "accept revoked", status, http.StatusNotFound, body)
	})

	t.Run("expired invitation cannot be accepted and can be re-sent", func(t *testing.T) {
		grace := env.newUser(t, "Grace")
		token := admin.invite(workspaceID, grace.Email, "client")
		_, err := env.pool.Exec(context.Background(),
			`UPDATE invitations SET expires_at = now() - interval '1 minute' WHERE email = $1`, grace.Email)
		if err != nil {
			t.Fatal(err)
		}

		status, body := grace.accept(token)
		wantStatus(t, "accept expired", status, http.StatusNotFound, body)

		status, body = grace.accept(admin.invite(workspaceID, grace.Email, "client"))
		wantStatus(t, "accept re-sent invitation", status, http.StatusOK, body)
	})
}

// cookies returns the user's cookies by name, including the refresh cookie scoped to /v1/auth.
func (u *testUser) cookies() map[string]string {
	u.t.Helper()
	values := map[string]string{}
	for _, path := range []string{"/", "/v1/auth/"} {
		for _, c := range u.client.Jar.Cookies(mustParseURL(u.t, u.env.server.URL+path)) {
			values[c.Name] = c.Value
		}
	}
	return values
}

// withCookies returns a new client holding copies of the given cookies, like an attacker
// who copied them, or a second browser tab.
func (env *testEnv) withCookies(t *testing.T, cookies map[string]string) *testUser {
	t.Helper()
	u := env.anonymous(t)
	base := mustParseURL(t, env.server.URL)
	for name, value := range cookies {
		path := "/"
		if name == "enclave_refresh" {
			path = "/v1/auth"
		}
		u.client.Jar.SetCookies(base, []*http.Cookie{{Name: name, Value: value, Path: path}})
	}
	return u
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
