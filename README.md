Nordik Drive API
Go backend for the Nordik Drive data management platform.

The API supports file management, document handling, forms, and the main features used by the web application. It also connects the application to NIA chat.
What the project does
The platform lets users work with uploaded files and their records through one application. File version history helps users work with earlier versions when needed.

The backend is split into feature modules. Each module handles a specific part of the application, while shared helpers support tasks used in several places.
Main features
Area	Features
Files	Uploads, file listings, and file metadata.
Versions	File replacement, version history, and restoring earlier versions.
Records	Data retrieval and preparation for the web application.
Documents	Photo and document handling.
Forms	Forms and saved submissions.
Support	Support request and scheduling features.
NIA	Chat support within the application.


Technology
Tool	Used for
Go 1.25	Backend application.
Gin	HTTP routing and handlers.
GORM and PostgreSQL	Database access and application data.
Google Cloud Storage	Media and document storage.
Vertex AI	Model-based features.
gRPC	Communication with the separate NIA backend when configured.
Docker	Container builds.


Code structure
HTTP controllers handle requests and responses. Services contain the feature logic and use GORM for database access. The application entry point connects the modules and starts the server.

This keeps file, form, chat, and support code in separate parts of the project.
Location	Purpose
cmd/server/	Application entry point.
config/	Configuration loading.
internal/file/	File and version features.
internal/formsubmission/	Form features.
internal/chat/	Chat integration.
internal/util/	Shared utilities.
db-init/	Database setup files.
makefile	Test commands.


Local development
You need Go 1.25, PostgreSQL, and the supporting services required for the features you want to use. Docker is useful for running development dependencies.
git clone https://github.com/databasenordik/nordikdriveapi.git
cd nordikdriveapi
go mod download
Prepare a development database and your local service configuration. Then start the server:
go run ./cmd/server
The default HTTP port is 8080.

The Go process reads its configuration from the environment. A local database connection alone does not configure the cloud, chat, or email integrations.

Use generated sample records and a separate development database when trying features.
Selected application endpoints
These endpoints cover common file and chat features. They require the application's normal user access; they are not anonymous public content endpoints.
Method	Path	Purpose
GET	/api/file	List files available through the application.
POST	/api/file/upload	Upload a file.
GET	/api/file/history	Get file version history.
POST	/api/file/replace	Replace a file.
POST	/api/chat	Send a chat question.


The web application supplies the session and request fields needed for these operations. This table gives a feature overview rather than a complete API contract.
Chat support
The project includes a Go chat implementation and an adapter for a separate NIA service. The active backend depends on the application's development configuration.

Chat requests and interface features should be checked with the matching backend. Some features, such as persisted conversation history, depend on the separate NIA service.
Tests
Run the Makefile targets:
make test-unit
make test-integration
Both targets currently run the same Go test command. They do not select separate test groups.

To run the test command directly:
go test ./... -count=1
Tests cover application modules and shared helpers. Optional service integration checks need their own test setup.
Build
go build -o nordik-drive-api ./cmd/server
Build the container image:
docker build -t nordik-drive-api:local .
The image needs development service configuration before it can run the full application.
Related project
The Nordik Drive frontend provides the user interface for this API.
