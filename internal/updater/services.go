package updater

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// SubsDelivery is how Subs hands subscription events to Core.
type SubsDelivery string

const (
	SubsDeliveryHTTP        SubsDelivery = "http"
	SubsDeliveryRedisStream SubsDelivery = "redis-stream"
)

func validSubsDelivery(delivery SubsDelivery) bool {
	switch delivery {
	case "", SubsDeliveryHTTP, SubsDeliveryRedisStream:
		return true
	default:
		return false
	}
}

// Core runs PHP-FPM and several long-lived Artisan processes. Each process is
// its own fixed unit so a caller can never name an arbitrary command.
const (
	coreQueueUnit      = "nexus-core.service"
	coreSyncQueueUnit  = "nexus-core-sync.service"
	corePulseCheckUnit = "nexus-core-pulse-check.service"
	corePulseWorkUnit  = "nexus-core-pulse-work.service"
	coreSubsStreamUnit = "nexus-core-subs-stream.service"
	corePHPFPMUnit     = "nexus-core-php-fpm.service"
)

// hostServices records the host facts that decide which optional Core units
// run: Pulse ingests through Redis only when Core is configured for it, and the
// Subs stream consumer runs only when Subs delivers through a Redis Stream.
type hostServices struct {
	pulseRedis bool
	subsStream bool
}

func hostServicesFor(paths Paths, state InstallationState) hostServices {
	services := hostServices{}
	if !state.Components[ComponentCore].Installed {
		return services
	}
	driver, err := readEnvValue(filepath.Join(paths.NexusConfigDir, string(ComponentCore), ".env"), "PULSE_INGEST_DRIVER")
	services.pulseRedis = err == nil && strings.EqualFold(strings.TrimSpace(driver), "redis")
	services.subsStream = state.SubsDelivery == SubsDeliveryRedisStream
	return services
}

// coreWorkerUnits lists the Artisan units that run for Core on this host. The
// PHP-FPM alias is restarted with them but enabled through PHP's own unit.
func coreWorkerUnits(services hostServices) []string {
	units := []string{coreQueueUnit, coreSyncQueueUnit, corePulseCheckUnit}
	if services.pulseRedis {
		units = append(units, corePulseWorkUnit)
	}
	if services.subsStream {
		units = append(units, coreSubsStreamUnit)
	}
	return units
}

func optionalCoreWorkerUnits() []string {
	return []string{corePulseWorkUnit, coreSubsStreamUnit}
}

// componentEnableUnits lists the units to enable when a component starts.
func componentEnableUnits(component Component, services hostServices) ([]string, error) {
	if component == ComponentCore {
		return coreWorkerUnits(services), nil
	}
	unit, err := componentServiceUnit(component)
	if err != nil {
		return nil, err
	}
	return []string{unit}, nil
}

// requireServiceUnits fails before anything changes when a unit this binary
// expects was never installed, which happens when the updater updated itself
// on a host whose systemd units predate it.
func (engine DeploymentEngine) requireServiceUnits(ctx context.Context, components []Component, services hostServices) error {
	for _, component := range components {
		units, err := componentServiceUnits(component, services)
		if err != nil {
			return err
		}
		for _, unit := range units {
			if err := engine.Executor.Run(ctx, engine.Config.SystemctlBinary, []string{"cat", unit}); err != nil {
				return fmt.Errorf("%s is not installed; rerun the Nexus bootstrap command to install the current service units", unit)
			}
		}
	}
	return nil
}

