package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type WorkerLauncher interface {
	Launch(context.Context, string) error
}

type CommandWorkerLauncher struct {
	SystemctlBinary string
	UnitPrefix      string
}

func (launcher CommandWorkerLauncher) Launch(ctx context.Context, operationID string) error {
	if !uuidPattern.MatchString(operationID) {
		return errors.New("invalid worker operation id")
	}
	if launcher.SystemctlBinary == "" || !filepath.IsAbs(launcher.SystemctlBinary) || launcher.UnitPrefix != "nexus-updater-worker@" {
		return errors.New("worker launcher configuration is invalid")
	}
	dispatchContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return runFixedCommand(dispatchContext, launcher.SystemctlBinary, []string{"start", "--no-block", launcher.UnitPrefix + operationID + ".service"})
}

type StatusData struct {
	Available          bool              `json:"available"`
	Installed          bool              `json:"installed"`
	ErrorCode          string            `json:"error_code,omitempty"`
	InstalledReleaseID string            `json:"installed_release_id,omitempty"`
	PreviousReleaseID  string            `json:"previous_release_id,omitempty"`
	UpdaterVersion     string            `json:"updater_version"`
	Channel            string            `json:"channel"`
	Profile            Profile           `json:"profile,omitempty"`
	Platform           string            `json:"platform,omitempty"`
	Host               HostStatus        `json:"host"`
	Components         []ComponentStatus `json:"components"`
	Operations         []OperationData   `json:"operations,omitempty"`
	Cleanup            CleanupPreview    `json:"cleanup"`
}

type HostStatus struct {
	DisplayName       string   `json:"display_name"`
	Profile           Profile  `json:"profile,omitempty"`
	Location          string   `json:"location"`
	ManagementMode    string   `json:"management_mode"`
	CapabilityVersion string   `json:"capability_version"`
	Capabilities      []string `json:"capabilities"`
	LastContactAt     string   `json:"last_contact_at"`
}

type ComponentStatus struct {
	ID                 Component `json:"component_id"`
	Label              string    `json:"label"`
	Required           bool      `json:"required"`
	Installed          bool      `json:"installed"`
	Enabled            bool      `json:"enabled"`
	Version            string    `json:"installed_version,omitempty"`
	ReleaseID          string    `json:"release_id,omitempty"`
	RuntimeState       string    `json:"runtime_state"`
	HealthState        string    `json:"health_state"`
	ConfigurationState string    `json:"configuration_state"`
	Reachable          bool      `json:"reachable"`
	HostLocation       string    `json:"host_location"`
	AvailableActions   []string  `json:"available_actions"`
}

type UpdateData struct {
	InstalledRelease string        `json:"installed_release,omitempty"`
	LatestRelease    string        `json:"latest_release,omitempty"`
	UpdateAvailable  bool          `json:"update_available"`
	Releases         []ReleaseInfo `json:"releases"`
	ReleaseNotes     string        `json:"release_notes,omitempty"`
	PublishedAt      time.Time     `json:"published_at,omitempty"`
}

type DoctorCheck struct {
	Name    string `json:"name"`
	Passed  bool   `json:"passed"`
	Message string `json:"message"`
}

type DoctorData struct {
	Healthy bool          `json:"healthy"`
	Checks  []DoctorCheck `json:"checks"`
}

type CleanupPreview struct {
	ReclaimableBytes int64    `json:"reclaimable_bytes"`
	Paths            []string `json:"entries"`
}

type Server struct {
	config      Config
	store       *OperationStore
	locks       *LockSet
	launcher    WorkerLauncher
	releases    ReleaseProvider
	listener    net.Listener
	connections chan struct{}
}

func NewServer(config Config, launcher WorkerLauncher) (*Server, error) {
	return newServer(config, launcher, NewGitHubReleaseClient())
}

