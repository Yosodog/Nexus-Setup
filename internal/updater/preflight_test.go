package updater

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testPreflightEnvironment(t *testing.T, pwResponse string, pwStatus int) preflightEnvironment {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("api_key") != "good-key" {
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(writer, `{"errors":[{"message":"Invalid API key"}]}`)
			return
		}
		writer.WriteHeader(pwStatus)
		_, _ = io.WriteString(writer, pwResponse)
	}))
	t.Cleanup(server.Close)
	return preflightEnvironment{
		lookupIP: func(_ context.Context, host string) ([]net.IP, error) {
			if host == "nexus.example.com" {
				return []net.IP{net.ParseIP("203.0.113.10")}, nil
			}
			return nil, errors.New("no such host")
		},
		localAddresses:   func() ([]net.IP, error) { return []net.IP{net.ParseIP("203.0.113.10")}, nil },
		probeHTTP:        func(context.Context, string) error { return errors.New("unreachable") },
		pwEndpoint:       server.URL,
		pwClient:         server.Client(),
		pwRequestTimeout: 5 * time.Second,
	}
}

const pwMatchingResponse = `{"data":{"me":{"nation":{"id":"123","nation_name":"Yosodia"}},"alliances":{"data":[{"id":"456","name":"Test Alliance"}]},"nations":{"data":[{"id":123,"nation_name":"Yosodia","alliance_id":456}]}}}`

func preflightOptions() InstallOptions {
	return InstallOptions{Profile: ProfileFull, Channel: ChannelStable, Domain: "nexus.example.com", PWAPIKey: "good-key", AdminNationID: "123", AllianceID: "456"}
}

func severities(results []PreflightResult) map[string]PreflightSeverity {
	found := map[string]PreflightSeverity{}
	for _, result := range results {
		found[result.Check] = result.Severity
	}
	return found
}

func TestPreflightPassesForMatchingDomainAndCredentials(t *testing.T) {
	results := testPreflightEnvironment(t, pwMatchingResponse, http.StatusOK).run(context.Background(), preflightOptions())
	for _, result := range results {
		if result.Severity != PreflightPassed {
			t.Fatalf("unexpected finding: %+v", result)
		}
	}
	if len(results) != 4 {
		t.Fatalf("expected domain, key, alliance, and nation checks: %+v", results)
	}
}

func TestPreflightFailsWhenDomainDoesNotResolve(t *testing.T) {
	options := preflightOptions()
	options.Domain = "missing.example.com"
	results := testPreflightEnvironment(t, pwMatchingResponse, http.StatusOK).run(context.Background(), options)
	if severities(results)["domain"] != PreflightFailed || !PreflightBlocked(results) {
		t.Fatalf("unresolvable domain did not block installation: %+v", results)
	}
}

func TestPreflightWarnsWhenDomainPointsElsewhere(t *testing.T) {
	environment := testPreflightEnvironment(t, pwMatchingResponse, http.StatusOK)
	environment.localAddresses = func() ([]net.IP, error) { return []net.IP{net.ParseIP("10.0.0.5")}, nil }
	results := environment.run(context.Background(), preflightOptions())
	if severities(results)["domain"] != PreflightWarning || PreflightBlocked(results) {
		t.Fatalf("expected a domain warning: %+v", results)
	}
	environment.probeHTTP = func(context.Context, string) error { return nil }
	if severities(environment.run(context.Background(), preflightOptions()))["domain"] != PreflightPassed {
		t.Fatal("a reachable domain behind NAT should pass")
	}
}

func TestPreflightRejectsBadAPIKey(t *testing.T) {
	options := preflightOptions()
	options.PWAPIKey = "wrong-key"
	results := testPreflightEnvironment(t, pwMatchingResponse, http.StatusOK).run(context.Background(), options)
	if severities(results)["politics_and_war"] != PreflightFailed {
		t.Fatalf("bad API key was not rejected: %+v", results)
	}
}

func TestPreflightChecksAllianceAndNation(t *testing.T) {
	response := `{"data":{"me":{"nation":{"id":"123","nation_name":"Yosodia"}},"alliances":{"data":[]},"nations":{"data":[{"id":"123","nation_name":"Yosodia","alliance_id":"999"}]}}}`
	results := testPreflightEnvironment(t, response, http.StatusOK).run(context.Background(), preflightOptions())
	found := severities(results)
	if found["alliance"] != PreflightFailed || found["administrator_nation"] != PreflightWarning {
		t.Fatalf("unexpected alliance and nation findings: %+v", results)
	}
}

func TestPreflightOnlyWarnsWhenPoliticsAndWarIsUnavailable(t *testing.T) {
	results := testPreflightEnvironment(t, "upstream error", http.StatusBadGateway).run(context.Background(), preflightOptions())
	if severities(results)["politics_and_war"] != PreflightWarning || PreflightBlocked(results) {
		t.Fatalf("an unavailable API should only warn: %+v", results)
	}
}

func TestPreflightSkipsDatabaseOnlyHosts(t *testing.T) {
	options := InstallOptions{Profile: ProfileDatabaseOnly}
	if results := testPreflightEnvironment(t, pwMatchingResponse, http.StatusOK).run(context.Background(), options); len(results) != 0 {
		t.Fatalf("database-only host ran network checks: %+v", results)
	}
}

func TestPreflightSubsOnlyChecksOnlyTheKey(t *testing.T) {
	options := InstallOptions{Profile: ProfileSubsOnly, PWAPIKey: "good-key"}
	results := testPreflightEnvironment(t, `{"data":{"me":{"nation":{"id":"1","nation_name":"N"}}}}`, http.StatusOK).run(context.Background(), options)
	if len(results) != 1 || results[0].Severity != PreflightPassed || !strings.Contains(results[0].Message, "belongs to N") {
		t.Fatalf("unexpected subs-only findings: %+v", results)
	}
}
