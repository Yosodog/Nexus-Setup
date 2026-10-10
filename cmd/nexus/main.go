package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Yosodog/Nexus-Setup/internal/updater"
)

var stdinReader = bufio.NewReader(os.Stdin)

var componentByName = map[string]updater.Component{"core": updater.ComponentCore, "subs": updater.ComponentSubs, "discord": updater.ComponentDiscord}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "nexus:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) == 0 {
		help, _ := commandHelp("")
		fmt.Fprint(os.Stderr, help)
		return errors.New("a command is required")
	}
	if arguments[0] == "help" || arguments[0] == "--help" || arguments[0] == "-h" {
		topic := ""
		if arguments[0] == "help" && len(arguments) == 2 {
			topic = arguments[1]
		} else if len(arguments) != 1 {
			return errors.New("usage: nexus help [command]")
		}
		text, err := commandHelp(topic)
		if err != nil {
			return err
		}
		fmt.Print(text)
		return nil
	}
	if len(arguments) == 2 && (arguments[1] == "--help" || arguments[1] == "-h") {
		text, err := commandHelp(arguments[0])
		if err != nil {
			return err
		}
		fmt.Print(text)
		return nil
	}
	if arguments[0] == "internal" {
		return runInternal(arguments[1:])
	}
	if arguments[0] == "version" {
		return printVersion(arguments[1:])
	}
	if arguments[0] == "install" {
		return runInstall(arguments[1:])
	}
	jsonOutput := takeFlag(&arguments, "--json")
	yes := takeFlag(&arguments, "--yes")
	client, err := updater.NewClient(updater.DefaultPaths().SocketPath)
	if err != nil {
		return err
	}
	if len(arguments) == 1 && arguments[0] == "channel" {
		return printChannel(client, jsonOutput)
	}
	request, err := commandRequest(arguments)
	if err != nil {
		return err
	}
	if updaterOperationMutates(request.Operation) && !yes {
		if request.Operation == updater.OperationUpdate {
			check, checkErr := call(client, updater.OperationCheckUpdates, "", nil)
			if checkErr != nil {
				return checkErr
			}
			updateAvailable, err := printUpdateSummary(check)
			if err != nil {
				return err
			}
			if !updateAvailable {
				return nil
			}
		}
		if request.Operation == updater.OperationSetChannel && request.Channel == updater.ChannelBeta {
			fmt.Println("The beta channel installs prereleases. They get less testing than stable releases.")
		}
		if request.Operation == updater.OperationCleanup {
			status, statusErr := call(client, updater.OperationGetStatus, "", nil)
			if statusErr != nil {
				return statusErr
			}
			if err := printCleanupSummary(status); err != nil {
				return err
			}
		}
		confirmed, confirmErr := confirm("Continue? [y/N]: ")
		if confirmErr != nil {
			return confirmErr
		}
		if !confirmed {
			fmt.Println("No changes were made.")
			return nil
		}
	}
	response, err := client.Call(context.Background(), request)
	if err != nil {
		return err
	}
	if response.Accepted && updaterOperationMutates(request.Operation) && !jsonOutput {
		return waitForOperation(client, response.OperationID)
	}
	return printResponse(response, jsonOutput)
}