// reconcileCoreServices enables the Core units this host needs, restarts them
// so changed unit definitions take effect, and stops optional units it no
// longer needs. It runs when bootstrap rewrites the units on a managed host.
func reconcileCoreServices(ctx context.Context, systemctl string, paths Paths) error {
	state, err := LoadInstallation(paths)
	if err != nil {
		return nil
	}
	core := state.Components[ComponentCore]
	if !core.Installed || !core.Enabled {
		return nil
	}
	services := hostServicesFor(paths, state)
	wanted := coreWorkerUnits(services)
	for _, unit := range optionalCoreWorkerUnits() {
		if !containsString(wanted, unit) {
			_ = runFixedCommand(ctx, systemctl, []string{"disable", "--now", unit})
		}
	}
	for _, unit := range wanted {
		if err := runFixedCommand(ctx, systemctl, []string{"enable", unit}); err != nil {
			return err
		}
		if err := runFixedCommand(ctx, systemctl, []string{"restart", unit}); err != nil {
			return err
		}
	}
	return nil
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// coreArtisanUnit renders a long-running Core Artisan process with the same
// sandbox as the scheduler.
func coreArtisanUnit(description, arguments, memoryMax string) string {
	return `[Unit]
Description=` + description + `
After=network-online.target mysql.service redis-server.service

[Service]
Type=simple
User=nexus-core
Group=nexus-core
WorkingDirectory=/opt/nexus/nexus-core/current
ExecStart=/usr/bin/php /opt/nexus/nexus-core/current/artisan ` + arguments + `
Restart=always
RestartSec=5
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/nexus/nexus-core
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
RestrictNamespaces=yes
RestrictSUIDSGID=yes
RestrictRealtime=yes
LockPersonality=yes
SystemCallArchitectures=native
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
CapabilityBoundingSet=
AmbientCapabilities=
MemoryMax=` + memoryMax + `
TasksMax=128
LimitNOFILE=4096
UMask=0027

[Install]
WantedBy=multi-user.target
`
}

func coreServiceUnitFiles() map[string]string {
	return map[string]string{
		"/etc/systemd/system/" + coreQueueUnit:      coreArtisanUnit("Nexus Core queue worker", "queue:work --queue=default --sleep=3 --tries=3 --max-time=3600", "512M"),
		"/etc/systemd/system/" + coreSyncQueueUnit:  coreArtisanUnit("Nexus Core sync queue worker", "queue:work --queue=sync --sleep=3 --tries=3 --max-time=3600", "1G"),
		"/etc/systemd/system/" + corePulseCheckUnit: coreArtisanUnit("Nexus Core Pulse server checks", "pulse:check", "256M"),
		"/etc/systemd/system/" + corePulseWorkUnit:  coreArtisanUnit("Nexus Core Pulse Redis ingest", "pulse:work", "256M"),
		"/etc/systemd/system/" + coreSubsStreamUnit: coreArtisanUnit("Nexus Core Subs stream consumer", "subs:consume-stream", "512M"),
	}
}

// subsRedisStream is an adopted Redis Stream delivery from Subs to Core. Core
// keeps its own SUBS_REDIS_* settings in its .env.
type subsRedisStream struct {
	URL        string
	Stream     string
	HMACSecret string
}

const defaultSubsRedisStream = "nexus:subscriptions:v1"

func (stream subsRedisStream) validate() error {
	if stream.URL == "" || stream.Stream == "" {
		return errors.New("Subs Redis Stream delivery needs SUBS_REDIS_URL and SUBS_REDIS_STREAM")
	}
	parsed, err := url.Parse(stream.URL)
	if err != nil || (parsed.Scheme != "redis" && parsed.Scheme != "rediss" && parsed.Scheme != "unix") {
		return errors.New("SUBS_REDIS_URL must be a redis://, rediss://, or unix:// URL")
	}
	if len(stream.HMACSecret) < 32 {
		return errors.New("SUBS_REDIS_HMAC_SECRET must be at least 32 characters")
	}
	return nil
}

// applySubsDelivery sets Subs' delivery keys, writing the stream's HMAC secret
// as a credential file rather than into the environment.
func (engine DeploymentEngine) applySubsDelivery(environment map[string]string, stream *subsRedisStream) error {
	if stream == nil {
		environment["DELIVERY_DRIVER"] = string(SubsDeliveryHTTP)
		return nil
	}
	if err := stream.validate(); err != nil {
		return err
	}
	secretPath, err := engine.writeCredential(ComponentSubs, "redis-hmac-secret", stream.HMACSecret)
	if err != nil {
		return err
	}
	environment["DELIVERY_DRIVER"] = string(SubsDeliveryRedisStream)
	environment["SUBS_REDIS_URL"] = stream.URL
	environment["SUBS_REDIS_STREAM"] = stream.Stream
	environment["SUBS_REDIS_HMAC_SECRET_FILE"] = secretPath
	return nil
}