func newServer(config Config, launcher WorkerLauncher, releases ReleaseProvider) (*Server, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	store, err := NewOperationStore(config.Paths)
	if err != nil {
		return nil, err
	}
	locks, err := NewLockSet(config.Paths)
	if err != nil {
		return nil, err
	}
	if launcher == nil {
		launcher = CommandWorkerLauncher{SystemctlBinary: config.SystemctlBinary, UnitPrefix: config.WorkerUnitPrefix}
	}
	if releases == nil {
		releases = NewGitHubReleaseClient()
	}
	return &Server{config: config, store: store, locks: locks, launcher: launcher, releases: releases, connections: make(chan struct{}, 32)}, nil
}

func (server *Server) Store() *OperationStore {
	return server.store
}

func (server *Server) ListenAndServe(ctx context.Context) error {
	if server.listener != nil {
		return errors.New("server is already listening")
	}
	if err := os.MkdirAll(server.config.Paths.RuntimeDir, 0750); err != nil {
		return err
	}
	if info, err := os.Lstat(server.config.Paths.SocketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("configured updater socket exists and is not a socket")
		}
		if err := os.Remove(server.config.Paths.SocketPath); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", server.config.Paths.SocketPath)
	if err != nil {
		return err
	}
	return server.serve(ctx, listener)
}

func (server *Server) Serve(ctx context.Context, listener net.Listener) error {
	if server.listener != nil || listener == nil {
		return errors.New("a single listener is required")
	}
	return server.serve(ctx, listener)
}

func (server *Server) serve(ctx context.Context, listener net.Listener) error {
	server.listener = listener
	if err := os.Chmod(server.config.Paths.SocketPath, server.config.SocketMode); err != nil {
		_ = listener.Close()
		return err
	}
	if server.config.SocketGroupGID >= 0 {
		if err := os.Chown(server.config.Paths.SocketPath, 0, server.config.SocketGroupGID); err != nil {
			_ = listener.Close()
			return err
		}
	}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	for {
		connection, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case server.connections <- struct{}{}:
			go func() {
				defer func() { <-server.connections }()
				server.handleConnection(connection)
			}()
		default:
			_ = WriteFrame(connection, errorResponse("", "busy", "the updater has too many active clients"), MaxResponseSize)
			_ = connection.Close()
		}
	}
}

func (server *Server) Close() error {
	if server.listener == nil {
		return nil
	}
	err := server.listener.Close()
	server.listener = nil
	return err
}

func (server *Server) handleConnection(connection net.Conn) {
	defer connection.Close()
	identity, err := unixPeerIdentity(connection)
	if err != nil || !server.allowedPeer(identity) {
		_ = WriteFrame(connection, errorResponse("", "permission_denied", "local peer is not authorized"), MaxResponseSize)
		return
	}
	_ = serveRequest(connection, server.handleRequest)
}

func (server *Server) allowedPeer(identity peerIdentity) bool {
	for _, uid := range server.config.AllowedUIDs {
		if identity.UID == uid {
			return true
		}
	}
	for _, gid := range server.config.AllowedGIDs {
		if identity.GID == gid {
			return true
		}
	}
	return false
}

