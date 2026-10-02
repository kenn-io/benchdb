//go:build windows

package serverapp

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestBootstrapPersistsCredentialsWithCurrentUserOnlyACLs(t *testing.T) {
	dir := bootstrapEnv(t)
	require.NoError(t, os.Mkdir(dir, 0700))
	setTestACL(t, dir, true, "S-1-1-0")
	t.Setenv("BENCHDB_OIDC_ISSUER_URL", "https://issuer.example")
	t.Setenv("BENCHDB_OIDC_CLIENT_ID", "client")
	t.Setenv("BENCHDB_OIDC_CLIENT_SECRET", "secret")
	t.Setenv("BENCHDB_INTENDED_BASE_URL", "https://benchdb.example")

	cfg, err := loadConfig()
	require.NoError(t, err)
	require.NotEmpty(t, cfg.sessionSecret)

	assertCurrentUserOnlyACL(t, dir, true)
	assertCurrentUserOnlyACL(t, filepath.Join(dir, "api-token"), false)
	assertCurrentUserOnlyACL(t, filepath.Join(dir, "session-secret"), false)
}

func TestBootstrapTightensACLsOnExistingCredentials(t *testing.T) {
	dir := bootstrapEnv(t)
	require.NoError(t, os.Mkdir(dir, 0700))
	setTestACL(t, dir, true, "S-1-1-0")

	const token = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	credentialPath := filepath.Join(dir, "api-token")
	require.NoError(t, os.WriteFile(credentialPath, []byte(token), 0600))
	setTestACL(t, credentialPath, false, "S-1-1-0")

	cfg, err := loadConfig()
	require.NoError(t, err)
	require.Equal(t, token, cfg.apiToken)

	assertCurrentUserOnlyACL(t, dir, true)
	assertCurrentUserOnlyACL(t, credentialPath, false)
}

func setTestACL(t *testing.T, path string, isDir bool, sidString string) {
	t.Helper()

	sid, err := windows.StringToSid(sidString)
	require.NoError(t, err)
	inheritance := uint32(windows.NO_INHERITANCE)
	trusteeType := windows.TRUSTEE_TYPE(windows.TRUSTEE_IS_USER)
	if isDir {
		inheritance = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	if sidString == "S-1-1-0" {
		trusteeType = windows.TRUSTEE_IS_GROUP
	}
	access := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       inheritance,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  trusteeType,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}}
	var pinner runtime.Pinner
	pinner.Pin(sid)
	acl, err := windows.ACLFromEntries(access, nil)
	pinner.Unpin()
	require.NoError(t, err)
	require.NoError(t, windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	))
}

func assertCurrentUserOnlyACL(t *testing.T, path string, isDir bool) {
	t.Helper()

	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	require.NoError(t, err)
	userSID := user.User.Sid

	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION,
	)
	require.NoError(t, err)
	require.NotNil(t, descriptor)
	owner, _, err := descriptor.Owner()
	require.NoError(t, err)
	require.True(t, userSID.Equals(owner), "credential owner must be the process user")

	control, _, err := descriptor.Control()
	require.NoError(t, err)
	require.NotZero(t, control&windows.SE_DACL_PROTECTED, "DACL must block inherited grants")

	dacl, _, err := descriptor.DACL()
	require.NoError(t, err)
	require.EqualValues(t, 1, dacl.AceCount, "only one trustee may have access")
	var ace *windows.ACCESS_ALLOWED_ACE
	require.NoError(t, windows.GetAce(dacl, 0, &ace))
	require.Equal(t, uint8(windows.ACCESS_ALLOWED_ACE_TYPE), ace.Header.AceType)
	require.EqualValues(t, windows.STANDARD_RIGHTS_REQUIRED|windows.SYNCHRONIZE|0x1ff, ace.Mask)
	expectedFlags := uint8(windows.NO_INHERITANCE)
	if isDir {
		expectedFlags = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	}
	require.Equal(t, expectedFlags, ace.Header.AceFlags)

	aclUserSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	require.True(t, userSID.Equals(aclUserSID), "only the process user may have access")
}
