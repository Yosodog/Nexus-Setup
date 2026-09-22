package updater

import "testing"

func TestDatabaseOnlyRequiresPrivateCrossHostAddresses(t *testing.T) {
	for _, options := range []InstallOptions{
		{Profile: ProfileDatabaseOnly},
		{Profile: ProfileDatabaseOnly, DatabaseListenAddress: "127.0.0.1", ApplicationHost: "10.0.0.8"},
		{Profile: ProfileDatabaseOnly, DatabaseListenAddress: "10.0.0.4", ApplicationHost: "8.8.8.8"},
	} {
		if err := validateInstallOptions(options); err == nil {
			t.Fatalf("unsafe database-only profile was accepted: %+v", options)
		}
	}
	if err := validateInstallOptions(InstallOptions{
		Profile: ProfileDatabaseOnly, DatabaseListenAddress: "10.0.0.4", ApplicationHost: "10.0.0.8",
	}); err != nil {
		t.Fatalf("private database-only profile was rejected: %v", err)
	}
}