func commandHelp(topic string) (string, error) {
	switch topic {
	case "":
		return `Nexus deployment and update manager

Usage:
  nexus <command> [options]

Commands:
  install                         Install or adopt Nexus
  update [--check] [--yes]        Check for or install updates
  status [--json]                 Show the installed release and services
  doctor [--json]                 Run read-only host diagnostics
  rollback [component] [--yes]    Restore previous component releases
  cleanup [--yes]                 Remove old managed releases
  component list [--json]         List Core, Subs, and Discord
  component install <name>        Install Subs or Discord
  component enable <name>         Enable Subs or Discord
  component disable <name>        Disable Subs or Discord
  component restart <name>        Restart Core, Subs, or Discord
  channel [stable|beta]           Show or change the release channel
  operation show <uuid> [--json]  Show durable operation progress
  version [--json]                Print the nexus CLI version
  help [command]                  Show help

Mutation commands accept --yes for intentional non-interactive use.
`, nil
	case "install":
		return "Usage: nexus install [--channel stable|beta] [--yes]\n\nGuided installation or adoption of a standard legacy Nexus deployment.\nThe channel defaults to stable; beta also installs prereleases.\n", nil
	case "update":
		return "Usage: nexus update [--check] [--yes]\n\nChecks each component's official GitHub releases on this host's channel and\ninstalls the newest releases that are compatible with each other.\n", nil
	case "status":
		return "Usage: nexus status [--json]\n", nil
	case "doctor":
		return "Usage: nexus doctor [--json]\n", nil
	case "rollback":
		return "Usage: nexus rollback [core|subs|discord] [--yes]\n\nWithout a component, restores the previous release of every component the last\nupdate changed. With a component, restores only that component's previous\nrelease. Database migrations are not reversed.\n", nil
	case "cleanup":
		return "Usage: nexus cleanup [--yes]\n\nKeeps current and previous releases and removes older managed deployment files.\n", nil
	case "component":
		return `Usage:
  nexus component list [--json]
  nexus component install <subs|discord> [--yes]
  nexus component enable <subs|discord> [--yes]
  nexus component disable <subs|discord> [--yes]
  nexus component restart <core|subs|discord> [--yes]
`, nil
	case "operation":
		return "Usage: nexus operation show <operation-id> [--json]\n", nil
	case "channel":
		return "Usage: nexus channel [stable|beta]\n\nWithout an argument, prints the release channel. stable follows only stable\nreleases; beta also follows prereleases such as v1.2.0-beta.1. Switching back\nto stable never downgrades: installed prereleases stay until stable releases\novertake them. The change applies to the next nexus update.\n", nil
	case "version":
		return "Usage: nexus version [--json]\n", nil
	default:
		return "", fmt.Errorf("unknown help topic %q", topic)
	}
}

func waitForOperation(client updater.Client, operationID string) error {
	fmt.Printf("Operation started: %s\n", operationID)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	lastStatus := ""
	connectionFailures := 0
	for {
		request, err := requestFor(updater.OperationGetOperation)
		if err != nil {
			return err
		}
		request.OperationID = operationID
		response, err := client.Call(ctx, request)
		if err != nil {
			connectionFailures++
			if connectionFailures == 1 {
				fmt.Println("Nexus is restarting; reconnecting to the updater...")
			}
			if connectionFailures >= 60 {
				return fmt.Errorf("operation is still running but status is unavailable; inspect it with nexus operation show %s", operationID)
			}
		} else {
			connectionFailures = 0
			var operation updater.OperationData
			if err := updater.DecodeResponseData(response, &operation); err != nil {
				return err
			}
			if operation.Status != lastStatus {
				fmt.Printf("Status: %s\n", strings.ReplaceAll(operation.Status, "_", " "))
				lastStatus = operation.Status
			}
			switch operation.Status {
			case "completed":
				if reclaimed, ok := operation.Metadata["reclaimed_bytes"].(float64); ok && reclaimed > 0 {
					fmt.Printf("Reclaimed %.0f bytes.\n", reclaimed)
				}
				return nil
			case "failed", "rolled_back", "recovery_required":
				message := operation.ErrorMessage
				if message == "" {
					message = "the operation did not complete"
				}
				return fmt.Errorf("%s (%s); inspect operation %s", message, operation.Status, operationID)
			}
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("stopped waiting; operation %s continues in the background", operationID)
		case <-timer.C:
		}
	}
}

func commandRequest(arguments []string) (updater.Request, error) {
	if len(arguments) == 0 {
		return updater.Request{}, errors.New("a command is required")
	}
	switch arguments[0] {
	case "status":
		return requestFor(updater.OperationGetStatus)
	case "doctor":
		return requestFor(updater.OperationDoctor)
	case "update":
		if len(arguments) == 2 && arguments[1] == "--check" {
			return requestFor(updater.OperationCheckUpdates)
		}
		if len(arguments) != 1 {
			return updater.Request{}, errors.New("usage: nexus update [--check] [--yes]")
		}
		return requestFor(updater.OperationUpdate)
	case "rollback":
		if len(arguments) > 2 {
			return updater.Request{}, errors.New("usage: nexus rollback [core|subs|discord] [--yes]")
		}
		request, err := requestFor(updater.OperationRollback)
		if err != nil || len(arguments) == 1 {
			return request, err
		}
		component, exists := componentByName[arguments[1]]
		if !exists {
			return updater.Request{}, errors.New("component must be core, subs, or discord")
		}
		request.Component = component
		return request, nil
	case "cleanup":
		if len(arguments) != 1 {
			return updater.Request{}, errors.New("usage: nexus cleanup [--yes]")
		}
		return requestFor(updater.OperationCleanup)
	case "component":
		return componentRequest(arguments[1:])
	case "operation":
		return operationRequest(arguments[1:])
	case "channel":
		if len(arguments) != 2 {
			return updater.Request{}, errors.New("usage: nexus channel [stable|beta]")
		}
		channel := updater.Channel(arguments[1])
		if channel != updater.ChannelStable && channel != updater.ChannelBeta {
			return updater.Request{}, errors.New("channel must be stable or beta")
		}
		request, err := requestFor(updater.OperationSetChannel)
		request.Channel = channel
		return request, err
	default:
		return updater.Request{}, fmt.Errorf("unknown command %q", arguments[0])
	}
}

