//go:build windows

package tokenfile

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func validatePrivateDirectory(path string, _ os.FileInfo) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("inspect token file directory ACL %s: %w", path, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return fmt.Errorf("token file directory %s does not have a private ACL", path)
	}
	user, allowed, err := privateDirectoryTrustees()
	if err != nil {
		return fmt.Errorf("resolve token file directory trustees: %w", err)
	}
	for i := uint16(0); i < dacl.AceCount; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
			return fmt.Errorf("inspect token file directory ACL %s: %w", path, err)
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return fmt.Errorf("token file directory %s has an unsupported ACL entry", path)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)) //nolint:gosec // G103: the ACE stores its SID inline at SidStart; x/sys has no safe accessor.
		trusted := sid.Equals(user)
		for _, candidate := range allowed {
			trusted = trusted || sid.Equals(candidate)
		}
		if !trusted {
			return fmt.Errorf("token file directory %s must be owner-only", path)
		}
	}
	return nil
}

func privateDirectoryTrustees() (*windows.SID, []*windows.SID, error) {
	var token windows.Token
	err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = token.Close() }()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, nil, err
	}
	kinds := []windows.WELL_KNOWN_SID_TYPE{
		windows.WinLocalSystemSid,
		windows.WinBuiltinAdministratorsSid,
		windows.WinCreatorOwnerSid,
		windows.WinCreatorOwnerRightsSid,
	}
	allowed := make([]*windows.SID, 0, len(kinds))
	for _, kind := range kinds {
		sid, sidErr := windows.CreateWellKnownSid(kind)
		if sidErr != nil {
			return nil, nil, sidErr
		}
		allowed = append(allowed, sid)
	}
	return user.User.Sid, allowed, nil
}
