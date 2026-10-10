package updater

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// PreflightSeverity says whether a preflight finding stops the installation.
type PreflightSeverity string

const (
	PreflightPassed  PreflightSeverity = "passed"
	PreflightWarning PreflightSeverity = "warning"
	PreflightFailed  PreflightSeverity = "failed"
)

// PreflightResult is one installation precondition checked before packages
// are installed, so a wrong DNS record or API key is reported in seconds
// instead of at the end of a long installation.
type PreflightResult struct {
	Check    string
	Severity PreflightSeverity
	Message  string
}

// PreflightBlocked reports whether any result must stop the installation.
func PreflightBlocked(results []PreflightResult) bool {
	for _, result := range results {
		if result.Severity == PreflightFailed {
			return true
		}
	}
	return false
}

// preflightEnvironment holds the network operations preflight uses, so tests
// can replace them.
type preflightEnvironment struct {
	lookupIP         func(context.Context, string) ([]net.IP, error)
	localAddresses   func() ([]net.IP, error)
	probeHTTP        func(context.Context, string) error
	pwEndpoint       string
	pwClient         *http.Client
	pwRequestTimeout time.Duration
}

func defaultPreflightEnvironment() preflightEnvironment {
	return preflightEnvironment{
		lookupIP: func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		},
		localAddresses:   interfaceAddresses,
		probeHTTP:        probeOwnHTTP,
		pwEndpoint:       "https://api.politicsandwar.com/graphql",
		pwClient:         &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		pwRequestTimeout: 15 * time.Second,
	}
}

// PreflightInstall checks the domain and Politics & War credentials an
// installation depends on. It changes nothing on the host.
func PreflightInstall(ctx context.Context, options InstallOptions) []PreflightResult {
	return defaultPreflightEnvironment().run(ctx, options)
}

func (environment preflightEnvironment) run(ctx context.Context, options InstallOptions) []PreflightResult {
	results := []PreflightResult{}
	switch options.Profile {
	case ProfileFull, ProfileAppWebSubsRemoteDB, ProfileWebOnly:
		results = append(results, environment.checkDomain(ctx, options.Domain))
		results = append(results, environment.checkPoliticsAndWar(ctx, options.PWAPIKey, options.AdminNationID, options.AllianceID)...)
	case ProfileSubsOnly:
		results = append(results, environment.checkPoliticsAndWar(ctx, options.PWAPIKey, "", "")...)
	}
	return results
}

func (environment preflightEnvironment) checkDomain(ctx context.Context, domain string) PreflightResult {
	const check = "domain"
	lookupContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	addresses, err := environment.lookupIP(lookupContext, domain)
	if err != nil || len(addresses) == 0 {
		return PreflightResult{Check: check, Severity: PreflightFailed, Message: fmt.Sprintf("%s does not resolve yet. Create an A record that points it at this server, wait for it to resolve, and run the installer again.", domain)}
	}
	resolved := make([]string, 0, len(addresses))
	for _, address := range addresses {
		resolved = append(resolved, address.String())
	}
	if local, localErr := environment.localAddresses(); localErr == nil {
		for _, address := range addresses {
			for _, candidate := range local {
				if address.Equal(candidate) {
					return PreflightResult{Check: check, Severity: PreflightPassed, Message: fmt.Sprintf("%s points at this server (%s).", domain, address)}
				}
			}
		}
	}
	probeContext, probeCancel := context.WithTimeout(ctx, 10*time.Second)
	defer probeCancel()
	if err := environment.probeHTTP(probeContext, domain); err == nil {
		return PreflightResult{Check: check, Severity: PreflightPassed, Message: fmt.Sprintf("%s reaches this server on port 80.", domain)}
	}
	return PreflightResult{Check: check, Severity: PreflightWarning, Message: fmt.Sprintf("%s resolves to %s, but this server could not reach itself through that address on port 80. The TLS certificate request at the end of installation will fail unless the record points here and inbound port 80 is open.", domain, strings.Join(resolved, ", "))}
}

func interfaceAddresses() ([]net.IP, error) {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addresses))
	for _, address := range addresses {
		if network, ok := address.(*net.IPNet); ok {
			ips = append(ips, network.IP)
		}
	}
	return ips, nil
}

// probeOwnHTTP serves a one-time token on port 80 and fetches it through the
// domain, which is what the Let's Encrypt HTTP challenge will need to do.
func probeOwnHTTP(ctx context.Context, domain string) error {
	listener, err := net.Listen("tcp", ":80")
	if err != nil {
		return err
	}
	var tokenBytes [16]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		_ = listener.Close()
		return err
	}
	token := hex.EncodeToString(tokenBytes[:])
	path := "/.well-known/nexus-preflight/" + token
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != path {
			http.NotFound(writer, request)
			return
		}
		_, _ = io.WriteString(writer, token)
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+domain+path, nil)
	if err != nil {
		return err
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 256))
	if err != nil || response.StatusCode != http.StatusOK || string(body) != token {
		return errors.New("domain did not reach this server")
	}
	return nil
}