func componentRequest(arguments []string) (updater.Request, error) {
	if len(arguments) == 1 && arguments[0] == "list" {
		return requestFor(updater.OperationListComponents)
	}
	if len(arguments) != 2 {
		return updater.Request{}, errors.New("usage: nexus component <install|enable|disable|restart> <subs|discord|core>")
	}
	component, exists := componentByName[arguments[1]]
	if !exists {
		return updater.Request{}, errors.New("component must be core, subs, or discord")
	}
	operationByName := map[string]updater.Operation{
		"install": updater.OperationInstallComponent, "enable": updater.OperationEnableComponent,
		"disable": updater.OperationDisableComponent, "restart": updater.OperationRestartComponent,
	}
	operation, exists := operationByName[arguments[0]]
	if !exists {
		return updater.Request{}, errors.New("component action must be install, enable, disable, or restart")
	}
	request, err := requestFor(operation)
	if err != nil {
		return updater.Request{}, err
	}
	request.Component = component
	if operation == updater.OperationInstallComponent && component == updater.ComponentDiscord {
		botToken, err := readSecret("Discord bot token: ")
		if err != nil {
			return updater.Request{}, err
		}
		clientID, err := readLine("Discord application ID: ")
		if err != nil {
			return updater.Request{}, err
		}
		guildID, err := readLine("Discord guild ID: ")
		if err != nil {
			return updater.Request{}, err
		}
		request.Configuration = map[string]string{"bot_token": botToken, "client_id": clientID, "guild_id": guildID}
	}
	return request, nil
}

func operationRequest(arguments []string) (updater.Request, error) {
	if len(arguments) != 2 || arguments[0] != "show" {
		return updater.Request{}, errors.New("usage: nexus operation show <operation-id>")
	}
	request, err := requestFor(updater.OperationGetOperation)
	if err != nil {
		return updater.Request{}, err
	}
	request.OperationID = arguments[1]
	return request, nil
}

func requestFor(operation updater.Operation) (updater.Request, error) {
	id, err := updater.NewOperationID()
	if err != nil {
		return updater.Request{}, err
	}
	request := updater.Request{Version: updater.ProtocolVersion, ID: id, Operation: operation, Source: "cli"}
	if updaterOperationMutates(operation) {
		request.OperationID = id
	}
	return request, nil
}

func updaterOperationMutates(operation updater.Operation) bool {
	switch operation {
	case updater.OperationUpdate, updater.OperationRollback, updater.OperationCleanup,
		updater.OperationInstallComponent, updater.OperationEnableComponent,
		updater.OperationDisableComponent, updater.OperationRestartComponent,
		updater.OperationSetChannel:
		return true
	default:
		return false
	}
}

func call(client updater.Client, operation updater.Operation, component updater.Component, configuration map[string]string) (updater.Response, error) {
	request, err := requestFor(operation)
	if err != nil {
		return updater.Response{}, err
	}
	request.Component = component
	request.Configuration = configuration
	return client.Call(context.Background(), request)
}

func printResponse(response updater.Response, jsonOutput bool) error {
	if jsonOutput {
		fmt.Println(string(response.Data))
		return nil
	}
	if response.Accepted {
		fmt.Printf("Operation started: %s\n", response.OperationID)
		fmt.Printf("Track it with: nexus operation show %s\n", response.OperationID)
		return nil
	}
	if len(response.Data) == 0 {
		fmt.Println("Request completed.")
		return nil
	}
	var value any
	if err := json.Unmarshal(response.Data, &value); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	return nil
}