func (server *Server) handleRequest(request Request) Response {
	if err := request.Validate(); err != nil {
		return errorResponse(request.ID, "invalid_request", safeProtocolMessage(err))
	}
	switch request.Operation {
	case OperationGetStatus:
		return server.dataResponse(request.ID, server.statusData())
	case OperationCheckUpdates:
		data, err := server.checkUpdates(context.Background())
		if err != nil {
			return errorResponse(request.ID, "update_check_failed", safeProtocolMessage(err))
		}
		return server.dataResponse(request.ID, data)
	case OperationDoctor:
		return server.dataResponse(request.ID, server.doctorData())
	case OperationGetOperation:
		state, err := server.store.Load(request.OperationID)
		if err != nil {
			return errorResponse(request.ID, "operation_not_found", "operation state is unavailable")
		}
		return server.dataResponse(request.ID, operationResponseData(state))
	case OperationListComponents:
		return server.dataResponse(request.ID, server.componentData())
	}
	if !mutatingOperation(request.Operation) {
		return errorResponse(request.ID, "operation_unavailable", "operation is not available")
	}
	if _, err := LoadInstallation(server.config.Paths); err != nil {
		return errorResponse(request.ID, "not_installed", "Nexus must be installed before this operation is available")
	}
	state, existing, err := server.store.CreateOrGet(context.Background(), request)
	if err != nil {
		return errorResponse(request.ID, "operation_rejected", safeProtocolMessage(err))
	}
	if existing {
		data, _ := json.Marshal(operationResponseData(state))
		return Response{Version: ProtocolVersion, ID: request.ID, Accepted: state.Status == OperationQueued || state.Status == OperationRunning, OperationID: state.ID, Data: data}
	}
	if err := writeOperationConfiguration(server.config.Paths, state.ID, request.Configuration); err != nil {
		_ = failOperationWithStatus(server.store, state.ID, OperationFailed, PhaseCleaningUp, "configuration_handoff_failed", "component configuration could not be handed to the worker")
		return errorResponse(request.ID, "configuration_handoff_failed", "component configuration could not be handed to the worker")
	}
	if err := server.launcher.Launch(context.Background(), state.ID); err != nil {
		removeOperationConfiguration(server.config.Paths, state.ID)
		_ = failOperationWithStatus(server.store, state.ID, OperationFailed, PhaseCleaningUp, "worker_dispatch_failed", "worker dispatch failed")
		return errorResponse(request.ID, "worker_dispatch_failed", "worker dispatch failed")
	}
	data, _ := json.Marshal(operationResponseData(state))
	return Response{Version: ProtocolVersion, ID: request.ID, Accepted: true, OperationID: state.ID, Data: data}
}

