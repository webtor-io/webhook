### Project Guidelines

#### Build and Configuration

This project is a Go-based webhook service. It uses `urfave/cli` for command-line interface management and depends on `webtor-io/common-services` for core functionalities like database connectivity and server scaffolding.

**Build Instructions:**
To build the project locally, run:
```bash
go build -o server
```
The `Dockerfile` provides the production build environment using a multi-stage build (alpine for certs, golang for build, and a final alpine image for the runtime).

**Environment Variables:**
Key configuration is handled via environment variables (which can also be passed as CLI flags):
- `PORT`: HTTP listening port (default: 8080)
- `HOST`: Listening host (default: "")
- `PATREON_SECRET`: Secret used for Patreon webhook signature validation.
- `POSTGRES_HOST`, `POSTGRES_PORT`, `POSTGRES_USER`, `POSTGRES_PASSWORD`, `POSTGRES_DB`: PostgreSQL connection details.

#### Testing

**Running Tests:**
To run all tests in the project:
```bash
go test ./...
```
To run tests for a specific package (e.g., `services`):
```bash
go test ./services/...
```

**Adding New Tests:**
- Use Go's standard `testing` package.
- Place test files in the same directory as the code being tested, with a `_test.go` suffix.
- For services that interact with the database, consider using mocks or a dedicated test database, as `common-services` expects a valid PG connection for many operations.

**Example Test:**
The following test demonstrates how to verify the Patreon signature validation logic:
```go
func TestPatreon_Validate(t *testing.T) {
    s := &Patreon{}
    secret := "test-secret"
    message := []byte(`{"data": "test"}`)
    
    mac := hmac.New(md5.New, []byte(secret))
    mac.Write(message)
    expectedMAC := mac.Sum(nil)

    if !s.validate(message, expectedMAC, []byte(secret)) {
        t.Errorf("Validation failed for valid signature")
    }
}
```

#### Additional Development Information

**Architecture:**
- **Main Entry:** `main.go` initializes the CLI app and calls `configure(app)`.
- **Configuration:** `configure.go` sets up the commands (e.g., `serve`, `migrate`).
- **Server Setup:** `serve.go` wires together the services (Postgres, Probe, Web, Patreon).
- **Services:** Located in the `services/` directory. Each service typically has a `RegisterFlags` function and a `NewService` constructor.
- **Models:** Database models are located in `models/`. They use `go-pg` tags for ORM mapping.

**Code Style:**
- Follow standard Go conventions (`gofmt`, `go lint`).
- Use `logrus` for logging.
- Error handling should use `github.com/pkg/errors` for wrapping context where appropriate.

**Database Migrations:**
Migrations are handled by `common-services` and located in the `migrations/` directory. They are automatically run on server start in `serve.go`.