func printUpdateSummary(response updater.Response) (bool, error) {
	var data updater.UpdateData
	if err := updater.DecodeResponseData(response, &data); err != nil {
		return false, err
	}
	fmt.Printf("Channel: %s\n", data.Channel)
	for _, change := range data.Components {
		fmt.Printf("  %-14s %s -> %s\n", change.Component, change.From, change.To)
	}
	if data.UpdaterRelease != nil {
		fmt.Printf("  %-14s %s -> %s\n", updater.ComponentUpdater, data.UpdaterRelease.From, data.UpdaterRelease.To)
	}
	needsUpdater := false
	for _, held := range data.HeldBack {
		fmt.Printf("  %-14s %s is held back: %s\n", held.Component, held.Release, held.Reason)
		needsUpdater = needsUpdater || held.NeedsUpdater
	}
	if needsUpdater && data.UpdaterRelease != nil {
		fmt.Println("Run nexus update again after this update finishes to install the held-back releases.")
	}
	if !data.UpdateAvailable {
		fmt.Println("Nexus is up to date.")
		return false, nil
	}
	for _, change := range data.Components {
		if change.Notes != "" {
			fmt.Printf("\n%s %s\n%s\n", change.Component, change.To, change.Notes)
		}
	}
	return true, nil
}

func printChannel(client updater.Client, jsonOutput bool) error {
	response, err := call(client, updater.OperationGetStatus, "", nil)
	if err != nil {
		return err
	}
	var data updater.StatusData
	if err := updater.DecodeResponseData(response, &data); err != nil {
		return err
	}
	if jsonOutput {
		encoded, _ := json.Marshal(map[string]string{"channel": data.Channel})
		fmt.Println(string(encoded))
		return nil
	}
	fmt.Println(data.Channel)
	return nil
}

func printCleanupSummary(response updater.Response) error {
	var data updater.StatusData
	if err := updater.DecodeResponseData(response, &data); err != nil {
		return err
	}
	fmt.Printf("Cleanup can reclaim %d bytes from %d managed entries.\n", data.Cleanup.ReclaimableBytes, len(data.Cleanup.Paths))
	for _, entry := range data.Cleanup.Paths {
		fmt.Println(" -", entry)
	}
	return nil
}

func takeFlag(arguments *[]string, flagName string) bool {
	filtered := (*arguments)[:0]
	found := false
	for _, argument := range *arguments {
		if argument == flagName {
			found = true
			continue
		}
		filtered = append(filtered, argument)
	}
	*arguments = filtered
	return found
}

func confirm(prompt string) (bool, error) {
	value, err := readLine(prompt)
	if err != nil {
		return false, err
	}
	value = strings.ToLower(strings.TrimSpace(value))
	return value == "y" || value == "yes", nil
}

func readLine(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	value, err := stdinReader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) && value == "" {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

func readSecret(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	terminal := false
	if info, err := os.Stdin.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
		terminal = exec.Command("/usr/bin/stty", "-echo").Run() == nil
	}
	if terminal {
		defer func() {
			_ = exec.Command("/usr/bin/stty", "echo").Run()
			fmt.Fprintln(os.Stderr)
		}()
	}
	value, err := stdinReader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) && value == "" {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

func printVersion(arguments []string) error {
	jsonOutput := len(arguments) == 1 && arguments[0] == "--json"
	if len(arguments) > 1 || (len(arguments) == 1 && !jsonOutput) {
		return errors.New("usage: nexus version [--json]")
	}
	if jsonOutput {
		encoded, _ := json.Marshal(map[string]string{"version": updater.BuildVersion})
		fmt.Println(string(encoded))
	} else {
		fmt.Println(updater.BuildVersion)
	}
	return nil
}

func runInternal(arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("an internal command is required")
	}
	switch arguments[0] {
	case "coordinator":
		return runCoordinator()
	case "worker":
		if len(arguments) != 2 {
			return errors.New("worker operation id is required")
		}
		config, err := updater.LoadInstalledConfig()
		if err != nil {
			return err
		}
		store, err := updater.NewOperationStore(config.Paths)
		if err != nil {
			return err
		}
		locks, err := updater.NewLockSet(config.Paths)
		if err != nil {
			return err
		}
		return updater.RunWorker(context.Background(), config, store, locks, arguments[1], nil)
	case "configure-host":
		return updater.ConfigureHost(context.Background())
	default:
		return errors.New("unknown internal command")
	}
}