type OperationData struct {
	OperationID  string         `json:"operation_id"`
	Operation    Operation      `json:"operation"`
	Component    Component      `json:"component_id,omitempty"`
	Status       string         `json:"status"`
	ErrorCode    string         `json:"error_code,omitempty"`
	ErrorMessage string         `json:"error_message,omitempty"`
	Releases     []string       `json:"releases,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
	StartedAt    *time.Time     `json:"started_at,omitempty"`
	FinishedAt   *time.Time     `json:"finished_at,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

func operationResponseData(state OperationState) OperationData {
	status := string(state.Status)
	if state.Status == OperationQueued {
		status = "accepted"
	} else if state.Status == OperationRunning {
		status = string(state.Phase)
	}
	metadata := map[string]any{"phase": string(state.Phase)}
	if state.ProgressBytes > 0 {
		metadata["bytes_downloaded"] = state.ProgressBytes
	}
	if state.ProgressTotal > 0 {
		metadata["bytes_total"] = state.ProgressTotal
	}
	if state.ReclaimedBytes > 0 {
		metadata["reclaimed_bytes"] = state.ReclaimedBytes
	}
	return OperationData{OperationID: state.ID, Operation: state.Operation, Component: state.Component, Status: status,
		ErrorCode: state.ErrorCode, ErrorMessage: state.ErrorMessage, Releases: state.Releases,
		CreatedAt: state.CreatedAt, StartedAt: state.StartedAt, FinishedAt: state.FinishedAt, Metadata: metadata}
}

func (server *Server) dataResponse(id string, value any) Response {
	data, err := json.Marshal(value)
	if err != nil || len(data) > MaxResponseSize {
		return errorResponse(id, "response_failed", "response could not be encoded")
	}
	return Response{Version: ProtocolVersion, ID: id, Data: data}
}

func errorResponse(id, code, message string) Response {
	return Response{Version: ProtocolVersion, ID: id, Error: &ProtocolError{Code: code, Message: message}}
}

func (server *Server) statusData() StatusData {
	data := StatusData{Available: true, UpdaterVersion: BuildVersion, Channel: "stable",
		Host: HostStatus{DisplayName: "Local Nexus host", Location: "This server", ManagementMode: "local", CapabilityVersion: "1",
			Capabilities: []string{}, LastContactAt: time.Now().UTC().Format(time.RFC3339)}}
	if server.config.CurrentOS != "" {
		data.Platform = server.config.CurrentOS + "-" + server.config.CurrentOSVersion + "/" + server.config.CurrentArch
	}
	if state, err := LoadInstallation(server.config.Paths); err == nil {
		data.Installed = true
		data.InstalledReleaseID = state.CurrentRelease
		data.PreviousReleaseID = state.PreviousRelease
		data.Profile = state.Profile
		data.Host.Profile = state.Profile
		if state.Profile == ProfileDatabaseOnly {
			data.Host.Capabilities = []string{"doctor"}
		} else {
			data.Host.Capabilities = []string{"update", "rollback", "cleanup", "components"}
		}
	}
	data.Components = server.componentData()
	if operations, err := server.store.List(20); err == nil {
		data.Operations = make([]OperationData, 0, len(operations))
		for _, operation := range operations {
			data.Operations = append(data.Operations, operationResponseData(operation))
		}
	}
	if preview, err := PreviewCleanup(server.config.Paths, 24*time.Hour); err == nil {
		data.Cleanup = preview
	}
	return data
}

func (server *Server) checkUpdates(ctx context.Context) (UpdateData, error) {
	state, err := LoadInstallation(server.config.Paths)
	if err != nil {
		return UpdateData{}, errors.New("Nexus is not installed")
	}
	if state.Profile == ProfileDatabaseOnly {
		return UpdateData{}, errors.New("database-only hosts do not have a local Nexus application release to update")
	}
	releases, err := server.releases.StableReleases(ctx)
	if err != nil {
		return UpdateData{}, err
	}
	updates, err := UpdatesAfter(releases, state.CurrentRelease)
	if err != nil {
		return UpdateData{}, err
	}
	data := UpdateData{InstalledRelease: state.CurrentRelease, Releases: updates, UpdateAvailable: len(updates) > 0}
	if len(updates) > 0 {
		latest := updates[len(updates)-1]
		data.LatestRelease = latest.Tag
		data.ReleaseNotes = latest.Notes
		data.PublishedAt = latest.PublishedAt
	}
	return data, nil
}

func (server *Server) componentData() []ComponentStatus {
	state, _ := LoadInstallation(server.config.Paths)
	canInstallOptional := state.Profile != ProfileDatabaseOnly && state.Profile != ProfileSubsOnly && state.CurrentRelease != ""
	statuses := make([]ComponentStatus, 0, 3)
	for _, component := range []Component{ComponentCore, ComponentSubs, ComponentDiscord} {
		record := state.Components[component]
		configurationReady := componentConfigurationReady(server.config.Paths, component)
		status := ComponentStatus{ID: component, Label: componentLabel(component), Required: component == ComponentCore || (component == ComponentSubs && (state.Profile == ProfileFull || state.Profile == ProfileAppWebSubsRemoteDB)),
			Installed: record.Installed, Enabled: record.Enabled, Version: record.CurrentRelease, ReleaseID: record.CurrentRelease,
			RuntimeState: "not_installed", HealthState: "unknown", ConfigurationState: "not_configured", HostLocation: "This server", AvailableActions: []string{}}
		if record.Installed {
			if configurationReady {
				status.ConfigurationState = "ready"
			} else {
				status.ConfigurationState = "incomplete"
			}
			if record.Enabled {
				status.RuntimeState, status.HealthState, status.Reachable = server.componentRuntimeStatus(component, record.CurrentRelease)
				if !configurationReady && status.HealthState == "healthy" {
					status.HealthState = "degraded"
				}
				status.AvailableActions = []string{"restart", "disable"}
			} else {
				status.RuntimeState = "disabled"
				status.AvailableActions = []string{"enable"}
			}
		} else if component != ComponentCore && canInstallOptional {
			status.AvailableActions = []string{"install"}
		}
		if component == ComponentCore && record.Installed {
			status.AvailableActions = []string{"restart"}
		}
		statuses = append(statuses, status)
	}
	return statuses
}

func componentConfigurationReady(paths Paths, component Component) bool {
	files := []string{}
	switch component {
	case ComponentCore:
		files = []string{filepath.Join(paths.NexusConfigDir, string(component), ".env")}
	case ComponentSubs:
		files = []string{
			filepath.Join(paths.NexusConfigDir, string(component), "environment"),
			filepath.Join(paths.CredentialDir, string(component), "pw-api-token"),
			filepath.Join(paths.CredentialDir, string(component), "nexus-api-token"),
		}
	case ComponentDiscord:
		files = []string{
			filepath.Join(paths.NexusConfigDir, string(component), "environment"),
			filepath.Join(paths.CredentialDir, string(component), "bot-token"),
			filepath.Join(paths.CredentialDir, string(component), "nexus-api-token"),
			filepath.Join(paths.CredentialDir, string(component), "relay-private-key"),
		}
	default:
		return false
	}
	for _, path := range files {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0007 != 0 || info.Size() <= 0 {
			return false
		}
	}
	return true
}

func (server *Server) componentRuntimeStatus(component Component, release string) (string, string, bool) {
	units, err := componentServiceUnits(component)
	if err != nil {
		return "unknown", "unknown", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	active := true
	for _, unit := range units {
		if err := runFixedCommand(ctx, server.config.SystemctlBinary, []string{"is-active", "--quiet", unit}); err != nil {
			active = false
			break
		}
	}
	if active {
		if component != ComponentCore {
			if err := validateComponentHealth(server.config.Paths, component, release, time.Now().UTC()); err != nil {
				return "running", "degraded", true
			}
		}
		return "running", "healthy", true
	}
	if ctx.Err() != nil {
		return "unreachable", "unknown", false
	}
	failedContext, failedCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer failedCancel()
	for _, unit := range units {
		if err := runFixedCommand(failedContext, server.config.SystemctlBinary, []string{"is-failed", "--quiet", unit}); err == nil {
			return "failed", "degraded", false
		}
	}
	return "stopped", "degraded", false
}

func componentLabel(component Component) string {
	switch component {
	case ComponentCore:
		return "Nexus Core"
	case ComponentSubs:
		return "Nexus Subs"
	case ComponentDiscord:
		return "Nexus Discord"
	default:
		return "Nexus component"
	}
}

func (server *Server) doctorData() DoctorData {
	checks := []DoctorCheck{}
	_, installErr := LoadInstallation(server.config.Paths)
	checks = append(checks, DoctorCheck{Name: "installation_state", Passed: installErr == nil, Message: safeCheckMessage(installErr, "Installation state is valid.")})
	var stats syscall.Statfs_t
	diskErr := syscall.Statfs(server.config.Paths.StateDir, &stats)
	free := uint64(0)
	if diskErr == nil {
		free = stats.Bavail * uint64(stats.Bsize)
	}
	checks = append(checks, DoctorCheck{Name: "disk_space", Passed: diskErr == nil && free >= minimumReleaseSpace, Message: fmt.Sprintf("%d bytes available for updates.", free)})
	for _, path := range []string{server.config.Paths.StateDir, server.config.Paths.ConfigDir, server.config.Paths.ReleaseDir} {
		info, err := os.Stat(path)
		checks = append(checks, DoctorCheck{Name: "path_" + filepath.Base(path), Passed: err == nil && info.IsDir(), Message: path})
	}
	healthy := true
	for _, check := range checks {
		healthy = healthy && check.Passed
	}
	return DoctorData{Healthy: healthy, Checks: checks}
}

func safeCheckMessage(err error, success string) string {
	if err == nil {
		return success
	}
	if errors.Is(err, os.ErrNotExist) {
		return "Nexus has not been installed yet."
	}
	return "The installation state could not be validated."
}