// pwID accepts Politics & War identifiers encoded as JSON strings or numbers.
type pwID string

func (identifier *pwID) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		*identifier = pwID(text)
		return nil
	}
	var number json.Number
	if err := json.Unmarshal(data, &number); err != nil {
		return err
	}
	*identifier = pwID(number.String())
	return nil
}

type pwPreflightResponse struct {
	Data struct {
		Me *struct {
			Nation *struct {
				ID         pwID   `json:"id"`
				NationName string `json:"nation_name"`
			} `json:"nation"`
		} `json:"me"`
		Alliances *struct {
			Data []struct {
				ID   pwID   `json:"id"`
				Name string `json:"name"`
			} `json:"data"`
		} `json:"alliances"`
		Nations *struct {
			Data []struct {
				ID         pwID   `json:"id"`
				NationName string `json:"nation_name"`
				AllianceID pwID   `json:"alliance_id"`
			} `json:"data"`
		} `json:"nations"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func (environment preflightEnvironment) checkPoliticsAndWar(ctx context.Context, apiKey, nationID, allianceID string) []PreflightResult {
	const check = "politics_and_war"
	query := "{ me { nation { id nation_name } }"
	if allianceID != "" {
		query += " alliances(id: [" + allianceID + "], first: 1) { data { id name } }"
	}
	if nationID != "" {
		query += " nations(id: [" + nationID + "], first: 1) { data { id nation_name alliance_id } }"
	}
	query += " }"
	for _, identifier := range []string{nationID, allianceID} {
		if identifier != "" && ValidateNumericID(identifier) != nil {
			return []PreflightResult{{Check: check, Severity: PreflightFailed, Message: "nation and alliance IDs must be numeric"}}
		}
	}
	body, _ := json.Marshal(map[string]string{"query": query})
	requestContext, cancel := context.WithTimeout(ctx, environment.pwRequestTimeout)
	defer cancel()
	endpoint := environment.pwEndpoint + "?" + url.Values{"api_key": {apiKey}}.Encode()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return []PreflightResult{{Check: check, Severity: PreflightWarning, Message: "the Politics & War API key could not be checked"}}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "nexus-installer")
	response, err := environment.pwClient.Do(request)
	if err != nil {
		return []PreflightResult{{Check: check, Severity: PreflightWarning, Message: "Politics & War could not be reached, so the API key was not checked. Installation can continue, but Nexus needs that API to work."}}
	}
	defer response.Body.Close()
	var payload pwPreflightResponse
	decodeErr := json.NewDecoder(io.LimitReader(response.Body, 256*1024)).Decode(&payload)
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden || (decodeErr == nil && payload.Data.Me == nil && len(payload.Errors) > 0) {
		return []PreflightResult{{Check: check, Severity: PreflightFailed, Message: "Politics & War rejected the API key. Copy it again from the Account page and rerun the installer."}}
	}
	if decodeErr != nil || response.StatusCode != http.StatusOK || payload.Data.Me == nil {
		return []PreflightResult{{Check: check, Severity: PreflightWarning, Message: "Politics & War returned an unexpected response (HTTP " + strconv.Itoa(response.StatusCode) + "), so the API key was not fully checked."}}
	}
	results := []PreflightResult{}
	owner := "an unknown nation"
	if payload.Data.Me.Nation != nil {
		owner = payload.Data.Me.Nation.NationName
	}
	results = append(results, PreflightResult{Check: check, Severity: PreflightPassed, Message: "The Politics & War API key works (it belongs to " + owner + ")."})
	allianceName := ""
	if allianceID != "" {
		if payload.Data.Alliances == nil || len(payload.Data.Alliances.Data) == 0 {
			results = append(results, PreflightResult{Check: "alliance", Severity: PreflightFailed, Message: "Politics & War has no alliance with ID " + allianceID + "."})
		} else {
			allianceName = payload.Data.Alliances.Data[0].Name
			results = append(results, PreflightResult{Check: "alliance", Severity: PreflightPassed, Message: "Alliance " + allianceID + " is " + allianceName + "."})
		}
	}
	if nationID != "" {
		if payload.Data.Nations == nil || len(payload.Data.Nations.Data) == 0 {
			results = append(results, PreflightResult{Check: "administrator_nation", Severity: PreflightFailed, Message: "Politics & War has no nation with ID " + nationID + "."})
		} else if nation := payload.Data.Nations.Data[0]; allianceID != "" && string(nation.AllianceID) != allianceID {
			results = append(results, PreflightResult{Check: "administrator_nation", Severity: PreflightWarning, Message: "Administrator nation " + nation.NationName + " is not a member of alliance " + allianceID + "."})
		} else {
			results = append(results, PreflightResult{Check: "administrator_nation", Severity: PreflightPassed, Message: "Administrator nation " + nationID + " is " + nation.NationName + "."})
		}
	}
	return results
}