func runCoordinator() error {
	config, err := updater.LoadInstalledConfig()
	if err != nil {
		return err
	}
	server, err := updater.NewServer(config, nil)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if listener, activated, err := systemdListener(); err != nil {
		return err
	} else if activated {
		return server.Serve(ctx, listener)
	}
	return server.ListenAndServe(ctx)
}

func systemdListener() (net.Listener, bool, error) {
	pidValue := os.Getenv("LISTEN_PID")
	fdCount := os.Getenv("LISTEN_FDS")
	if pidValue == "" && fdCount == "" {
		return nil, false, nil
	}
	pid, err := strconv.Atoi(pidValue)
	if err != nil || pid != os.Getpid() {
		return nil, false, errors.New("invalid systemd LISTEN_PID")
	}
	count, err := strconv.Atoi(fdCount)
	if err != nil || count != 1 {
		return nil, false, errors.New("exactly one systemd listener is required")
	}
	file := os.NewFile(uintptr(3), "nexus-updater-control.sock")
	if file == nil {
		return nil, false, errors.New("systemd listener descriptor is unavailable")
	}
	listener, err := net.FileListener(file)
	_ = file.Close()
	if err != nil {
		return nil, false, err
	}
	if _, ok := listener.(*net.UnixListener); !ok {
		_ = listener.Close()
		return nil, false, errors.New("systemd listener is not a Unix socket")
	}
	return listener, true, nil
}

func runInstall(arguments []string) error {
	flags := flag.NewFlagSet("install", flag.ContinueOnError)
	yes := flags.Bool("yes", false, "accept the installation summary")
	channel := flags.String("channel", string(updater.ChannelStable), "release channel: stable or beta")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return errors.New("nexus install must be run with sudo")
	}
	assessment, err := updater.AssessLegacyInstallation(context.Background())
	if err != nil {
		return fmt.Errorf("legacy installation assessment failed: %w", err)
	}
	if assessment.Detected {
		componentNames := make([]string, 0, len(assessment.Components))
		for _, component := range assessment.Components {
			componentNames = append(componentNames, string(component)+" "+assessment.Releases[component])
		}
		fmt.Fprintf(os.Stderr, "Detected a standard legacy Nexus installation (%s).\n", strings.Join(componentNames, ", "))
		fmt.Fprintln(os.Stderr, "The managed release will be installed alongside it and the original checkout will be preserved.")
		if !*yes {
			confirmed, confirmErr := confirm("Adopt this installation now? [y/N]: ")
			if confirmErr != nil || !confirmed {
				return confirmErr
			}
		}
		return updater.AdoptLegacyInstallation(context.Background())
	}
	if *channel != string(updater.ChannelStable) && *channel != string(updater.ChannelBeta) {
		return errors.New("--channel must be stable or beta")
	}
	options, err := promptInstallOptions()
	if err != nil {
		return err
	}
	options.Channel = updater.Channel(*channel)
	if !*yes {
		fmt.Fprintf(os.Stderr, "\nInstall profile: %s\nRelease channel: %s\nDomain: %s\n", options.Profile, options.Channel, options.Domain)
		confirmed, err := confirm("Install Nexus now? [y/N]: ")
		if err != nil || !confirmed {
			return err
		}
	}
	return updater.InstallFresh(context.Background(), options)
}

func promptInstallOptions() (updater.InstallOptions, error) {
	fmt.Fprintln(os.Stderr, "Choose a deployment profile:")
	fmt.Fprintln(os.Stderr, "  1. Everything on this server (recommended)")
	fmt.Fprintln(os.Stderr, "  2. Application and Subs with an existing remote database")
	fmt.Fprintln(os.Stderr, "  3. Web application with an existing remote database")
	fmt.Fprintln(os.Stderr, "  4. Database server only")
	fmt.Fprintln(os.Stderr, "  5. Subs worker only")
	profiles := map[string]updater.Profile{"1": updater.ProfileFull, "2": updater.ProfileAppWebSubsRemoteDB, "3": updater.ProfileWebOnly, "4": updater.ProfileDatabaseOnly, "5": updater.ProfileSubsOnly}
	choice, err := ask("Profile [1]: ", false, func(value string) error {
		if value == "" {
			return nil
		}
		if _, exists := profiles[value]; !exists {
			return errors.New("enter a number from 1 to 5")
		}
		return nil
	})
	if err != nil {
		return updater.InstallOptions{}, err
	}
	if choice == "" {
		choice = "1"
	}
	profile := profiles[choice]
	options := updater.InstallOptions{Profile: profile}
	if profile == updater.ProfileDatabaseOnly {
		if options.DatabaseListenAddress, err = ask("This database server's private IPv4 address: ", false, updater.ValidatePrivateIPv4); err != nil {
			return options, err
		}
		if options.ApplicationHost, err = ask("Application server's private IPv4 address: ", false, func(value string) error {
			if value == options.DatabaseListenAddress {
				return errors.New("the application server must be a different host")
			}
			return updater.ValidatePrivateIPv4(value)
		}); err != nil {
			return options, err
		}
	}
	if profile != updater.ProfileDatabaseOnly && profile != updater.ProfileSubsOnly {
		if options.Domain, err = ask("Domain: ", false, updater.ValidateDomain); err != nil {
			return options, err
		}
		if options.AdminEmail, err = ask("Administrator email: ", false, updater.ValidateEmail); err != nil {
			return options, err
		}
		if options.AdminPassword, err = askNewPassword(); err != nil {
			return options, err
		}
		if options.AdminNationID, err = ask("Administrator nation ID: ", false, updater.ValidateNumericID); err != nil {
			return options, err
		}
		if options.AllianceID, err = ask("Alliance ID: ", false, updater.ValidateNumericID); err != nil {
			return options, err
		}
		if options.PWAPIKey, err = ask("Politics & War API key: ", true, updater.ValidateRequired); err != nil {
			return options, err
		}
		if options.PWMutationKey, err = ask("Politics & War mutation key: ", true, updater.ValidateRequired); err != nil {
			return options, err
		}
	}
	if profile == updater.ProfileAppWebSubsRemoteDB || profile == updater.ProfileWebOnly {
		if options.DatabaseHost, err = ask("Remote database host: ", false, updater.ValidateRequired); err != nil {
			return options, err
		}
		if options.DatabaseName, err = ask("Remote database name: ", false, updater.ValidateRequired); err != nil {
			return options, err
		}
		if options.DatabaseUser, err = ask("Remote database user: ", false, updater.ValidateRequired); err != nil {
			return options, err
		}
		if options.DatabasePassword, err = ask("Remote database password: ", true, updater.ValidateRequired); err != nil {
			return options, err
		}
	}
	if profile == updater.ProfileSubsOnly {
		if options.CoreURL, err = ask("Nexus Subs API URL (https://your-nexus.example/api/v1/subs): ", false, updater.ValidateHTTPSURL); err != nil {
			return options, err
		}
		if options.CoreToken, err = ask("Nexus Core token: ", true, updater.ValidateRequired); err != nil {
			return options, err
		}
		if options.PWAPIKey, err = ask("Politics & War API key: ", true, updater.ValidateRequired); err != nil {
			return options, err
		}
	}
	return options, nil
}

const maxAnswerAttempts = 5

// ask prompts until the answer passes validate, explaining each rejection.
func ask(prompt string, secret bool, validate func(string) error) (string, error) {
	for attempt := 1; ; attempt++ {
		var value string
		var err error
		if secret {
			value, err = readSecret(prompt)
		} else {
			value, err = readLine(prompt)
		}
		if err != nil {
			return "", err
		}
		validationErr := validate(value)
		if validationErr == nil {
			return value, nil
		}
		if attempt == maxAnswerAttempts {
			return "", fmt.Errorf("%s %w", strings.TrimSpace(prompt), validationErr)
		}
		fmt.Fprintf(os.Stderr, "  %s. Try again.\n", capitalize(validationErr.Error()))
	}
}

func askNewPassword() (string, error) {
	for attempt := 1; ; attempt++ {
		password, err := ask("Administrator password (12+ characters): ", true, updater.ValidateAdminPassword)
		if err != nil {
			return "", err
		}
		confirmation, err := readSecret("Confirm administrator password: ")
		if err != nil {
			return "", err
		}
		if confirmation == password {
			return password, nil
		}
		if attempt == maxAnswerAttempts {
			return "", errors.New("the administrator passwords did not match")
		}
		fmt.Fprintln(os.Stderr, "  The passwords did not match. Try again.")
	}
}

func capitalize(value string) string {
	if value == "" {
		return value
	}
	return strings.ToUpper(value[:1]) + value[1:]
}
